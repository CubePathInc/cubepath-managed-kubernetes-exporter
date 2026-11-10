package collector_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/collector"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/cubepath"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/fakeapi"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/snapshot"
)

var update = flag.Bool("update", false, "rewrite the golden file")

const t0 = 1758100000

// fixtureRegistry runs a real refresh against the fixture fake, with a frozen clock.
func fixtureRegistry(t *testing.T) (*prometheus.Registry, *snapshot.Store, time.Time) {
	t.Helper()
	now := time.Unix(t0, 0)
	fake, err := fakeapi.LoadFixture("../../testdata/fixture", "tok")
	if err != nil {
		t.Fatal(err)
	}
	fake.Now = func() int64 { return t0 }
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	store := snapshot.NewStore()
	client, err := cubepath.New(cubepath.Options{BaseURL: srv.URL, Token: "tok", Observe: store.Stats().ObserveRequest, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ref := snapshot.NewRefresher(client, store, snapshot.Config{Interval: time.Minute}, nil, func() time.Time { return now })
	if res := ref.Refresh(context.Background()); res.Err != nil {
		t.Fatal(res.Err)
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(collector.New(store, func() time.Time { return now }))
	return reg, store, now
}

func exposition(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String()
}

func TestLintAndPedanticGather(t *testing.T) {
	reg, _, _ := fixtureRegistry(t)
	problems, err := testutil.GatherAndLint(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}

func TestGoldenExposition(t *testing.T) {
	reg, _, _ := fixtureRegistry(t)
	got := exposition(t, reg)
	const path = "../../testdata/golden/exposition.txt"
	if *update {
		if err := os.MkdirAll("../../testdata/golden", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden file missing, run with -update: %v", err)
	}
	if got != string(want) {
		t.Fatalf("exposition differs from %s (run go test ./internal/collector -update to refresh it)\n--- got ---\n%s", path, got)
	}
}

func TestNoFakeZeros(t *testing.T) {
	reg, _, _ := fixtureRegistry(t)
	out := exposition(t, reg)
	// Cluster B was not polled: only info and monitored exist for it.
	for _, name := range []string{"cluster_up", "cluster_ready", "cluster_nodes", "cluster_pods_pending", "cluster_apiserver_latency_seconds"} {
		if strings.Contains(out, `cubepath_k8s_`+name+`{cluster="staging"`) || strings.Contains(out, `cubepath_k8s_`+name+`{cluster_uuid="22222222`) {
			t.Errorf("%s must be absent for an unmonitored cluster", name)
		}
	}
	if !strings.Contains(out, `cubepath_k8s_cluster_monitored{cluster="staging",cluster_uuid="22222222-2222-2222-2222-222222222222"} 0`) {
		t.Error("cluster_monitored must be 0 for the unpolled cluster")
	}
	// The node with null health fields has info only.
	for _, name := range []string{"node_ready", "node_unschedulable", "node_condition", "node_allocatable_cpu_cores", "node_usage_cpu_cores"} {
		if strings.Contains(out, "cubepath_k8s_"+name+"{") && strings.Contains(out, `node="staging-default-0"`) {
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "cubepath_k8s_"+name+"{") && strings.Contains(line, `node="staging-default-0"`) {
					t.Errorf("fake series: %s", line)
				}
			}
		}
	}
	// Stale VM (prod-gpu-0) and the null disk write of staging-default-0.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "cubepath_k8s_node_vm_") && strings.Contains(line, `node="prod-gpu-0"`) {
			t.Errorf("stale VM series exposed: %s", line)
		}
		if strings.HasPrefix(line, "cubepath_k8s_node_vm_disk_write") && strings.Contains(line, `node="staging-default-0"`) {
			t.Errorf("null disk write exposed: %s", line)
		}
	}
}

func TestFailedPollExposesNoCounters(t *testing.T) {
	store := snapshot.NewStore()
	now := time.Unix(t0, 0)
	store.Set(&snapshot.Snapshot{TakenAt: now, Clusters: []snapshot.Cluster{{
		UUID: "u", Name: "c",
		Health: &snapshot.Health{PolledAt: now, Up: false, ErrorKind: "tls", Latency: 0, NodesTotal: 0, ReadyzFailed: []string{"etcd"}},
	}}})
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(collector.New(store, func() time.Time { return now }))
	out := exposition(t, reg)
	for _, want := range []string{`cubepath_k8s_cluster_up{cluster="c",cluster_uuid="u"} 0`, `cubepath_k8s_cluster_poll_error{cluster="c",cluster_uuid="u",kind="tls"} 1`,
		`cubepath_k8s_cluster_readyz_check_failed{check="etcd",cluster="c",cluster_uuid="u"} 1`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, name := range []string{"apiserver_latency_seconds", "cluster_nodes", "pods_pending", "pods_failed", "component_ready", "metrics_server_installed"} {
		if strings.Contains(out, "cubepath_k8s_cluster_"+name) || strings.Contains(out, "cubepath_k8s_"+name+"{") {
			t.Errorf("%s must be absent after a failed poll", name)
		}
	}
}

func TestExpiredSnapshotServesOnlyMeta(t *testing.T) {
	reg, store, now := fixtureRegistry(t)
	late := now.Add(snapshot.SnapshotMaxAge + time.Minute)
	reg2 := prometheus.NewPedanticRegistry()
	reg2.MustRegister(collector.New(store, func() time.Time { return late }))
	out := exposition(t, reg2)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "cubepath_k8s_") && !strings.HasPrefix(line, "cubepath_k8s_exporter_") {
			t.Fatalf("cluster series served from an expired snapshot: %s", line)
		}
	}
	if !strings.Contains(out, "cubepath_k8s_exporter_snapshot_age_seconds 660") {
		t.Errorf("snapshot age missing or wrong:\n%s", out)
	}
	if strings.Contains(out, "cubepath_k8s_exporter_clusters ") || strings.Contains(out, "cubepath_k8s_exporter_nodes ") {
		t.Error("cluster and node counts must not be served from an expired snapshot")
	}
	if !strings.Contains(out, "cubepath_k8s_exporter_build_info") || !strings.Contains(out, "cubepath_k8s_exporter_family_available") {
		t.Error("meta metrics must stay")
	}
	// The fresh registry still serves the cluster series.
	if !strings.Contains(exposition(t, reg), "cubepath_k8s_cluster_up") {
		t.Error("the fresh snapshot must serve cluster series")
	}
}

