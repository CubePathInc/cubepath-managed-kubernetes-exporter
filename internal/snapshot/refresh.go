package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/cubepath"
)

const (
	// MinInterval is the fastest refresh: the collector polls every 60 s, so faster adds nothing.
	MinInterval = 30 * time.Second
	// DefaultInterval is the default refresh interval.
	DefaultInterval = 60 * time.Second
	// MaxBackoff bounds the delay after consecutive failed refreshes.
	MaxBackoff = 5 * time.Minute

	// MaxVMRequestsInFlight bounds the concurrent VM chunk requests.
	MaxVMRequestsInFlight = 4
	// MaxRESTRequestsInFlight bounds the concurrent REST requests.
	MaxRESTRequestsInFlight = 3
	// MaxClusterRequestsInFlight bounds the concurrent cluster detail requests.
	MaxClusterRequestsInFlight = 3
)

// Source is the part of the CubePath client the refresher uses.
type Source interface {
	ClusterRefs(ctx context.Context, projectID string) ([]cubepath.ClusterRef, []cubepath.GQLError, error)
	ClusterDetails(ctx context.Context, uuids []string) (cubepath.ClustersResult, error)
	VMs(ctx context.Context, ids []string) (cubepath.VMsResult, error)
	RESTClusters(ctx context.Context) ([]cubepath.RESTCluster, error)
	RESTPools(ctx context.Context, clusterUUID string) ([]cubepath.RESTPool, error)
}

// Config configures a Refresher.
type Config struct {
	// ProjectID optionally restricts the clusters to one project (numeric).
	ProjectID string
	// Clusters optionally restricts the clusters to these uuids.
	Clusters []string
	Interval time.Duration
	// Timeout bounds one refresh; zero means three quarters of Interval.
	Timeout time.Duration
}

// Result is the outcome of one refresh.
type Result struct {
	Err        error
	RetryAfter time.Duration
}

type invEntry struct {
	info    *cubepath.RESTCluster
	pools   []cubepath.RESTPool
	poolsOK bool
}

type inventory struct {
	at time.Time
	// ttl is how long this inventory is reused: InventoryTTL when complete, InventoryRetryTTL
	// when some cluster could not be read.
	ttl       time.Duration
	entries   map[string]*invEntry
	requested map[string]bool
}

// Refresher builds snapshots from the CubePath API. Refresh is not safe for concurrent
// use; Run calls it sequentially so it never overlaps itself.
type Refresher struct {
	src    Source
	store  *Store
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
	filter map[string]bool
	inv    *inventory
	// last known data, used for the part of a refresh that failed (never older than the max ages).
	clusterCache map[string]cachedCluster
	vmPrev       map[string]*VM
}

type cachedCluster struct {
	c  cubepath.Cluster
	at time.Time
}

