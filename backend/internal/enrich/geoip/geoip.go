// Package geoip is the IP-to-location middleware: one Resolver interface over
// MaxMind GeoIP2 / GeoLite2 (and DB-IP, same MMDB format) databases and the
// IPinfo API, with field-level merging across providers and a TTL cache that
// merges concurrent lookups of the same IP.
package geoip

import (
	"context"
	"errors"
	"net/netip"
)

// Record is what the providers know about one IP. Zero values mean unknown.
type Record struct {
	IP          string  `json:"ip"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	HasLocation bool    `json:"has_location"`
	AccuracyKM  float64 `json:"accuracy_km,omitempty"`
	City        string  `json:"city,omitempty"`
	Region      string  `json:"region,omitempty"`
	CountryISO2 string  `json:"country_iso2,omitempty"`
	ASN         int     `json:"asn,omitempty"`
	ASOrg       string  `json:"as_org,omitempty"`
	ISP         string  `json:"isp,omitempty"`
	Hostname    string  `json:"hostname,omitempty"`
	// Mobile network codes (MaxMind GeoIP2-ISP). Present when the IP belongs
	// to a cellular operator's address space.
	MobileMCC string `json:"mobile_mcc,omitempty"`
	MobileMNC string `json:"mobile_mnc,omitempty"`
	Anycast   bool   `json:"anycast,omitempty"`
	// Sources maps each populated field group (location, asn, isp, mobile)
	// to the provider that supplied it.
	Sources map[string]string `json:"sources,omitempty"`
}

// Resolver resolves an IP. Implementations must be safe for concurrent use.
type Resolver interface {
	Name() string
	Lookup(ctx context.Context, ip netip.Addr) (Record, error)
}

var (
	// ErrNotFound means the provider has no data for the IP.
	ErrNotFound = errors.New("geoip: no data for address")
	// ErrBogon means the IP is private/reserved and was not sent to any provider.
	ErrBogon = errors.New("geoip: non-routable address")
)

// IsBogon reports addresses no geolocation provider can answer for.
func IsBogon(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range bogons {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

var bogons = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"),
}

func (r *Record) source(group, provider string) {
	if r.Sources == nil {
		r.Sources = map[string]string{}
	}
	r.Sources[group] = provider
}
