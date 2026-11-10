// Package collector exposes a snapshot as Prometheus metrics. Descriptors are stable and
// every sample is built with MustNewConstMetric from memory: a scrape never calls the API.
package collector

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/snapshot"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/version"
)

const (
	namespace     = "cubepath_k8s"
	metaNamespace = "cubepath_k8s_exporter"
	// MaxLabelLen caps every label value that comes from the API.
	MaxLabelLen = 128
)

var (
	clusterLabels = []string{"cluster_uuid", "cluster"}
	poolLabels    = append(append([]string{}, clusterLabels...), "pool")
	nodeLabels    = append(append([]string{}, clusterLabels...), "node")
)

func with(base []string, extra ...string) []string {
	return append(append([]string{}, base...), extra...)
}

func desc(name, help string, labels []string) *prometheus.Desc {
	return prometheus.NewDesc(namespace+"_"+name, help, labels, nil)
}

func metaDesc(name, help string, labels []string) *prometheus.Desc {
	return prometheus.NewDesc(metaNamespace+"_"+name, help, labels, nil)
}

// Collector implements prometheus.Collector over a snapshot.Store.
type Collector struct {
	store *snapshot.Store
	now   func() time.Time

	// Cluster.
	clusterInfo, clusterUp, clusterReady, clusterReadyzFailed, clusterLatency, clusterLastPoll,
	clusterPollError, clusterNodes, clusterPodsPending, clusterPodsFailed, clusterCompReady,
	clusterCompDesired, clusterMetricsServer, clusterClientCert, clusterServerCert, clusterMonitored *prometheus.Desc

	// Node pools.
	poolInfo, poolNodes, poolAutoscale, poolPlanCPU, poolPlanMem, poolPlanDisk *prometheus.Desc

	// Nodes.
	nodeInfo, nodeReady, nodeUnschedulable, nodeCondition, nodeReadySince, nodeLastSeen,
	nodeAllocCPU, nodeAllocMem, nodeAllocPods, nodeUsageCPU, nodeUsageMem *prometheus.Desc

	// Node VM.
	vmCPU, vmMem, vmDiskRead, vmDiskWrite, vmNetRx, vmNetTx, vmSample *prometheus.Desc

	// Exporter meta.
	buildInfo, lastRefreshTS, lastRefreshOK, refreshDuration, refreshErrors, apiRequests, apiErrors,
	familyAvailable, snapshotAge, clustersCount, nodesCount, truncated *prometheus.Desc
}

