package snapshot_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/cubepath"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/fakeapi"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/snapshot"
)

const (
	t0       = 1758100000
	clusterA = "11111111-1111-1111-1111-111111111111"
	clusterB = "22222222-2222-2222-2222-222222222222"
)

type env struct {
	fake  *fakeapi.Server
	srv   *httptest.Server
	store *snapshot.Store
	ref   *snapshot.Refresher
	now   *atomic.Int64
}

func (e *env) at() time.Time { return time.Unix(e.now.Load(), 0) }

func newEnv(t *testing.T, cfg snapshot.Config) *env {
	t.Helper()
	e := &env{now: &atomic.Int64{}}
	e.now.Store(t0)
	fake, err := fakeapi.LoadFixture("../../testdata/fixture", "tok")
	if err != nil {
		t.Fatal(err)
	}
	fake.Now = func() int64 { return t0 } // the data stays frozen while the refresher's clock moves
	e.fake = fake
	e.srv = httptest.NewServer(fake)
	t.Cleanup(e.srv.Close)
	e.store = snapshot.NewStore()
	client, err := cubepath.New(cubepath.Options{BaseURL: e.srv.URL, Token: "tok", Observe: e.store.Stats().ObserveRequest, Now: e.at})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval == 0 {
		cfg.Interval = 60 * time.Second
	}
	e.ref = snapshot.NewRefresher(client, e.store, cfg, nil, e.at)
	return e
}

func cluster(t *testing.T, sn *snapshot.Snapshot, uuid string) snapshot.Cluster {
	t.Helper()
	for _, c := range sn.Clusters {
		if c.UUID == uuid {
			return c
		}
	}
	t.Fatalf("cluster %s not in snapshot", uuid)
	return snapshot.Cluster{}
}

func node(t *testing.T, c snapshot.Cluster, name string) snapshot.Node {
	t.Helper()
	for _, n := range c.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node %s not found", name)
	return snapshot.Node{}
}

func TestRefreshBuildsSnapshot(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	if res := e.ref.Refresh(context.Background()); res.Err != nil {
		t.Fatal(res.Err)
	}
	sn := e.store.Current(e.at())
	if sn == nil || len(sn.Clusters) != 2 || sn.NodeCount() != 4 || sn.Truncated {
		t.Fatalf("snapshot: %+v", sn)
	}
	if !e.store.Ready() {
		t.Fatal("store must be ready after a successful refresh")
	}
	a := cluster(t, sn, clusterA)
	if a.Health == nil || !a.Health.Up || a.Health.Ready || a.Health.NodesCordoned != 1 || len(a.Health.Components) != 5 {
		t.Fatalf("health: %+v", a.Health)
	}
	if a.Info == nil || a.Info.Location != "bcn-1" || a.Info.HA == nil || !*a.Info.HA || a.Info.Status != "active" {
		t.Fatalf("info: %+v", a.Info)
	}
	if len(a.Pools) != 2 || a.Pools[0].Name != "default" || a.Pools[0].Current != 2 || *a.Pools[0].PlanMemBytes != 4096*1048576 {
		t.Fatalf("pools: %+v", a.Pools)
	}
	n0 := node(t, a, "prod-default-0")
	if n0.Pool != "default" || n0.VPSID != "1001" || n0.VM == nil || *n0.VM.CPURatio != 0.125 || *n0.AllocCPUCores != 1.9 || *n0.UsageCPUCores != 0.38 {
		t.Fatalf("node 0: %+v vm=%+v", n0, n0.VM)
	}
	if n0.VM.NetReceive["public"] != 300 || n0.VM.NetReceive["private"] != 2700 || n0.VM.NetTransmit["public"] != 70 || len(n0.VM.NetReceive) != 2 {
		t.Fatalf("nics: %+v %+v", n0.VM.NetReceive, n0.VM.NetTransmit)
	}
	if n0.VM.Sample != time.Unix(t0-30, 0) {
		t.Fatalf("sample: %v", n0.VM.Sample)
	}
	// Empty breakdown: fall back to the total of VPS.metrics.
	n1 := node(t, a, "prod-default-1")
	if n1.VM.NetReceive["total"] != 6000 || n1.VM.NetTransmit["total"] != 3500 || len(n1.VM.NetReceive) != 1 {
		t.Fatalf("fallback: %+v", n1.VM)
	}
	if n1.DiskPressure == nil || !*n1.DiskPressure {
		t.Fatal("disk pressure must be carried")
	}
	n2 := node(t, a, "prod-gpu-0")
	if n2.Pool != "gpu" || n2.UsageCPUCores != nil || n2.UsageMemBytes != nil {
		t.Fatalf("node 2: %+v", n2)
	}
	v := e.store.Stats().View()
	if !v.Families[snapshot.FamilyClusters] || !v.Families[snapshot.FamilyPools] || !v.Families[snapshot.FamilyNodeVM] || !v.LastSuccess {
		t.Fatalf("families: %+v", v.Families)
	}
}

