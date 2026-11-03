package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/flotrpy/packetpulse/backend/internal/model"
)

// MemoryRepository is the in-memory JourneyRepository. Journeys are stored
// as JSON so callers can never mutate stored state through shared pointers
// (the same isolation a real database gives). When Capacity is reached the
// oldest journey is evicted.
type MemoryRepository struct {
	mu       sync.RWMutex
	capacity int
	docs     map[string][]byte
	meta     map[string]journeyMeta
	order    []string // insertion order, oldest first
}

type journeyMeta struct {
	dst     string
	verdict string
	ts      int64
}

func NewMemoryRepository(capacity int) *MemoryRepository {
	if capacity <= 0 {
		capacity = 10000
	}
	return &MemoryRepository{capacity: capacity, docs: map[string][]byte{}, meta: map[string]journeyMeta{}}
}

func (m *MemoryRepository) Save(ctx context.Context, j *model.Journey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j.JourneyID == "" {
		return fmt.Errorf("journey has no id")
	}
	b, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("encode journey: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.docs[j.JourneyID]; !exists {
		m.order = append(m.order, j.JourneyID)
	}
	m.docs[j.JourneyID] = b
	m.meta[j.JourneyID] = journeyMeta{dst: j.Metadata.Destination.IP, verdict: j.Compliance.Verdict, ts: j.Metadata.Timestamp.UnixNano()}
	for len(m.order) > m.capacity {
		old := m.order[0]
		m.order = m.order[1:]
		delete(m.docs, old)
		delete(m.meta, old)
	}
	return nil
}

func (m *MemoryRepository) Get(ctx context.Context, id string) (*model.Journey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	b, ok := m.docs[id]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	var j model.Journey
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("decode journey %s: %w", id, err)
	}
	return &j, nil
}

// List returns matching journeys, newest first.
func (m *MemoryRepository) List(ctx context.Context, f JourneyFilter) ([]*model.Journey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.order))
	for _, id := range m.order {
		md := m.meta[id]
		if f.DestinationIP != "" && md.dst != f.DestinationIP {
			continue
		}
		if f.Verdict != "" && md.verdict != f.Verdict {
			continue
		}
		if !f.Since.IsZero() && md.ts < f.Since.UnixNano() {
			continue
		}
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(a, b int) bool { return m.meta[ids[a]].ts > m.meta[ids[b]].ts })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	docs := make([][]byte, len(ids))
	for i, id := range ids {
		docs[i] = m.docs[id]
	}
	m.mu.RUnlock()

	out := make([]*model.Journey, 0, len(docs))
	for _, b := range docs {
		var j model.Journey
		if err := json.Unmarshal(b, &j); err != nil {
			return nil, err
		}
		out = append(out, &j)
	}
	return out, nil
}

func (m *MemoryRepository) Count(ctx context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.docs), ctx.Err()
}