// New builds a Collector. now may be nil.
func New(store *snapshot.Store, now func() time.Time) *Collector {
	if now == nil {
		now = time.Now
	}
	c := &Collector{store: store, now: now}

	c.clusterInfo = desc("cluster_info", "Static facts of the cluster (value is always 1). Version, status, location and HA come from the CubePath inventory and are empty when it is unavailable.", with(clusterLabels, "version", "status", "location", "ha_control_plane"))
	c.clusterUp = desc("cluster_up", "1 when the CubePath monitoring collector reached the cluster API server in its latest poll.", clusterLabels)
	c.clusterReady = desc("cluster_ready", "1 when every readyz check of the cluster API server passed in the latest poll.", clusterLabels)
	c.clusterReadyzFailed = desc("cluster_readyz_check_failed", "1 for each readyz check that failed in the latest poll.", with(clusterLabels, "check"))
	c.clusterLatency = desc("cluster_apiserver_latency_seconds", "Latency of the latest successful poll of the cluster API server.", clusterLabels)
	c.clusterLastPoll = desc("cluster_last_poll_timestamp_seconds", "Unix time of the latest poll of the cluster by the CubePath monitoring collector.", clusterLabels)
	c.clusterPollError = desc("cluster_poll_error", "1 with the kind of error when the latest poll failed (dns, tcp, tls, unauthorized, readyz, timeout, http or decode).", with(clusterLabels, "kind"))
	c.clusterNodes = desc("cluster_nodes", "Nodes of the cluster by state: total, ready or cordoned.", with(clusterLabels, "state"))
	c.clusterPodsPending = desc("cluster_pods_pending", "Pods in Pending phase in the latest poll.", clusterLabels)
	c.clusterPodsFailed = desc("cluster_pods_failed", "Pods in Failed phase in the latest poll.", clusterLabels)
	c.clusterCompReady = desc("cluster_component_ready", "Ready replicas of a platform component of kube-system (cilium, api_proxy, konnectivity, coredns, autoscaler).", with(clusterLabels, "component"))
	c.clusterCompDesired = desc("cluster_component_desired", "Desired replicas of a platform component of kube-system.", with(clusterLabels, "component"))
	c.clusterMetricsServer = desc("cluster_metrics_server_installed", "1 when metrics-server is installed, which node usage needs.", clusterLabels)
	c.clusterClientCert = desc("cluster_client_cert_expiry_timestamp_seconds", "Unix time at which the client certificate used to monitor the cluster expires.", clusterLabels)
	c.clusterServerCert = desc("cluster_server_cert_expiry_timestamp_seconds", "Unix time at which the API server certificate of the cluster expires.", clusterLabels)
	c.clusterMonitored = desc("cluster_monitored", "1 when the cluster was polled in the last 10 minutes, 0 otherwise (cluster series are then absent).", clusterLabels)

	c.poolInfo = desc("node_pool_info", "Static facts of a node pool (value is always 1).", with(poolLabels, "plan"))
	c.poolNodes = desc("node_pool_nodes", "Nodes of a node pool by state: desired, min, max or current.", with(poolLabels, "state"))
	c.poolAutoscale = desc("node_pool_autoscale_enabled", "1 when autoscaling is enabled for the node pool.", poolLabels)
	c.poolPlanCPU = desc("node_pool_plan_cpu_cores", "Nominal vCPUs of the plan of the node pool.", poolLabels)
	c.poolPlanMem = desc("node_pool_plan_memory_bytes", "Nominal memory of the plan of the node pool.", poolLabels)
	c.poolPlanDisk = desc("node_pool_plan_disk_bytes", "Nominal disk of the plan of the node pool.", poolLabels)

	c.nodeInfo = desc("node_info", "Static facts of a node (value is always 1). Pool is empty when the pool inventory is unavailable.", with(nodeLabels, "pool", "kubelet_version", "vps_id"))
	c.nodeReady = desc("node_ready", "1 when the Ready condition of the node is true.", nodeLabels)
	c.nodeUnschedulable = desc("node_unschedulable", "1 when the node is cordoned.", nodeLabels)
	c.nodeCondition = desc("node_condition", "1 when a pressure condition of the node is true.", with(nodeLabels, "condition"))
	c.nodeReadySince = desc("node_ready_since_timestamp_seconds", "Unix time of the last transition of the Ready condition.", nodeLabels)
	c.nodeLastSeen = desc("node_last_seen_timestamp_seconds", "Unix time of the latest poll that saw the node.", nodeLabels)
	c.nodeAllocCPU = desc("node_allocatable_cpu_cores", "CPU the kubelet offers to pods.", nodeLabels)
	c.nodeAllocMem = desc("node_allocatable_memory_bytes", "Memory the kubelet offers to pods.", nodeLabels)
	c.nodeAllocPods = desc("node_allocatable_pods", "Pods the kubelet accepts.", nodeLabels)
	c.nodeUsageCPU = desc("node_usage_cpu_cores", "CPU used on the node as reported by metrics-server (sampled every 5 minutes).", nodeLabels)
	c.nodeUsageMem = desc("node_usage_memory_bytes", "Memory used on the node as reported by metrics-server (sampled every 5 minutes).", nodeLabels)

	c.vmCPU = desc("node_vm_cpu_usage_ratio", "CPU usage of the server behind the node, measured from outside the guest (0 to 1 of its vCPUs).", nodeLabels)
	c.vmMem = desc("node_vm_memory_usage_bytes", "Memory in use on the server behind the node, measured from outside the guest.", nodeLabels)
	c.vmDiskRead = desc("node_vm_disk_read_bytes_per_second", "Disk read rate of the server behind the node.", nodeLabels)
	c.vmDiskWrite = desc("node_vm_disk_write_bytes_per_second", "Disk write rate of the server behind the node.", nodeLabels)
	c.vmNetRx = desc("node_vm_network_receive_bytes_per_second", "Network receive rate of the server behind the node, per NIC (public, private) or total.", with(nodeLabels, "nic"))
	c.vmNetTx = desc("node_vm_network_transmit_bytes_per_second", "Network transmit rate of the server behind the node, per NIC (public, private) or total.", with(nodeLabels, "nic"))
	c.vmSample = desc("node_vm_sample_timestamp_seconds", "Unix time of the newest sample among the node VM series.", nodeLabels)

	c.buildInfo = metaDesc("build_info", "Build information of the exporter (value is always 1).", []string{"version", "revision"})
	c.lastRefreshTS = metaDesc("last_refresh_timestamp_seconds", "Unix time of the latest refresh attempt.", nil)
	c.lastRefreshOK = metaDesc("last_refresh_success", "1 when the latest refresh succeeded.", nil)
	c.refreshDuration = metaDesc("refresh_duration_seconds", "Duration of the latest refresh.", nil)
	c.refreshErrors = metaDesc("refresh_errors_total", "Failures of a refresh stage (clusters, pools, node_vm).", []string{"stage"})
	c.apiRequests = metaDesc("api_requests_total", "HTTP requests to the CubePath API by API and result.", []string{"api", "result"})
	c.apiErrors = metaDesc("api_errors_total", "Errors returned by the CubePath API by code.", []string{"code"})
	c.familyAvailable = metaDesc("family_available", "1 when the latest refresh could read a family of metrics (clusters, pools, node_vm).", []string{"family"})
	c.snapshotAge = metaDesc("snapshot_age_seconds", "Age of the snapshot the cluster metrics come from.", nil)
	c.clustersCount = metaDesc("clusters", "Clusters in the served snapshot.", nil)
	c.nodesCount = metaDesc("nodes", "Nodes in the served snapshot.", nil)
	c.truncated = metaDesc("truncated", "1 when the snapshot was cut to the cardinality limits.", nil)
	return c
}

