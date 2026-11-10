package snapshot

import (
	"math"
	"time"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/cubepath"
)

// expectedUnits pins the unit of every series the exporter reads. A series with another
// unit is dropped instead of being exposed under a wrong name.
var expectedUnits = map[string]string{
	"cpu_usage":                      "RATIO",
	"memory_usage":                   "BYTES",
	"disk_read_usage":                "BYTES_PER_SECOND",
	"disk_write_usage":               "BYTES_PER_SECOND",
	"network_receive_usage":          "BYTES_PER_SECOND",
	"network_transmit_usage":         "BYTES_PER_SECOND",
	"network_public_receive_usage":   "BYTES_PER_SECOND",
	"network_public_transmit_usage":  "BYTES_PER_SECOND",
	"network_private_receive_usage":  "BYTES_PER_SECOND",
	"network_private_transmit_usage": "BYTES_PER_SECOND",
}

type Point = cubepath.Point

type latest struct {
	ts    time.Time
	value float64
}

// latestPoints returns, per known series, its newest point when that point is not null,
// is finite and is not older than SeriesMaxAge.
func latestPoints(res *cubepath.MetricsResult, now time.Time) map[string]latest {
	out := map[string]latest{}
	if res == nil {
		return out
	}
	for _, s := range res.Series {
		unit, known := expectedUnits[s.Name]
		if !known || s.Unit != unit {
			continue
		}
		// The newest point decides: a null there means the series is absent, never that an
		// older sample still stands for it.
		var best *Point
		for i := range s.Points {
			if p := &s.Points[i]; best == nil || p.TS > best.TS {
				best = p
			}
		}
		if best == nil || best.Value == nil || math.IsNaN(*best.Value) || math.IsInf(*best.Value, 0) {
			continue
		}
		ts := time.Unix(int64(best.TS), 0)
		if now.Sub(ts) > SeriesMaxAge {
			continue
		}
		out[s.Name] = latest{ts: ts, value: *best.Value}
	}
	return out
}

// buildVM turns the series of one server into a VM. It returns nil when no series has a
// fresh point.
func buildVM(d *cubepath.VMData, now time.Time) *VM {
	if d == nil {
		return nil
	}
	m := latestPoints(d.Metrics, now)
	b := latestPoints(d.NetworkBreakdown, now)
	vm := &VM{NetReceive: map[string]float64{}, NetTransmit: map[string]float64{}}
	var newest time.Time
	take := func(l latest, dst **float64) {
		v := l.value
		*dst = &v
		if l.ts.After(newest) {
			newest = l.ts
		}
	}
	if l, ok := m["cpu_usage"]; ok {
		take(l, &vm.CPURatio)
	}
	if l, ok := m["memory_usage"]; ok {
		take(l, &vm.MemoryBytes)
	}
	if l, ok := m["disk_read_usage"]; ok {
		take(l, &vm.DiskRead)
	}
	if l, ok := m["disk_write_usage"]; ok {
		take(l, &vm.DiskWrite)
	}
	net := func(dst map[string]float64, public, private, total string) {
		found := false
		for nic, name := range map[string]string{NICPublic: public, NICPrivate: private} {
			if l, ok := b[name]; ok {
				dst[nic] = l.value
				found = true
				if l.ts.After(newest) {
					newest = l.ts
				}
			}
		}
		if !found {
			if l, ok := m[total]; ok {
				dst[NICTotal] = l.value
				if l.ts.After(newest) {
					newest = l.ts
				}
			}
		}
	}
	net(vm.NetReceive, "network_public_receive_usage", "network_private_receive_usage", "network_receive_usage")
	net(vm.NetTransmit, "network_public_transmit_usage", "network_private_transmit_usage", "network_transmit_usage")
	if newest.IsZero() {
		return nil
	}
	vm.Sample = newest
	return vm
}