// NewRefresher builds a Refresher. now may be nil.
func NewRefresher(src Source, store *Store, cfg Config, log *slog.Logger, now func() time.Time) *Refresher {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = cfg.Interval * 3 / 4
	}
	r := &Refresher{src: src, store: store, cfg: cfg, log: log, now: now}
	if len(cfg.Clusters) > 0 {
		r.filter = map[string]bool{}
		for _, u := range cfg.Clusters {
			r.filter[strings.ToLower(u)] = true
		}
	}
	return r
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Run refreshes immediately, then every interval, until ctx is done. A failed refresh backs
// off exponentially and a rate limit waits at least for the Retry-After of the API.
func (r *Refresher) Run(ctx context.Context) {
	failures := 0
	for {
		res := r.Refresh(ctx)
		delay := r.cfg.Interval
		if res.Err != nil {
			failures++
			for i := 1; i < failures && delay < MaxBackoff; i++ {
				delay *= 2
			}
			if delay > MaxBackoff {
				delay = MaxBackoff
			}
		} else {
			failures = 0
		}
		if res.RetryAfter > delay {
			delay = res.RetryAfter
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func retryAfterOf(err error) time.Duration {
	var rl *cubepath.RateLimitError
	if errors.As(err, &rl) {
		return rl.RetryAfter
	}
	return 0
}

func maxDur(a, b time.Duration) time.Duration {
	if b > a {
		return b
	}
	return a
}

func (r *Refresher) countErr(err error) {
	var qe *cubepath.QueryError
	if errors.As(err, &qe) {
		return // already counted from the errors array
	}
	r.store.Stats().ObserveAPIError(cubepath.ErrorCode(err))
}

func (r *Refresher) countGQL(errs []cubepath.GQLError) {
	for _, e := range errs {
		r.store.Stats().ObserveAPIError(cubepath.NormalizeCode(e.Code()))
	}
}

// Refresh performs one refresh. On a complete failure the previous snapshot stays in the store.
func (r *Refresher) Refresh(ctx context.Context) Result {
	start := r.now()
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	st := r.store.Stats()
	var retry time.Duration

	refs, errs, err := r.src.ClusterRefs(ctx, r.cfg.ProjectID)
	r.countGQL(errs)
	if err != nil {
		r.countErr(err)
		st.ObserveStageError(StageClusters)
		st.SetRefresh(r.now(), false, r.now().Sub(start), map[string]bool{FamilyClusters: false})
		r.log.Warn("refresh failed", "stage", StageClusters, "code", cubepath.ErrorCode(err))
		return Result{Err: err, RetryAfter: retryAfterOf(err)}
	}

	uuids, truncated := r.selectRefs(refs)
	now := r.now()

	clusters, rt, err := r.loadClusters(ctx, uuids, now)
	retry = maxDur(retry, rt)
	if err != nil {
		st.SetRefresh(r.now(), false, r.now().Sub(start), map[string]bool{FamilyClusters: false})
		r.log.Warn("refresh failed", "stage", StageClusters, "code", cubepath.ErrorCode(err))
		return Result{Err: err, RetryAfter: retry}
	}
	if r.limitNodes(clusters) {
		truncated = true
	}

	inv, poolsOK, rt := r.loadInventory(ctx, uuids, now)
	retry = maxDur(retry, rt)

	vms, vmOK, rt := r.loadVMs(ctx, clusters, now)
	retry = maxDur(retry, rt)

	sn := &Snapshot{TakenAt: r.now(), Truncated: truncated}
	for i := range clusters {
		sn.Clusters = append(sn.Clusters, buildCluster(&clusters[i], inv[strings.ToLower(clusters[i].UUID)], poolsOK, vms, now))
	}
	r.store.Set(sn)
	st.SetRefresh(sn.TakenAt, true, sn.TakenAt.Sub(start), map[string]bool{
		FamilyClusters: true, FamilyPools: poolsOK, FamilyNodeVM: vmOK,
	})
	r.log.Info("refresh done", "clusters", len(sn.Clusters), "nodes", sn.NodeCount(),
		"pools_available", poolsOK, "node_vm_available", vmOK, "truncated", truncated)
	return Result{RetryAfter: retry}
}

// selectRefs applies the cluster filter and the cluster cap, deterministically, before
// anything expensive is read. It returns the uuids to fetch.
func (r *Refresher) selectRefs(in []cubepath.ClusterRef) ([]string, bool) {
	seen := map[string]bool{}
	var out []string
	for _, c := range in {
		key := strings.ToLower(c.UUID)
		if c.UUID == "" || seen[key] || (r.filter != nil && !r.filter[key]) || !cubepath.ValidClusterUUID(c.UUID) {
			continue
		}
		seen[key] = true
		out = append(out, c.UUID)
	}
	sort.Strings(out)
	if len(out) > MaxClusters {
		return out[:MaxClusters], true
	}
	return out, false
}

// loadClusters reads health and nodes of the selected clusters, a few per request (the API
// prices both fields per cluster). A cluster whose request failed keeps its last known data
// while that is younger than SnapshotMaxAge. It fails only when not one cluster could be read.
func (r *Refresher) loadClusters(ctx context.Context, uuids []string, now time.Time) ([]cubepath.Cluster, time.Duration, error) {
	if len(uuids) == 0 {
		r.clusterCache = nil
		return nil, 0, nil
	}
	st := r.store.Stats()
	chunks := cubepath.ChunksOf(uuids, cubepath.MaxClustersPerRequest)
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		stop    atomic.Bool
		retry   time.Duration
		lastErr error
		fetched = map[string]*cubepath.Cluster{}
		counted = make([]bool, len(chunks)) // a stage error was already recorded
		sem     = make(chan struct{}, MaxClusterRequestsInFlight)
	)
	for i, chunk := range chunks {
		wg.Add(1)
		go func(i int, chunk []string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if stop.Load() {
				return
			}
			res, err := r.src.ClusterDetails(ctx, chunk)
			r.countGQL(res.Errors)
			if err != nil {
				r.countErr(err)
				st.ObserveStageError(StageClusters)
				mu.Lock()
				counted[i] = true
				lastErr = err
				retry = maxDur(retry, retryAfterOf(err))
				mu.Unlock()
				if retryAfterOf(err) > 0 || errors.Is(err, cubepath.ErrUnauthenticated) {
					stop.Store(true)
				}
				return
			}
			mu.Lock()
			for u, c := range res.ByUUID {
				fetched[u] = c
			}
			mu.Unlock()
		}(i, chunk)
	}
	wg.Wait()

	cache := make(map[string]cachedCluster, len(uuids))
	var out []cubepath.Cluster
	missing := 0
	for i, chunk := range chunks {
		for _, u := range chunk {
			if c := fetched[u]; c != nil {
				cache[u] = cachedCluster{c: *c, at: now}
				out = append(out, *c)
				continue
			}
			missing++
			if !counted[i] {
				// Skipped by a stop or by the timeout, or answered null for this cluster.
				st.ObserveStageError(StageClusters)
				counted[i] = true
			}
			if prev, ok := r.clusterCache[u]; ok && now.Sub(prev.at) < SnapshotMaxAge {
				cache[u] = prev
				out = append(out, prev.c)
			}
		}
	}
	if len(fetched) == 0 {
		if lastErr == nil {
			if lastErr = ctx.Err(); lastErr == nil {
				lastErr = errors.New("no cluster could be read")
			}
		}
		return nil, retry, lastErr
	}
	r.clusterCache = cache
	if missing > 0 {
		r.log.Warn("cluster refresh incomplete", "stage", StageClusters, "missing", missing)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out, retry, nil
}

// limitNodes sorts, de-duplicates and caps the nodes of the clusters, deterministically.
// It reports whether the cap was hit.
func (r *Refresher) limitNodes(out []cubepath.Cluster) bool {
	truncated := false
	remaining := MaxNodes
	for i := range out {
		nodes := append([]cubepath.Node(nil), out[i].Nodes...)
		sort.SliceStable(nodes, func(a, b int) bool { return nodes[a].Name < nodes[b].Name })
		var uniq []cubepath.Node
		for _, n := range nodes {
			if n.Name == "" || (len(uniq) > 0 && uniq[len(uniq)-1].Name == n.Name) {
				continue
			}
			uniq = append(uniq, n)
		}
		if len(uniq) > remaining {
			uniq = uniq[:remaining]
			truncated = true
		}
		remaining -= len(uniq)
		out[i].Nodes = uniq
	}
	return truncated
}

// loadInventory returns the REST inventory per cluster (cached for InventoryTTL).
func (r *Refresher) loadInventory(ctx context.Context, uuids []string, now time.Time) (map[string]*invEntry, bool, time.Duration) {
	st := r.store.Stats()
	if r.inv != nil && now.Sub(r.inv.at) < r.inv.ttl && covers(r.inv.requested, uuids) {
		return r.inv.entries, hasPools(r.inv.entries, uuids), 0
	}
	fail := func(err error) (map[string]*invEntry, bool, time.Duration) {
		r.countErr(err)
		st.ObserveStageError(StagePools)
		r.log.Warn("inventory refresh failed", "stage", StagePools, "code", cubepath.ErrorCode(err))
		if r.inv != nil && now.Sub(r.inv.at) < InventoryMaxAge {
			return r.inv.entries, hasPools(r.inv.entries, uuids), retryAfterOf(err)
		}
		return nil, false, retryAfterOf(err)
	}
	if len(uuids) == 0 {
		return nil, true, 0
	}
	list, err := r.src.RESTClusters(ctx)
	if err != nil {
		return fail(err)
	}
	var projectID int64 = -1
	if r.cfg.ProjectID != "" {
		if v, err := strconv.ParseInt(r.cfg.ProjectID, 10, 64); err == nil {
			projectID = v
		}
	}
	want := map[string]bool{}
	for _, u := range uuids {
		want[strings.ToLower(u)] = true
	}
	entries := map[string]*invEntry{}
	for i := range list {
		c := list[i]
		if !want[strings.ToLower(c.UUID)] || !cubepath.ValidClusterUUID(c.UUID) {
			continue
		}
		if projectID >= 0 && c.ProjectID != projectID {
			continue
		}
		entries[strings.ToLower(c.UUID)] = &invEntry{info: &c}
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		stop    atomic.Bool
		retry   time.Duration
		sem     = make(chan struct{}, MaxRESTRequestsInFlight)
		partial atomic.Bool // some cluster could not be read: retry sooner than InventoryTTL
	)
	for key, e := range entries {
		var prev *invEntry
		if r.inv != nil {
			prev = r.inv.entries[key]
		}
		wg.Add(1)
		go func(key string, e *invEntry, prev *invEntry) {
			defer wg.Done()
			// Whatever happens, a cluster that cannot be read now keeps its last known pools.
			restore := func() {
				if prev != nil && prev.poolsOK {
					e.pools, e.poolsOK = prev.pools, true
				}
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				restore()
				partial.Store(true)
				return
			}
			if stop.Load() {
				restore()
				partial.Store(true)
				return
			}
			pools, err := r.src.RESTPools(ctx, e.info.UUID)
			if err != nil {
				var he *cubepath.HTTPError
				if !(errors.As(err, &he) && he.Status == 404) {
					r.countErr(err)
					st.ObserveStageError(StagePools)
					partial.Store(true)
				}
				mu.Lock()
				retry = maxDur(retry, retryAfterOf(err))
				if retryAfterOf(err) > 0 {
					stop.Store(true)
				}
				mu.Unlock()
				restore()
				return
			}
			e.pools, e.poolsOK = pools, true
		}(key, e, prev)
	}
	wg.Wait()
	ttl := InventoryTTL
	if partial.Load() {
		ttl = InventoryRetryTTL
	}
	r.inv = &inventory{at: now, ttl: ttl, entries: entries, requested: toSet(uuids)}
	return entries, hasPools(entries, uuids), retry
}

func toSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[strings.ToLower(s)] = true
	}
	return m
}

func covers(have map[string]bool, uuids []string) bool {
	for _, u := range uuids {
		if !have[strings.ToLower(u)] {
			return false
		}
	}
	return true
}

func hasPools(entries map[string]*invEntry, uuids []string) bool {
	if len(uuids) == 0 {
		return true
	}
	for _, u := range uuids {
		if e := entries[strings.ToLower(u)]; e != nil && e.poolsOK {
			return true
		}
	}
	return false
}

// loadVMs fetches the server series of every node, in chunks that respect the API limits.
func (r *Refresher) loadVMs(ctx context.Context, clusters []cubepath.Cluster, now time.Time) (map[string]*VM, bool, time.Duration) {
	var ids []string
	seen := map[string]bool{}
	for i := range clusters {
		for _, n := range clusters[i].Nodes {
			if n.VPS == nil || !cubepath.ValidVPSID(n.VPS.ID) || seen[n.VPS.ID] {
				continue
			}
			seen[n.VPS.ID] = true
			ids = append(ids, n.VPS.ID)
		}
	}
	if len(ids) == 0 {
		r.vmPrev = nil
		return nil, true, 0
	}
	st := r.store.Stats()
	chunks := cubepath.Chunks(ids)
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		stop    atomic.Bool
		out     = map[string]*VM{}
		retry   time.Duration
		failed  int
		unread  = make([]bool, len(chunks)) // the chunk failed or was skipped: carry its servers over
		counted = make([]bool, len(chunks))
		sem     = make(chan struct{}, MaxVMRequestsInFlight)
	)
	for i, chunk := range chunks {
		unread[i] = true
		wg.Add(1)
		go func(i int, chunk []string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if stop.Load() {
				return
			}
			res, err := r.src.VMs(ctx, chunk)
			r.countGQL(res.Errors)
			if err != nil {
				r.countErr(err)
				st.ObserveStageError(StageNodeVM)
				mu.Lock()
				counted[i] = true
				failed++
				retry = maxDur(retry, retryAfterOf(err))
				mu.Unlock()
				if retryAfterOf(err) > 0 || errors.Is(err, cubepath.ErrUnauthenticated) {
					stop.Store(true)
				}
				return
			}
			if len(res.Forbidden) == len(chunk) {
				// Not one server of this chunk is readable: the token has no vps:read scope.
				stop.Store(true)
			}
			mu.Lock()
			unread[i] = false
			for id, d := range res.ByID {
				if vm := buildVM(d, now); vm != nil {
					out[id] = vm
				}
			}
			mu.Unlock()
		}(i, chunk)
	}
	wg.Wait()
	for i, chunk := range chunks {
		if !unread[i] {
			continue
		}
		if !counted[i] {
			st.ObserveStageError(StageNodeVM) // skipped by a stop or by the timeout
			failed++
		}
		for _, id := range chunk {
			if prev := r.vmPrev[id]; prev != nil && now.Sub(prev.Sample) < SeriesMaxAge {
				out[id] = prev
			}
		}
	}
	r.vmPrev = out
	if failed > 0 {
		r.log.Warn("server series refresh incomplete", "stage", StageNodeVM, "failed_requests", failed)
	}
	return out, len(out) > 0, retry
}