func (c *Collector) all() []*prometheus.Desc {
	return []*prometheus.Desc{
		c.clusterInfo, c.clusterUp, c.clusterReady, c.clusterReadyzFailed, c.clusterLatency, c.clusterLastPoll,
		c.clusterPollError, c.clusterNodes, c.clusterPodsPending, c.clusterPodsFailed, c.clusterCompReady,
		c.clusterCompDesired, c.clusterMetricsServer, c.clusterClientCert, c.clusterServerCert, c.clusterMonitored,
		c.poolInfo, c.poolNodes, c.poolAutoscale, c.poolPlanCPU, c.poolPlanMem, c.poolPlanDisk,
		c.nodeInfo, c.nodeReady, c.nodeUnschedulable, c.nodeCondition, c.nodeReadySince, c.nodeLastSeen,
		c.nodeAllocCPU, c.nodeAllocMem, c.nodeAllocPods, c.nodeUsageCPU, c.nodeUsageMem,
		c.vmCPU, c.vmMem, c.vmDiskRead, c.vmDiskWrite, c.vmNetRx, c.vmNetTx, c.vmSample,
		c.buildInfo, c.lastRefreshTS, c.lastRefreshOK, c.refreshDuration, c.refreshErrors, c.apiRequests,
		c.apiErrors, c.familyAvailable, c.snapshotAge, c.clustersCount, c.nodesCount, c.truncated,
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.all() {
		ch <- d
	}
}

// CapLabel makes an API supplied string safe as a label value: valid UTF-8, bounded length.
func CapLabel(s string) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) > MaxLabelLen {
		r := []rune(s)
		s = string(r[:MaxLabelLen])
	}
	return s
}

