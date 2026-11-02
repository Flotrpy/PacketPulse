package geoip

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
)

// Static answers from a fixed table. Use it for operator overrides (put it
// first in a Chain, e.g. known router locations) or to replay recorded
// lookups in tests.
type Static struct {
	Label   string
	Records map[netip.Addr]Record
}

func (s *Static) Name() string {
	if s.Label == "" {
		return "static"
	}
	return s.Label
}

func (s *Static) Lookup(ctx context.Context, ip netip.Addr) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	ip = ip.Unmap()
	if IsBogon(ip) {
		return Record{}, ErrBogon
	}
	r, ok := s.Records[ip]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}

// LoadStatic reads a JSON object {"provider": "...", "records": [Record...]}.
func LoadStatic(path string) (*Static, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Provider string   `json:"provider"`
		Records  []Record `json:"records"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("static geoip %s: %w", path, err)
	}
	s := &Static{Label: doc.Provider, Records: map[netip.Addr]Record{}}
	for _, r := range doc.Records {
		a, err := netip.ParseAddr(r.IP)
		if err != nil {
			return nil, fmt.Errorf("static geoip %s: %w", path, err)
		}
		s.Records[a.Unmap()] = r
	}
	return s, nil
}