func TestStaleLatestPointIsDropped(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.ref.Refresh(context.Background())
	n2 := node(t, cluster(t, e.store.Current(e.at()), clusterA), "prod-gpu-0")
	if n2.VM != nil {
		t.Fatalf("every point of 1003 is 15 minutes old, the VM must be absent: %+v", n2.VM)
	}
	// The same fixture, 10 minutes later, is entirely stale: no VM at all.
	e.now.Add(600)
	e.ref.Refresh(context.Background())
	sn := e.store.Current(e.at())
	for _, c := range sn.Clusters {
		for _, n := range c.Nodes {
			if n.VM != nil {
				t.Fatalf("node %s must have no VM after the series aged", n.Name)
			}
		}
	}
	if e.store.Stats().View().Families[snapshot.FamilyNodeVM] {
		t.Fatal("node_vm must be unavailable when nothing is fresh")
	}
}

func TestNullFieldsAreAbsentNotZero(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.ref.Refresh(context.Background())
	sn := e.store.Current(e.at())
	b := cluster(t, sn, clusterB)
	if b.Health != nil {
		t.Fatal("null health must stay nil")
	}
	n := node(t, b, "staging-default-0")
	if n.Ready != nil || n.Unschedulable != nil || n.MemoryPressure != nil || n.DiskPressure != nil || n.PIDPressure != nil ||
		n.ReadySince != nil || n.LastSeen != nil || n.AllocCPUCores != nil || n.AllocMemBytes != nil || n.AllocPods != nil ||
		n.UsageCPUCores != nil || n.UsageMemBytes != nil || n.KubeletVersion != "" {
		t.Fatalf("null node fields must stay nil: %+v", n)
	}
	// The latest disk_write point of 2001 is null: the series is absent although an older point exists.
	if n.VM == nil || n.VM.DiskWrite != nil || n.VM.DiskRead == nil {
		t.Fatalf("vm: %+v", n.VM)
	}
	// Only the public side has data: no private NIC is invented.
	if _, ok := n.VM.NetReceive["private"]; ok || n.VM.NetReceive["public"] != 80 || len(n.VM.NetReceive) != 1 {
		t.Fatalf("nics: %+v", n.VM.NetReceive)
	}
}