func TestBeforeFirstRefresh(t *testing.T) {
	store := snapshot.NewStore()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(collector.New(store, nil))
	out := exposition(t, reg)
	for _, bad := range []string{"last_refresh_timestamp", "last_refresh_success", "snapshot_age", "cubepath_k8s_cluster"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s must be absent before the first refresh", bad)
		}
	}
	if !strings.Contains(out, "cubepath_k8s_exporter_build_info") || !strings.Contains(out, `refresh_errors_total{stage="clusters"} 0`) {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestCapLabel(t *testing.T) {
	long := strings.Repeat("é", 300)
	if got := collector.CapLabel(long); len([]rune(got)) != collector.MaxLabelLen {
		t.Fatalf("length %d", len([]rune(got)))
	}
	if got := collector.CapLabel("a\xffb"); got != "ab" {
		t.Fatalf("invalid UTF-8 must be removed, got %q", got)
	}
	if collector.CapLabel("ok") != "ok" {
		t.Fatal("short values are untouched")
	}
}

var docNameRe = regexp.MustCompile("(?m)^\\| `(cubepath_k8s[a-z_]*)` \\|")
var descNameRe = regexp.MustCompile(`fqName: "([^"]+)"`)

func TestDocumentedNamesMatchCode(t *testing.T) {
	raw, err := os.ReadFile("../../docs/metrics.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if !strings.Contains(doc, "\n## Data sources\n") {
		t.Error("docs/metrics.md must have a Data sources section")
	}
	documented := map[string]bool{}
	for _, m := range docNameRe.FindAllStringSubmatch(doc, -1) {
		if documented[m[1]] {
			t.Errorf("%s is documented twice", m[1])
		}
		documented[m[1]] = true
	}

	ch := make(chan *prometheus.Desc, 200)
	collector.New(snapshot.NewStore(), nil).Describe(ch)
	close(ch)
	registered := map[string]bool{}
	for d := range ch {
		m := descNameRe.FindStringSubmatch(d.String())
		if m == nil {
			t.Fatalf("cannot read the name of %s", d)
		}
		if registered[m[1]] {
			t.Errorf("%s is described twice", m[1])
		}
		registered[m[1]] = true
	}
	var missing, extra []string
	for n := range registered {
		if !documented[n] {
			missing = append(missing, n)
		}
	}
	for n := range documented {
		if !registered[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("registered but not documented in docs/metrics.md: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("documented in docs/metrics.md but not registered: %v", extra)
	}
	if len(registered) < 50 {
		t.Errorf("only %d metrics registered", len(registered))
	}
}

func TestEveryRegisteredMetricIsEmittedByTheFixtureOrKnownConditional(t *testing.T) {
	reg, _, _ := fixtureRegistry(t)
	out := exposition(t, reg)
	// Conditions the fixture does not create: a failed poll and an API error.
	conditional := map[string]bool{"cubepath_k8s_cluster_poll_error": true, "cubepath_k8s_exporter_api_errors_total": true}
	ch := make(chan *prometheus.Desc, 200)
	collector.New(snapshot.NewStore(), nil).Describe(ch)
	close(ch)
	for d := range ch {
		name := descNameRe.FindStringSubmatch(d.String())[1]
		if conditional[name] {
			continue
		}
		if !strings.Contains(out, "\n"+name+"{") && !strings.Contains(out, "\n"+name+" ") && !strings.HasPrefix(out, name) {
			t.Errorf("the fixture exercises no sample of %s", name)
		}
	}
}

func TestNamesAreDeduplicatedAfterCapping(t *testing.T) {
	long := strings.Repeat("x", collector.MaxLabelLen)
	store := snapshot.NewStore()
	now := time.Now()
	store.Set(&snapshot.Snapshot{TakenAt: now, Clusters: []snapshot.Cluster{{
		UUID: "c1", Name: "c",
		Health: &snapshot.Health{
			PolledAt: now, Up: true, Ready: false,
			ReadyzFailed: []string{long + "a", long + "b"},
			Components:   []snapshot.Component{{Name: long + "a", Ready: 1, Desired: 1}, {Name: long + "b", Ready: 0, Desired: 1}},
		},
		Pools: []snapshot.Pool{{Name: long + "a"}, {Name: long + "b"}},
		Nodes: []snapshot.Node{{Name: long + "a"}, {Name: long + "b"}},
	}}})
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(collector.New(store, nil))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("two names sharing a capped prefix must not produce duplicate samples: %v", err)
	}
	for _, mf := range mfs {
		switch mf.GetName() {
		case "cubepath_k8s_node_info", "cubepath_k8s_node_pool_info", "cubepath_k8s_cluster_readyz_check_failed":
			if len(mf.Metric) != 1 {
				t.Errorf("%s: %d samples, want 1", mf.GetName(), len(mf.Metric))
			}
		}
	}
}

// Every metric a dashboard or a rule reads must exist, so the three artefacts cannot drift apart.
func TestDashboardAndRulesUseRegisteredMetrics(t *testing.T) {
	ch := make(chan *prometheus.Desc, 200)
	collector.New(snapshot.NewStore(), nil).Describe(ch)
	close(ch)
	registered := map[string]bool{}
	for d := range ch {
		registered[descNameRe.FindStringSubmatch(d.String())[1]] = true
	}
	ruleRaw, err := os.ReadFile("../../rules/cubepath-kubernetes.rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Recorded series are defined by the rules file itself.
	for _, m := range regexp.MustCompile(`record: (\S+)`).FindAllStringSubmatch(string(ruleRaw), -1) {
		registered[m[1]] = true
	}
	use := regexp.MustCompile(`cubepath_k8s[a-z_:]*[a-z]`)
	files := []string{"../../rules/cubepath-kubernetes.rules.yaml", "../../dashboards/cubepath-kubernetes.json"}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range use.FindAllString(string(raw), -1) {
			name = strings.TrimSuffix(name, "_bucket")
			if !registered[name] {
				t.Errorf("%s reads %s, which the exporter does not expose", f, name)
			}
		}
	}
	var dash map[string]any
	raw, _ := os.ReadFile("../../dashboards/cubepath-kubernetes.json")
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Errorf("dashboard is not valid JSON: %v", err)
	}
}
