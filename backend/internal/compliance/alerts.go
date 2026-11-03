package compliance

import (
	"sync"
	"time"
)

// AlertStore keeps recent alerts in memory and fans them out to
// subscribers (SSE streams, webhooks). Slow subscribers drop alerts rather
// than stall evaluation.
type AlertStore struct {
	mu   sync.RWMutex
	cap  int
	ring []ComplianceViolationAlert
	subs map[int]chan ComplianceViolationAlert
	next int
}

func NewAlertStore(capacity int) *AlertStore {
	if capacity <= 0 {
		capacity = 10000
	}
	return &AlertStore{cap: capacity, subs: map[int]chan ComplianceViolationAlert{}}
}

func (s *AlertStore) Publish(a ComplianceViolationAlert) {
	s.mu.Lock()
	s.ring = append(s.ring, a)
	if len(s.ring) > s.cap {
		s.ring = s.ring[len(s.ring)-s.cap:]
	}
	for _, ch := range s.subs {
		select {
		case ch <- a:
		default:
		}
	}
	s.mu.Unlock()
}

// AlertFilter selects alerts; zero values match everything.
type AlertFilter struct {
	ProfileID string
	Severity  string
	PacketID  string
	Since     time.Time
	Limit     int
}

// List returns matching alerts, newest first.
func (s *AlertStore) List(f AlertFilter) []ComplianceViolationAlert {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []ComplianceViolationAlert{}
	for i := len(s.ring) - 1; i >= 0 && len(out) < limit; i-- {
		a := s.ring[i]
		if (f.ProfileID != "" && a.ProfileID != f.ProfileID) || (f.Severity != "" && a.Severity != f.Severity) ||
			(f.PacketID != "" && a.PacketID != f.PacketID) || (!f.Since.IsZero() && a.GeneratedAt.Before(f.Since)) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// Subscribe returns a channel of new alerts and a cancel function.
func (s *AlertStore) Subscribe() (<-chan ComplianceViolationAlert, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := s.next
	ch := make(chan ComplianceViolationAlert, 128)
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			close(c)
			delete(s.subs, id)
		}
	}
}