func TestMissingVPSScopeDropsOnlyVMFamily(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.fake.SetForbidVPS(true)
	res := e.ref.Refresh(context.Background())
	if res.Err != nil {
		t.Fatalf("a missing vps:read scope must not fail the refresh: %v", res.Err)
	}
	sn := e.store.Current(e.at())
	if sn == nil || sn.NodeCount() != 4 {
		t.Fatalf("snapshot: %+v", sn)
	}
	for _, c := range sn.Clusters {
		for _, n := range c.Nodes {
			if n.VM != nil {
				t.Fatal("no VM data without vps:read")
			}
		}
	}
	a := cluster(t, sn, clusterA)
	if a.Health == nil || len(a.Pools) != 2 || node(t, a, "prod-default-0").UsageCPUCores == nil {
		t.Fatal("cluster, pool and node families must be untouched")
	}
	v := e.store.Stats().View()
	if v.Families[snapshot.FamilyNodeVM] || !v.Families[snapshot.FamilyPools] || !v.Families[snapshot.FamilyClusters] {
		t.Fatalf("families: %+v", v.Families)
	}
	if v.APIErrors["FORBIDDEN"] == 0 {
		t.Fatalf("FORBIDDEN must be counted: %+v", v.APIErrors)
	}
	// The scope is missing for every server: the loop stops after the first chunk answer.
	if c := e.fake.Calls(); c.VMs != 1 {
		t.Fatalf("expected one VM request, got %d", c.VMs)
	}
}

func TestFailureKeepsOldSnapshotThenExpires(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	if res := e.ref.Refresh(context.Background()); res.Err != nil {
		t.Fatal(res.Err)
	}
	first := e.store.Current(e.at())
	e.fake.SetGraphQLStatus(500, "")
	e.now.Add(60)
	res := e.ref.Refresh(context.Background())
	if res.Err == nil {
		t.Fatal("expected a failed refresh")
	}
	if got := e.store.Current(e.at()); got != first {
		t.Fatal("the previous snapshot must keep being served")
	}
	v := e.store.Stats().View()
	if v.LastSuccess || v.RefreshErrors[snapshot.StageClusters] != 1 || v.APIErrors["HTTP_5XX"] != 1 || !v.EverSucceeded {
		t.Fatalf("stats: %+v", v)
	}
	if !e.store.Ready() {
		t.Fatal("readiness must survive a later failure while a snapshot is served")
	}
	e.now.Add(int64(snapshot.SnapshotMaxAge/time.Second) + 1)
	if e.store.Current(e.at()) != nil {
		t.Fatal("the snapshot must expire after 10 minutes without a successful refresh")
	}
	if e.store.Last() == nil {
		t.Fatal("Last must still return the expired snapshot for the age gauge")
	}
}

func TestRateLimitedRefreshReportsRetryAfter(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.fake.SetGraphQLStatus(429, "7")
	res := e.ref.Refresh(context.Background())
	if res.Err == nil || res.RetryAfter != 7*time.Second {
		t.Fatalf("res=%+v", res)
	}
	if e.store.Stats().View().APIErrors["RATE_LIMITED"] != 1 {
		t.Fatal("RATE_LIMITED must be counted")
	}
}

func TestUnauthenticatedFails(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	client, _ := cubepath.New(cubepath.Options{BaseURL: e.srv.URL, Token: "wrong"})
	ref := snapshot.NewRefresher(client, e.store, snapshot.Config{Interval: time.Minute}, nil, e.at)
	res := ref.Refresh(context.Background())
	if res.Err == nil || e.store.Ready() || e.store.Stats().View().APIErrors["UNAUTHENTICATED"] != 1 {
		t.Fatalf("res=%+v stats=%+v", res, e.store.Stats().View())
	}
}

func TestInventoryIsCachedForFiveMinutes(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.ref.Refresh(context.Background())
	e.now.Add(60)
	e.ref.Refresh(context.Background())
	c := e.fake.Calls()
	if c.RESTList != 1 || c.RESTPools != 2 || c.Clusters != 2 {
		t.Fatalf("calls: %+v", c)
	}
	e.now.Add(int64(snapshot.InventoryTTL / time.Second))
	e.ref.Refresh(context.Background())
	if c := e.fake.Calls(); c.RESTList != 2 || c.RESTPools != 4 {
		t.Fatalf("calls after the TTL: %+v", c)
	}
}

