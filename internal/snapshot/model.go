// Package snapshot owns the in-memory picture of the CubePath clusters: a refresh loop
// builds an immutable Snapshot, scrapes only read it.
package snapshot

import (
	"sync/atomic"
	"time"
)

// Cardinality guards: beyond these the snapshot is truncated, deterministically.
const (
	MaxClusters = 100
	MaxNodes    = 1000
)

const (
	// SeriesMaxAge is how old the latest point of a series may be before it is dropped.
	SeriesMaxAge = 5 * time.Minute
	// SnapshotMaxAge is how long a snapshot is served after the last successful refresh.
	SnapshotMaxAge = 10 * time.Minute
)

// Family names of the family_available meta metric.
const (
	FamilyClusters = "clusters"
	FamilyPools    = "pools"
	FamilyNodeVM   = "node_vm"
)

// Refresh stages of the refresh_errors_total meta metric.
const (
	StageClusters = "clusters"
	StagePools    = "pools"
	StageNodeVM   = "node_vm"
)

// NIC label values of the node VM network metrics.
const (
	NICPublic  = "public"
	NICPrivate = "private"
	NICTotal   = "total"
)

// Snapshot is an immutable picture of everything the exporter exposes. Never modify it
// after Store.Set.
type Snapshot struct {
	TakenAt   time.Time
	Clusters  []Cluster
	Truncated bool
}

// NodeCount returns the number of nodes of the snapshot.
func (s *Snapshot) NodeCount() int {
	n := 0
	for i := range s.Clusters {
		n += len(s.Clusters[i].Nodes)
	}
	return n
}

// Cluster is one cluster. Pointer fields are nil when the datum is unknown: nil means
// the series is absent, never zero.
type Cluster struct {
	UUID string
	Name string

	// Health is nil when the cluster was not polled in the last 10 minutes.
	Health *Health
	// Info is the inventory part of cluster_info.
	Info *Info
	// Pools is nil when the API returned the node pools as null.
	Pools []Pool
	Nodes []Node
}

// Info is the inventory part of cluster_info.
type Info struct {
	Version string
	Status  string
	// Location is empty when unknown.
	Location string
	// HA is nil when unknown.
	HA *bool
}

// Health is the latest poll of the cluster API server.
type Health struct {
	PolledAt      time.Time
	Up            bool
	Ready         bool
	ReadyzFailed  []string
	Latency       time.Duration
	Version       string
	NodesTotal    int64
	NodesReady    int64
	NodesCordoned int64
	PodsPending   int64
	PodsFailed    int64
	Components    []Component
	MetricsServer bool
	ClientCert    *time.Time
	ServerCert    *time.Time
	ErrorKind     string
}

// Component is a kube-system platform component.
type Component struct {
	Name    string
	Ready   int64
	Desired int64
}

// Pool is a node pool of a cluster.
type Pool struct {
	Name         string
	Plan         string
	Desired      *int64
	Min          *int64
	Max          *int64
	Current      int64
	AutoScale    *bool
	PlanCPU      *int64
	PlanMemBytes *float64
	PlanDiskByte *float64
}

// Node is a worker node with the server behind it.
type Node struct {
	Name           string
	Pool           string
	VPSID          string
	KubeletVersion string
	Ready          *bool
	Unschedulable  *bool
	MemoryPressure *bool
	DiskPressure   *bool
	PIDPressure    *bool
	ReadySince     *time.Time
	LastSeen       *time.Time
	AllocCPUCores  *float64
	AllocMemBytes  *float64
	AllocPods      *float64
	UsageCPUCores  *float64
	UsageMemBytes  *float64
	VM             *VM
}

// VM holds the latest fresh point of each series of the server behind a node.
type VM struct {
	Sample      time.Time
	CPURatio    *float64
	MemoryBytes *float64
	DiskRead    *float64
	DiskWrite   *float64
	NetReceive  map[string]float64
	NetTransmit map[string]float64
}

// Store holds the current snapshot and the exporter statistics.
type Store struct {
	cur   atomic.Pointer[Snapshot]
	stats *Stats
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{stats: newStats()} }

// Stats returns the statistics recorder.
func (s *Store) Stats() *Stats { return s.stats }

// Set publishes a new snapshot.
func (s *Store) Set(sn *Snapshot) { s.cur.Store(sn) }

// Current returns the snapshot, or nil when there is none or it is older than SnapshotMaxAge.
func (s *Store) Current(now time.Time) *Snapshot {
	sn := s.cur.Load()
	if sn == nil || now.Sub(sn.TakenAt) > SnapshotMaxAge {
		return nil
	}
	return sn
}

// Last returns the latest snapshot even when expired (nil when none), for the age gauge.
func (s *Store) Last() *Snapshot { return s.cur.Load() }

// Ready reports whether at least one refresh succeeded.
func (s *Store) Ready() bool { return s.stats.EverSucceeded() }