func buildCluster(c *cubepath.Cluster, inv *invEntry, poolsOK bool, vms map[string]*VM, now time.Time) Cluster {
	out := Cluster{UUID: c.UUID, Name: c.Name}
	out.Health = convertHealth(c.Health, now)
	if inv != nil && inv.info != nil {
		out.Info = &Info{Version: inv.info.Version, Status: inv.info.Status, HA: inv.info.HAControlPlane}
		if inv.info.Location != nil {
			out.Info.Location = inv.info.Location.LocationName
		}
	}
	poolByVPS := map[string]string{}
	poolByNode := map[string]string{}
	if inv != nil && inv.poolsOK && poolsOK {
		seen := map[string]bool{}
		pools := append([]cubepath.RESTPool(nil), inv.pools...)
		sort.SliceStable(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
		out.Pools = []Pool{}
		for _, p := range pools {
			if p.Name == "" || seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			pool := Pool{Name: p.Name, Desired: p.DesiredNodes, Min: p.MinNodes, Max: p.MaxNodes, AutoScale: p.AutoScale}
			if p.Plan != nil {
				pool.Plan = p.Plan.Name
				pool.PlanCPU = p.Plan.CPU
				if p.Plan.RAM != nil {
					v := float64(*p.Plan.RAM) * (1 << 20)
					pool.PlanMemBytes = &v
				}
				if p.Plan.Storage != nil {
					v := float64(*p.Plan.Storage) * (1 << 30)
					pool.PlanDiskByte = &v
				}
			}
			for _, n := range p.Nodes {
				if n.K8sStatus == "removed" {
					continue
				}
				pool.Current++
				poolByVPS[strconv.FormatInt(n.VPSID, 10)] = p.Name
				if n.NodeName != nil && *n.NodeName != "" {
					poolByNode[*n.NodeName] = p.Name
				}
			}
			out.Pools = append(out.Pools, pool)
		}
	}
	// A cluster not polled in the last 10 minutes has no live node state: what the API still
	// holds for its nodes is old, so it is not exposed (the node itself and its server are).
	observed := out.Health != nil
	for _, n := range c.Nodes {
		node := Node{Name: n.Name, LastSeen: ts(n.LastSeenAt)}
		if observed {
			node.Ready, node.Unschedulable = n.Ready, n.Unschedulable
			node.MemoryPressure, node.DiskPressure, node.PIDPressure = n.MemoryPressure, n.DiskPressure, n.PIDPressure
			node.ReadySince = ts(n.ReadySince)
		}
		if observed && n.KubeletVersion != nil {
			node.KubeletVersion = *n.KubeletVersion
		}
		if n.VPS != nil {
			node.VPSID = n.VPS.ID
			node.VM = vms[n.VPS.ID]
		}
		if p, ok := poolByVPS[node.VPSID]; ok && node.VPSID != "" {
			node.Pool = p
		} else if p, ok := poolByNode[n.Name]; ok {
			node.Pool = p
		}
		if a := n.Allocatable; observed && a != nil {
			node.AllocCPUCores = cores(a.CPUM)
			node.AllocMemBytes = a.MemBytes
			if a.Pods != nil {
				v := float64(*a.Pods)
				node.AllocPods = &v
			}
		}
		if u := n.Usage; observed && u != nil {
			node.UsageCPUCores = cores(u.CPUM)
			node.UsageMemBytes = u.MemBytes
		}
		out.Nodes = append(out.Nodes, node)
	}
	return out
}

func cores(m *int64) *float64 {
	if m == nil {
		return nil
	}
	v := float64(*m) / 1000
	return &v
}

func ts(t *cubepath.Timestamp) *time.Time {
	if t == nil || *t <= 0 {
		return nil
	}
	v := time.Unix(int64(*t), 0).UTC()
	return &v
}

func convertHealth(h *cubepath.Health, now time.Time) *Health {
	if h == nil || h.PolledAt <= 0 {
		return nil
	}
	polled := time.Unix(int64(h.PolledAt), 0).UTC()
	if now.Sub(polled) > SnapshotMaxAge {
		return nil // not polled in the last 10 minutes: not monitored
	}
	out := &Health{
		PolledAt: polled, Up: h.Up, Ready: h.Ready, ReadyzFailed: h.ReadyzFailed,
		Latency: time.Duration(h.LatencyMs) * time.Millisecond, Version: h.Version,
		NodesTotal: h.NodesTotal, NodesReady: h.NodesReady, NodesCordoned: h.NodesCordoned,
		PodsPending: h.PodsPending, PodsFailed: h.PodsFailed, MetricsServer: h.MetricsServer,
		ClientCert: ts(h.ClientCertExpires), ServerCert: ts(h.ServerCertExpires), ErrorKind: h.ErrorKind,
	}
	for _, c := range h.Components {
		out.Components = append(out.Components, Component{Name: c.Name, Ready: c.Ready, Desired: c.Desired})
	}
	sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].Name < out.Components[j].Name })
	return out
}