func TestRESTFailureDegradesToAbsentFamily(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.fake.SetRESTStatus(500)
	if res := e.ref.Refresh(context.Background()); res.Err != nil {
		t.Fatal(res.Err)
	}
	sn := e.store.Current(e.at())
	a := cluster(t, sn, clusterA)
	if a.Pools != nil || a.Info != nil || node(t, a, "prod-default-0").Pool != "" || a.Health == nil {
		t.Fatalf("cluster: %+v", a)
	}
	v := e.store.Stats().View()
	if v.Families[snapshot.FamilyPools] || !v.Families[snapshot.FamilyClusters] || v.RefreshErrors[snapshot.StagePools] != 1 {
		t.Fatalf("stats: %+v", v)
	}
}

func TestRESTFailureServesCachedInventory(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.ref.Refresh(context.Background())
	e.fake.SetRESTStatus(500)
	e.now.Add(int64(snapshot.InventoryTTL/time.Second) + 1)
	e.ref.Refresh(context.Background())
	a := cluster(t, e.store.Current(e.at()), clusterA)
	if len(a.Pools) != 2 {
		t.Fatal("a cached inventory younger than 15 minutes must keep being served")
	}
	e.now.Add(int64(snapshot.InventoryMaxAge / time.Second))
	e.ref.Refresh(context.Background())
	if a := cluster(t, e.store.Current(e.at()), clusterA); a.Pools != nil {
		t.Fatal("an inventory older than 15 minutes must be dropped")
	}
}

func TestClusterAndProjectFilters(t *testing.T) {
	e := newEnv(t, snapshot.Config{Clusters: []string{strings.ToUpper(clusterB)}})
	e.ref.Refresh(context.Background())
	sn := e.store.Current(e.at())
	if len(sn.Clusters) != 1 || sn.Clusters[0].UUID != clusterB {
		t.Fatalf("filter: %+v", sn.Clusters)
	}
	if c := e.fake.Calls(); c.RESTPools != 1 {
		t.Fatalf("only the filtered cluster may be asked for pools: %+v", c)
	}
	// The filter applies before the expensive read: only the chosen cluster is priced.
	if c := e.fake.Calls(); len(c.ClusterIDs) != 1 || len(c.ClusterIDs[0]) != 1 || c.ClusterIDs[0][0] != clusterB {
		t.Fatalf("cluster detail requests: %+v", c.ClusterIDs)
	}
	// The project filter applies to the REST inventory (the fixture clusters are in project 7).
	e = newEnv(t, snapshot.Config{ProjectID: "8"})
	e.ref.Refresh(context.Background())
	for _, c := range e.store.Current(e.at()).Clusters {
		if c.Info != nil || c.Pools != nil {
			t.Fatal("clusters of another project must get no inventory")
		}
	}
}

func bigCluster(uuid string, nodes int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`{"uuid":%q,"name":"c-%s","health":null,"nodes":[`, uuid, uuid))
	for i := nodes - 1; i >= 0; i-- { // descending: the cut must still be deterministic
		if i != nodes-1 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf(`{"name":"n-%05d","ready":true,"vps":null}`, i))
	}
	sb.WriteString(`]}`)
	return sb.String()
}

