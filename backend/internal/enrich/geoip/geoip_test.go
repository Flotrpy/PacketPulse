package geoip

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

// MaxMind reader against the free DB-IP Lite MMDBs (same format as GeoLite2).
// Set PACKETPULSE_MMDB_DIR to a directory holding dbip-city-lite-*.mmdb and
// dbip-asn-lite-*.mmdb (fetch with scripts/research/fetch_datasets.sh and gunzip).
func TestMaxMindWithDBIP(t *testing.T) {
	dir := os.Getenv("PACKETPULSE_MMDB_DIR")
	if dir == "" {
		dir = "/tmp/claude-0/mmdb"
	}
	city, _ := filepath.Glob(filepath.Join(dir, "dbip-city-lite-*.mmdb"))
	asn, _ := filepath.Glob(filepath.Join(dir, "dbip-asn-lite-*.mmdb"))
	if len(city) == 0 || len(asn) == 0 {
		t.Skip("DB-IP MMDB files not available")
	}
	m, err := OpenMaxMind(MaxMindPaths{City: city[0], ASN: asn[0]})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	r, err := m.Lookup(ctx, ip("80.231.200.1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ASN != 6453 || r.CountryISO2 != "FR" || !r.HasLocation || r.Sources["location"] != "maxmind" {
		t.Fatalf("%+v", r)
	}
	if _, err := m.Lookup(ctx, ip("100.72.0.1")); !errors.Is(err, ErrBogon) {
		t.Fatalf("CGNAT should be bogon, got %v", err)
	}
	if !strings.Contains(m.DatabaseTypes()["city"], "City") {
		t.Fatalf("types %v", m.DatabaseTypes())
	}
}

func TestIPinfoStandardAndLite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/8.8.8.8/json":
			w.Write([]byte(`{"ip":"8.8.8.8","hostname":"dns.google","city":"Mountain View","region":"California","country":"US","loc":"37.4056,-122.0775","org":"AS15169 Google LLC","anycast":true}`))
		case "/lite/1.1.1.1":
			w.Write([]byte(`{"ip":"1.1.1.1","asn":"AS13335","as_name":"Cloudflare, Inc.","country_code":"AU","country":"Australia"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	std := &IPinfo{BaseURL: srv.URL, Token: "tok"}
	r, err := std.Lookup(ctx, ip("8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ASN != 15169 || r.ASOrg != "Google LLC" || r.Lat != 37.4056 || r.Hostname != "dns.google" || !r.Anycast || r.Sources["location"] != "ipinfo" {
		t.Fatalf("%+v", r)
	}
	lite := &IPinfo{BaseURL: srv.URL, Token: "tok", Lite: true}
	r, err = lite.Lookup(ctx, ip("1.1.1.1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ASN != 13335 || r.CountryISO2 != "AU" || r.HasLocation || r.Sources["asn"] != "ipinfo_lite" {
		t.Fatalf("%+v", r)
	}
	if _, err := std.Lookup(ctx, ip("9.9.9.9")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 -> ErrNotFound, got %v", err)
	}
	if _, err := std.Lookup(ctx, ip("10.0.0.1")); !errors.Is(err, ErrBogon) {
		t.Fatalf("private -> ErrBogon without a request, got %v", err)
	}
}

func TestIPinfoRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ip":"8.8.8.8","country":"US","loc":"37.4,-122.0","org":"AS15169 Google LLC"}`))
	}))
	defer srv.Close()
	c := &IPinfo{BaseURL: srv.URL, MaxRetries: 4}
	start := time.Now()
	if _, err := c.Lookup(ctx, ip("8.8.8.8")); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || time.Since(start) > 3*time.Second {
		t.Fatalf("calls=%d took %s", calls.Load(), time.Since(start))
	}
}

type countingResolver struct {
	name  string
	rec   map[netip.Addr]Record
	calls atomic.Int32
	delay time.Duration
}

func (c *countingResolver) Name() string { return c.name }
func (c *countingResolver) Lookup(ctx context.Context, a netip.Addr) (Record, error) {
	c.calls.Add(1)
	time.Sleep(c.delay)
	if r, ok := c.rec[a]; ok {
		return r, nil
	}
	return Record{}, ErrNotFound
}

func TestChainMergesFieldsByPriority(t *testing.T) {
	a := ip("80.231.200.1")
	asnOnly := &countingResolver{name: "lite", rec: map[netip.Addr]Record{a: {ASN: 6453, ASOrg: "TATA", CountryISO2: "US", Sources: map[string]string{"asn": "lite"}}}}
	located := &countingResolver{name: "city", rec: map[netip.Addr]Record{a: {HasLocation: true, Lat: 43.3, Lon: 5.37, City: "Marseille", CountryISO2: "FR", ASN: 1, Sources: map[string]string{"location": "city", "asn": "city"}}}}
	r, err := (&Chain{Providers: []Resolver{asnOnly, located}}).Lookup(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if r.ASN != 6453 || r.City != "Marseille" || r.Sources["asn"] != "lite" || r.Sources["location"] != "city" {
		t.Fatalf("%+v", r)
	}
	// First provider's country wins even though it is the registry country.
	if r.CountryISO2 != "US" {
		t.Fatalf("country %s", r.CountryISO2)
	}
	if _, err := (&Chain{Providers: []Resolver{asnOnly}}).Lookup(ctx, ip("1.2.3.4")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCacheDedupesAndExpires(t *testing.T) {
	a := ip("8.8.8.8")
	up := &countingResolver{name: "up", delay: 30 * time.Millisecond, rec: map[netip.Addr]Record{a: {ASN: 15169}}}
	c := NewCache(up, 2, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Lookup(ctx, a) }()
	}
	wg.Wait()
	if up.calls.Load() != 1 {
		t.Fatalf("20 concurrent lookups made %d upstream calls", up.calls.Load())
	}
	// Negative results are cached too.
	c.Lookup(ctx, ip("1.2.3.4"))
	c.Lookup(ctx, ip("1.2.3.4"))
	if up.calls.Load() != 2 {
		t.Fatalf("negative result not cached: %d calls", up.calls.Load())
	}
	// LRU eviction at size 2.
	c.Lookup(ctx, ip("5.6.7.8"))
	c.Lookup(ctx, a)
	if up.calls.Load() != 4 {
		t.Fatalf("expected eviction of 8.8.8.8, calls=%d", up.calls.Load())
	}
	// TTL expiry.
	c2 := NewCache(up, 10, time.Millisecond)
	c2.Lookup(ctx, a)
	time.Sleep(5 * time.Millisecond)
	c2.Lookup(ctx, a)
	if up.calls.Load() != 6 {
		t.Fatalf("expired entry not refreshed, calls=%d", up.calls.Load())
	}
}

func TestStaticAndRecorderRoundTrip(t *testing.T) {
	a := ip("80.231.153.1")
	up := &countingResolver{name: "up", rec: map[netip.Addr]Record{a: {IP: a.String(), City: "Paris", HasLocation: true, Lat: 48.86, Lon: 2.35}}}
	rec := &Recorder{Next: up}
	rec.Lookup(ctx, a)
	path := filepath.Join(t.TempDir(), "rec.json")
	if err := rec.Save(path, "test"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadStatic(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Lookup(ctx, a)
	if err != nil || r.City != "Paris" || s.Name() != "test" {
		t.Fatalf("%+v %v", r, err)
	}
}
