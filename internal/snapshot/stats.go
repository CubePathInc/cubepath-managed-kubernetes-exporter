package snapshot

import (
	"sync"
	"time"
)

// RequestKey labels the api_requests_total counter.
type RequestKey struct{ API, Result string }

// StatsView is a consistent copy of the exporter statistics.
type StatsView struct {
	LastRefresh   time.Time
	LastSuccess   bool
	LastDuration  time.Duration
	EverSucceeded bool
	Families      map[string]bool
	RefreshErrors map[string]uint64
	APIRequests   map[RequestKey]uint64
	APIErrors     map[string]uint64
	HasRefreshed  bool
}

// Stats records what the exporter itself did. It is safe for concurrent use.
type Stats struct {
	mu   sync.Mutex
	view StatsView
}

func newStats() *Stats {
	s := &Stats{}
	s.view.Families = map[string]bool{}
	s.view.RefreshErrors = map[string]uint64{StageClusters: 0, StagePools: 0, StageNodeVM: 0}
	s.view.APIRequests = map[RequestKey]uint64{}
	// The rejection codes start at zero so increase() sees the very first one.
	s.view.APIErrors = map[string]uint64{"UNAUTHENTICATED": 0, "HTTP_403": 0}
	return s
}

// ObserveRequest counts one HTTP request to the API.
func (s *Stats) ObserveRequest(api, result string) {
	s.mu.Lock()
	s.view.APIRequests[RequestKey{api, result}]++
	s.mu.Unlock()
}

// ObserveAPIError counts one API error by (normalised) code.
func (s *Stats) ObserveAPIError(code string) {
	s.mu.Lock()
	s.view.APIErrors[code]++
	s.mu.Unlock()
}

// ObserveStageError counts one failure of a refresh stage.
func (s *Stats) ObserveStageError(stage string) {
	s.mu.Lock()
	s.view.RefreshErrors[stage]++
	s.mu.Unlock()
}

// SetRefresh records the outcome of a refresh. families replaces the previous availability.
func (s *Stats) SetRefresh(at time.Time, ok bool, d time.Duration, families map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.view.HasRefreshed = true
	s.view.LastRefresh = at
	s.view.LastSuccess = ok
	s.view.LastDuration = d
	if ok {
		s.view.EverSucceeded = true
	}
	s.view.Families = make(map[string]bool, len(families))
	for k, v := range families {
		s.view.Families[k] = v
	}
}

// EverSucceeded reports whether any refresh succeeded.
func (s *Stats) EverSucceeded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view.EverSucceeded
}

// View returns a deep copy.
func (s *Stats) View() StatsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.view
	v.Families = make(map[string]bool, len(s.view.Families))
	for k, x := range s.view.Families {
		v.Families[k] = x
	}
	v.RefreshErrors = make(map[string]uint64, len(s.view.RefreshErrors))
	for k, x := range s.view.RefreshErrors {
		v.RefreshErrors[k] = x
	}
	v.APIRequests = make(map[RequestKey]uint64, len(s.view.APIRequests))
	for k, x := range s.view.APIRequests {
		v.APIRequests[k] = x
	}
	v.APIErrors = make(map[string]uint64, len(s.view.APIErrors))
	for k, x := range s.view.APIErrors {
		v.APIErrors[k] = x
	}
	return v
}