func TestCardinalityGuard(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	var parts []string
	for i := snapshot.MaxClusters + 3; i > 0; i-- {
		parts = append(parts, bigCluster(fmt.Sprintf("c%04d", i), 1))
	}
	e.fake.SetClustersBody(`{"data":{"kubernetesClusters":[` + strings.Join(parts, ",") + `]}}`)
	e.ref.Refresh(context.Background())
	sn := e.store.Current(e.at())
	if !sn.Truncated || len(sn.Clusters) != snapshot.MaxClusters || sn.Clusters[0].UUID != "c0001" || sn.Clusters[snapshot.MaxClusters-1].UUID != "c0100" {
		t.Fatalf("clusters: %d truncated=%v first=%s", len(sn.Clusters), sn.Truncated, sn.Clusters[0].UUID)
	}
	// 100 clusters at 240 points each, fetched 15 per request: no request nears the budget and the
	// API never answers QUERY_TOO_COMPLEX, however many clusters the token has.
	c := e.fake.Calls()
	if c.Clusters != 1 || c.ClusterDetails != 7 || nonZero(e.store.Stats().View().APIErrors) != 0 {
		t.Fatalf("calls %+v errors %+v", c, e.store.Stats().View().APIErrors)
	}
	for _, ids := range c.ClusterIDs {
		if len(ids) > cubepath.MaxClustersPerRequest {
			t.Fatalf("request with %d clusters", len(ids))
		}
	}

	e.fake.SetClustersBody(`{"data":{"kubernetesClusters":[` + bigCluster("big", snapshot.MaxNodes+5) + `]}}`)
	e.now.Add(60)
	e.ref.Refresh(context.Background())
	sn = e.store.Current(e.at())
	c0 := sn.Clusters[0]
	if !sn.Truncated || len(c0.Nodes) != snapshot.MaxNodes || c0.Nodes[0].Name != "n-00000" || c0.Nodes[len(c0.Nodes)-1].Name != fmt.Sprintf("n-%05d", snapshot.MaxNodes-1) {
		t.Fatalf("nodes: %d truncated=%v", len(c0.Nodes), sn.Truncated)
	}

	e.fake.SetClustersBody(`{"data":{"kubernetesClusters":[` + bigCluster("small", 3) + `]}}`)
	e.now.Add(60)
	e.ref.Refresh(context.Background())
	if e.store.Current(e.at()).Truncated {
		t.Fatal("truncated must reset when the limits are not hit")
	}
}

func TestVMRequestsAreChunked(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	var nodes []string
	for i := 0; i < 40; i++ {
		id := fmt.Sprint(9000 + i)
		nodes = append(nodes, fmt.Sprintf(`{"name":"n%02d","ready":true,"vps":{"id":%q,"name":"n%02d"}}`, i, id, i))
		e.fake.VMs[id] = `{"id":"` + id + `","metrics":{"step":30,"series":[{"name":"cpu_usage","unit":"RATIO","points":[{"ts":@{-10},"value":0.5}]}]},"networkBreakdown":{"step":30,"series":[]}}`
	}
	e.fake.SetClustersBody(`{"data":{"kubernetesClusters":[{"uuid":"big","name":"big","health":null,"nodes":[` + strings.Join(nodes, ",") + `]}]}}`)
	if res := e.ref.Refresh(context.Background()); res.Err != nil {
		t.Fatal(res.Err)
	}
	c := e.fake.Calls()
	if c.VMs != 3 {
		t.Fatalf("40 servers need 3 requests, got %d", c.VMs)
	}
	var sizes []int
	for _, ids := range c.VMIDs {
		sizes = append(sizes, len(ids))
	}
	total := 0
	for _, s := range sizes {
		total += s
		if s > cubepath.MaxAliasesPerRequest {
			t.Fatalf("request with %d aliases", s)
		}
	}
	if total != 40 {
		t.Fatalf("sizes %v", sizes)
	}
	withVM := 0
	for _, n := range e.store.Current(e.at()).Clusters[0].Nodes {
		if n.VM != nil && *n.VM.CPURatio == 0.5 {
			withVM++
		}
	}
	if withVM != 40 {
		t.Fatalf("%d nodes with a VM", withVM)
	}
}

type countingSource struct {
	snapshot.Source
	active, max atomic.Int32
}

func (c *countingSource) ClusterRefs(ctx context.Context, p string) ([]cubepath.ClusterRef, []cubepath.GQLError, error) {
	n := c.active.Add(1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(15 * time.Millisecond)
	c.active.Add(-1)
	return c.Source.ClusterRefs(ctx, p)
}

func TestRunNeverOverlaps(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	client, _ := cubepath.New(cubepath.Options{BaseURL: e.srv.URL, Token: "tok"})
	src := &countingSource{Source: client}
	ref := snapshot.NewRefresher(src, e.store, snapshot.Config{Interval: 5 * time.Millisecond, Timeout: time.Second}, nil, e.at)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); ref.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.fake.Calls().Clusters < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if e.fake.Calls().Clusters < 4 {
		t.Fatal("the loop did not refresh repeatedly")
	}
	if src.max.Load() != 1 {
		t.Fatalf("refreshes overlapped: max concurrency %d", src.max.Load())
	}
}

