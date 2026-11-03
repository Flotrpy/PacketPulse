package geoip

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"sort"
	"sync"
)

// Recorder wraps a Resolver and remembers every successful answer, so a
// session against real databases can be replayed later with Static.
type Recorder struct {
	Next Resolver
	mu   sync.Mutex
	recs map[netip.Addr]Record
}

func (r *Recorder) Name() string { return r.Next.Name() }

func (r *Recorder) Lookup(ctx context.Context, ip netip.Addr) (Record, error) {
	rec, err := r.Next.Lookup(ctx, ip)
	if err == nil {
		r.mu.Lock()
		if r.recs == nil {
			r.recs = map[netip.Addr]Record{}
		}
		r.recs[ip.Unmap()] = rec
		r.mu.Unlock()
	}
	return rec, err
}

// Save writes the recorded answers in LoadStatic's format.
func (r *Recorder) Save(path, provider string) error {
	r.mu.Lock()
	recs := make([]Record, 0, len(r.recs))
	for _, v := range r.recs {
		recs = append(recs, v)
	}
	r.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].IP < recs[j].IP })
	b, err := json.MarshalIndent(map[string]any{"provider": provider, "records": recs}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