// firstSeen reports true the first time it sees key in set. Names are de-duplicated after
// CapLabel: two long names with the same 128 character prefix would otherwise produce the
// same label set twice, and the registry would drop the whole family of samples.
func firstSeen(set map[string]bool, key string) bool {
	if set[key] {
		return false
	}
	set[key] = true
	return true
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unix(t time.Time) float64 { return float64(t.Unix()) }

type emitter struct {
	ch chan<- prometheus.Metric
}

func (e emitter) gauge(d *prometheus.Desc, v float64, labels ...string) {
	e.ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func (e emitter) counter(d *prometheus.Desc, v float64, labels ...string) {
	e.ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
}

func (e emitter) optBool(d *prometheus.Desc, v *bool, labels ...string) {
	if v != nil {
		e.gauge(d, b2f(*v), labels...)
	}
}

func (e emitter) optFloat(d *prometheus.Desc, v *float64, labels ...string) {
	if v != nil {
		e.gauge(d, *v, labels...)
	}
}

func (e emitter) optTime(d *prometheus.Desc, v *time.Time, labels ...string) {
	if v != nil {
		e.gauge(d, unix(*v), labels...)
	}
}

// Collect implements prometheus.Collector. It reads memory only.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	e := emitter{ch}
	now := c.now()
	e.gauge(c.buildInfo, 1, CapLabel(version.Version), CapLabel(version.Revision))
	c.collectMeta(e, now)
	if sn := c.store.Current(now); sn != nil {
		for i := range sn.Clusters {
			c.collectCluster(e, &sn.Clusters[i])
		}
	}
}

func (c *Collector) collectMeta(e emitter, now time.Time) {
	v := c.store.Stats().View()
	if v.HasRefreshed {
		e.gauge(c.lastRefreshTS, unix(v.LastRefresh))
		e.gauge(c.lastRefreshOK, b2f(v.LastSuccess))
		e.gauge(c.refreshDuration, v.LastDuration.Seconds())
	}
	for stage, n := range v.RefreshErrors {
		e.counter(c.refreshErrors, float64(n), stage)
	}
	for k, n := range v.APIRequests {
		e.counter(c.apiRequests, float64(n), k.API, k.Result)
	}
	for code, n := range v.APIErrors {
		e.counter(c.apiErrors, float64(n), code)
	}
	for fam, ok := range v.Families {
		e.gauge(c.familyAvailable, b2f(ok), fam)
	}
	if last := c.store.Last(); last != nil {
		age := now.Sub(last.TakenAt).Seconds()
		if age < 0 {
			age = 0
		}
		e.gauge(c.snapshotAge, age)
	}
	if sn := c.store.Current(now); sn != nil {
		e.gauge(c.clustersCount, float64(len(sn.Clusters)))
		e.gauge(c.nodesCount, float64(sn.NodeCount()))
		e.gauge(c.truncated, b2f(sn.Truncated))
	}
}

func (c *Collector) collectCluster(e emitter, cl *snapshot.Cluster) {
	id := []string{CapLabel(cl.UUID), CapLabel(cl.Name)}
	l := func(extra ...string) []string { return append(append([]string{}, id...), extra...) }

	version, status, location, ha := "", "", "", ""
	if cl.Info != nil {
		version, status, location = CapLabel(cl.Info.Version), CapLabel(cl.Info.Status), CapLabel(cl.Info.Location)
		if cl.Info.HA != nil {
			ha = strconv.FormatBool(*cl.Info.HA)
		}
	}
	if version == "" && cl.Health != nil {
		version = CapLabel(cl.Health.Version)
	}
	e.gauge(c.clusterInfo, 1, l(version, status, location, ha)...)
	e.gauge(c.clusterMonitored, b2f(cl.Health != nil), l()...)

	if h := cl.Health; h != nil {
		e.gauge(c.clusterUp, b2f(h.Up), l()...)
		e.gauge(c.clusterReady, b2f(h.Ready), l()...)
		e.gauge(c.clusterLastPoll, unix(h.PolledAt), l()...)
		if h.ErrorKind != "" {
			e.gauge(c.clusterPollError, 1, l(CapLabel(h.ErrorKind))...)
		}
		seenChecks := map[string]bool{}
		for _, chk := range h.ReadyzFailed {
			if firstSeen(seenChecks, CapLabel(chk)) {
				e.gauge(c.clusterReadyzFailed, 1, l(CapLabel(chk))...)
			}
		}
		// The counters of a failed poll are not measurements: they are absent, not zero.
		if h.Up {
			e.gauge(c.clusterLatency, h.Latency.Seconds(), l()...)
			e.gauge(c.clusterNodes, float64(h.NodesTotal), l("total")...)
			e.gauge(c.clusterNodes, float64(h.NodesReady), l("ready")...)
			e.gauge(c.clusterNodes, float64(h.NodesCordoned), l("cordoned")...)
			e.gauge(c.clusterPodsPending, float64(h.PodsPending), l()...)
			e.gauge(c.clusterPodsFailed, float64(h.PodsFailed), l()...)
			seenComps := map[string]bool{}
			for _, comp := range h.Components {
				if !firstSeen(seenComps, CapLabel(comp.Name)) {
					continue
				}
				e.gauge(c.clusterCompReady, float64(comp.Ready), l(CapLabel(comp.Name))...)
				e.gauge(c.clusterCompDesired, float64(comp.Desired), l(CapLabel(comp.Name))...)
			}
			e.gauge(c.clusterMetricsServer, b2f(h.MetricsServer), l()...)
		}
		e.optTime(c.clusterClientCert, h.ClientCert, l()...)
		e.optTime(c.clusterServerCert, h.ServerCert, l()...)
	}

	seenPools := map[string]bool{}
	for _, p := range cl.Pools {
		if !firstSeen(seenPools, CapLabel(p.Name)) {
			continue
		}
		pl := func(extra ...string) []string { return l(append([]string{CapLabel(p.Name)}, extra...)...) }
		e.gauge(c.poolInfo, 1, pl(CapLabel(p.Plan))...)
		if p.Desired != nil {
			e.gauge(c.poolNodes, float64(*p.Desired), pl("desired")...)
		}
		if p.Min != nil {
			e.gauge(c.poolNodes, float64(*p.Min), pl("min")...)
		}
		if p.Max != nil {
			e.gauge(c.poolNodes, float64(*p.Max), pl("max")...)
		}
		e.gauge(c.poolNodes, float64(p.Current), pl("current")...)
		e.optBool(c.poolAutoscale, p.AutoScale, pl()...)
		if p.PlanCPU != nil {
			e.gauge(c.poolPlanCPU, float64(*p.PlanCPU), pl()...)
		}
		e.optFloat(c.poolPlanMem, p.PlanMemBytes, pl()...)
		e.optFloat(c.poolPlanDisk, p.PlanDiskByte, pl()...)
	}

	seenNodes := map[string]bool{}
	for i := range cl.Nodes {
		n := &cl.Nodes[i]
		if !firstSeen(seenNodes, CapLabel(n.Name)) {
			continue
		}
		nl := func(extra ...string) []string { return l(append([]string{CapLabel(n.Name)}, extra...)...) }
		e.gauge(c.nodeInfo, 1, nl(CapLabel(n.Pool), CapLabel(n.KubeletVersion), CapLabel(n.VPSID))...)
		e.optBool(c.nodeReady, n.Ready, nl()...)
		e.optBool(c.nodeUnschedulable, n.Unschedulable, nl()...)
		if n.MemoryPressure != nil {
			e.gauge(c.nodeCondition, b2f(*n.MemoryPressure), nl("memory_pressure")...)
		}
		if n.DiskPressure != nil {
			e.gauge(c.nodeCondition, b2f(*n.DiskPressure), nl("disk_pressure")...)
		}
		if n.PIDPressure != nil {
			e.gauge(c.nodeCondition, b2f(*n.PIDPressure), nl("pid_pressure")...)
		}
		e.optTime(c.nodeReadySince, n.ReadySince, nl()...)
		e.optTime(c.nodeLastSeen, n.LastSeen, nl()...)
		e.optFloat(c.nodeAllocCPU, n.AllocCPUCores, nl()...)
		e.optFloat(c.nodeAllocMem, n.AllocMemBytes, nl()...)
		e.optFloat(c.nodeAllocPods, n.AllocPods, nl()...)
		e.optFloat(c.nodeUsageCPU, n.UsageCPUCores, nl()...)
		e.optFloat(c.nodeUsageMem, n.UsageMemBytes, nl()...)

		if vm := n.VM; vm != nil {
			e.optFloat(c.vmCPU, vm.CPURatio, nl()...)
			e.optFloat(c.vmMem, vm.MemoryBytes, nl()...)
			e.optFloat(c.vmDiskRead, vm.DiskRead, nl()...)
			e.optFloat(c.vmDiskWrite, vm.DiskWrite, nl()...)
			for nic, v := range vm.NetReceive {
				e.gauge(c.vmNetRx, v, nl(nic)...)
			}
			for nic, v := range vm.NetTransmit {
				e.gauge(c.vmNetTx, v, nl(nic)...)
			}
			e.gauge(c.vmSample, unix(vm.Sample), nl()...)
		}
	}
}