func TestRunBacksOffOnRateLimit(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.fake.SetGraphQLStatus(429, "30")
	client, _ := cubepath.New(cubepath.Options{BaseURL: e.srv.URL, Token: "tok", Now: e.at, Observe: e.store.Stats().ObserveRequest})
	ref := snapshot.NewRefresher(client, e.store, snapshot.Config{Interval: 5 * time.Millisecond}, nil, e.at)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ref.Run(ctx)
	if n := e.fake.Calls().Clusters; n != 0 {
		t.Fatalf("clusters answered %d times", n)
	}
	// 429 answers are not counted as cluster calls by the fake: count the metric instead.
	if got := e.store.Stats().View().APIRequests[snapshot.RequestKey{API: "graphql", Result: "rate_limited"}]; got != 1 {
		t.Fatalf("a 30 s Retry-After must hold the loop back, saw %d requests", got)
	}
}

func TestPartialClusterFailureKeepsLastKnownThenDrops(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	if res := e.ref.Refresh(context.Background()); res.Err != nil {
		t.Fatal(res.Err)
	}
	e.fake.SetClusterFault(clusterB, "INTERNAL_ERROR")
	e.now.Add(60)
	if res := e.ref.Refresh(context.Background()); res.Err != nil {
		t.Fatalf("one readable cluster is enough: %v", res.Err)
	}
	sn := e.store.Current(e.at())
	if len(sn.Clusters) != 2 {
		t.Fatalf("the unread cluster must keep its last known data: %d clusters", len(sn.Clusters))
	}
	v := e.store.Stats().View()
	if v.RefreshErrors[snapshot.StageClusters] != 1 || v.APIErrors["INTERNAL_ERROR"] != 1 {
		t.Fatalf("stats: %+v", v)
	}
	e.now.Add(int64(snapshot.SnapshotMaxAge/time.Second) + 1)
	e.ref.Refresh(context.Background())
	sn = e.store.Current(e.at())
	if len(sn.Clusters) != 1 || sn.Clusters[0].UUID != clusterA {
		t.Fatalf("data older than 10 minutes must be dropped: %+v", sn.Clusters)
	}
}

func TestAllClustersUnreadableFailsTheRefresh(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.ref.Refresh(context.Background())
	first := e.store.Current(e.at())
	e.fake.SetClusterDetailStatus(500, "")
	e.now.Add(60)
	res := e.ref.Refresh(context.Background())
	if res.Err == nil || e.store.Current(e.at()) != first {
		t.Fatalf("res=%+v", res)
	}
	if v := e.store.Stats().View(); v.LastSuccess || v.RefreshErrors[snapshot.StageClusters] != 1 {
		t.Fatalf("stats: %+v", v)
	}
	e.fake.SetClusterDetailStatus(429, "9")
	res = e.ref.Refresh(context.Background())
	if res.RetryAfter != 9*time.Second {
		t.Fatalf("retry after: %+v", res)
	}
}

func TestInventoryKeepsPoolsWhenRESTIsLimitedMidway(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.ref.Refresh(context.Background())
	e.fake.SetRESTPoolsStatus(429)
	e.now.Add(int64(snapshot.InventoryTTL/time.Second) + 1)
	e.ref.Refresh(context.Background())
	a := cluster(t, e.store.Current(e.at()), clusterA)
	if len(a.Pools) != 2 || node(t, a, "prod-default-0").Pool != "default" {
		t.Fatalf("a rate limit must not drop the known pools: %+v", a.Pools)
	}
	before := e.fake.Calls().RESTPools
	// The incomplete inventory is retried soon, not after five minutes.
	e.fake.SetRESTPoolsStatus(0)
	e.now.Add(int64(snapshot.InventoryRetryTTL/time.Second) + 1)
	e.ref.Refresh(context.Background())
	if e.fake.Calls().RESTPools <= before {
		t.Fatal("an inventory with unread clusters must be retried after the short TTL")
	}
	// Complete again: back to the long TTL.
	n := e.fake.Calls().RESTPools
	e.now.Add(60)
	e.ref.Refresh(context.Background())
	if e.fake.Calls().RESTPools != n {
		t.Fatal("a complete inventory is reused for five minutes")
	}
}

func TestVMSeriesCarryOverWhenARequestFails(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.ref.Refresh(context.Background())
	e.fake.SetVMStatus(500, "")
	e.now.Add(60)
	if res := e.ref.Refresh(context.Background()); res.Err != nil {
		t.Fatal(res.Err)
	}
	n0 := node(t, cluster(t, e.store.Current(e.at()), clusterA), "prod-default-0")
	if n0.VM == nil || n0.VM.CPURatio == nil {
		t.Fatal("a failed request must not make the server series vanish for one scrape")
	}
	if e.store.Stats().View().RefreshErrors[snapshot.StageNodeVM] == 0 {
		t.Fatal("the failed request must be counted")
	}
	// Carried data ages out like any other series.
	e.now.Add(int64(snapshot.SeriesMaxAge/time.Second) + 1)
	e.ref.Refresh(context.Background())
	if n := node(t, cluster(t, e.store.Current(e.at()), clusterA), "prod-default-0"); n.VM != nil {
		t.Fatal("a carried sample older than the series max age must be dropped")
	}
}

func TestSkippedVMChunksAreCounted(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	var nodes []string
	for i := 0; i < 90; i++ { // six chunks
		id := fmt.Sprint(9000 + i)
		nodes = append(nodes, fmt.Sprintf(`{"name":"n%02d","ready":true,"vps":{"id":%q,"name":"n%02d"}}`, i, id, i))
	}
	e.fake.SetClustersBody(`{"data":{"kubernetesClusters":[{"uuid":"big","name":"big","health":null,"nodes":[` + strings.Join(nodes, ",") + `]}]}}`)
	e.fake.SetVMStatus(429, "5")
	res := e.ref.Refresh(context.Background())
	if res.Err != nil || res.RetryAfter != 5*time.Second {
		t.Fatalf("res=%+v", res)
	}
	// Each of the six chunks either failed or was skipped after the stop: all are visible.
	if n := e.store.Stats().View().RefreshErrors[snapshot.StageNodeVM]; n != 6 {
		t.Fatalf("node_vm errors: %d", n)
	}
}

func TestNodeStateIsAbsentWhenClusterNotPolled(t *testing.T) {
	e := newEnv(t, snapshot.Config{})
	e.fake.SetClustersBody(`{"data":{"kubernetesClusters":[{"uuid":"old","name":"old","health":null,"nodes":[` +
		`{"name":"w1","ready":true,"memoryPressure":false,"kubeletVersion":"v1.31.5","allocatable":{"cpuM":2000,"memBytes":1,"pods":110},` +
		`"usage":{"cpuM":10,"memBytes":2},"readySince":@{-9000},"lastSeenAt":@{-3000},"vps":null}]}]}}`)
	e.ref.Refresh(context.Background())
	c := e.store.Current(e.at()).Clusters[0]
	n := c.Nodes[0]
	if c.Health != nil || n.Ready != nil || n.MemoryPressure != nil || n.KubeletVersion != "" || n.AllocCPUCores != nil ||
		n.UsageCPUCores != nil || n.ReadySince != nil {
		t.Fatalf("an unpolled cluster must expose no live node state: %+v", n)
	}
	if n.LastSeen == nil {
		t.Fatal("last_seen stays: it says how old the data is")
	}
}

func nonZero(m map[string]uint64) int {
	n := 0
	for _, v := range m {
		if v != 0 {
			n++
		}
	}
	return n
}
