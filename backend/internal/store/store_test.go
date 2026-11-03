package store

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/model"
)

func TestLookupASN(t *testing.T) {
	r := NewSeededReference()
	cases := map[string]int{
		"8.8.8.8": 15169, "180.87.38.1": 6453, "80.231.200.1": 6453, "49.36.0.1": 55836,
		"18.96.32.10": 16509, "184.105.64.1": 6939, "4.69.140.98": 3356,
	}
	for ip, want := range cases {
		info, ok := r.LookupASN(netip.MustParseAddr(ip))
		if !ok || info.ASN != want {
			t.Errorf("%s: got %+v ok=%v, want AS%d", ip, info, ok, want)
		}
	}
	if _, ok := r.LookupASN(netip.MustParseAddr("10.1.2.3")); ok {
		t.Error("private address should not resolve")
	}
	// Tata's routers are registered to its US entity: registry != location.
	if info, _ := r.LookupASN(netip.MustParseAddr("180.87.38.1")); info.RegistryCountry != "US" {
		t.Errorf("registry country = %q", info.RegistryCountry)
	}
}

func TestMatchHostname(t *testing.T) {
	r := NewSeededReference()
	cases := map[string]string{
		"if-ae-2-2.tcore1.mlv-mumbai.as6453.net":      "Mumbai",
		"if-be-2-2.ecore1.emrs2-marseille.as6453.net": "Marseille",
		"ae-1.r20.lsanca07.us.bb.gin.ntt.net":         "Los Angeles",
		"be3036.ccr22.lon13.atlas.cogentco.com":       "London",
		"100ge0-35.core2.fra1.he.net":                 "Frankfurt",
		"xe-0-0-1.cr1.los-angeles.example.net":        "Los Angeles",
		"ae-2-3602.ear2.frankfurt1.level3.net":        "Frankfurt",
	}
	for host, want := range cases {
		c, tok, ok := r.MatchHostname(host)
		if !ok || c.Name != want {
			t.Errorf("%s: got %q (token %q) ok=%v, want %s", host, c.Name, tok, ok, want)
		}
	}
	// No city token: public resolvers, and "tor" (top-of-rack) is excluded on purpose.
	for _, host := range []string{"dns.google", "one.one.one.one", "tor1.rack4.dc.example.com"} {
		if c, tok, ok := r.MatchHostname(host); ok {
			t.Errorf("%s: unexpected match %s via %q", host, c.Name, tok)
		}
	}
}

func TestCablesBetween(t *testing.T) {
	r := NewSeededReference()
	ids := map[string]bool{}
	for _, c := range r.CablesBetween("IN", "FR") {
		ids[c.ID] = true
		if c.Planned {
			t.Errorf("planned cable %s returned", c.ID)
		}
	}
	for _, want := range []string{"imewe", "seamewe-4", "2africa"} {
		if !ids[want] {
			t.Errorf("missing %s in IN<->FR cables: %v", want, ids)
		}
	}
	if ids["india-europe-xpress-iex"] {
		t.Error("planned IEX (RFS 2027) should be excluded")
	}
	if len(r.CablesBetween("US", "ES")) == 0 {
		t.Error("expected MAREA/Grace Hopper between US and ES")
	}
}

func TestRepository(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository(3)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		j := &model.Journey{JourneyID: fmt.Sprintf("j%d", i)}
		j.Metadata.Timestamp = base.Add(time.Duration(i) * time.Minute)
		j.Metadata.Destination.IP = "8.8.8.8"
		j.Compliance.Verdict = []string{"compliant", "violation"}[i%2]
		if err := repo.Save(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := repo.Count(ctx); n != 3 {
		t.Fatalf("count = %d, want 3 after eviction", n)
	}
	if _, err := repo.Get(ctx, "j0"); err != ErrNotFound {
		t.Fatalf("oldest journey should be evicted, err=%v", err)
	}
	got, _ := repo.Get(ctx, "j4")
	got.Metadata.Destination.IP = "mutated"
	again, _ := repo.Get(ctx, "j4")
	if again.Metadata.Destination.IP != "8.8.8.8" {
		t.Fatal("stored journey was mutated through a returned pointer")
	}
	list, _ := repo.List(ctx, JourneyFilter{Verdict: "compliant"})
	if len(list) != 2 || list[0].JourneyID != "j4" || list[1].JourneyID != "j2" {
		t.Fatalf("list newest-first by verdict: %v", ids(list))
	}
	list, _ = repo.List(ctx, JourneyFilter{Since: base.Add(3 * time.Minute)})
	if len(list) != 2 {
		t.Fatalf("since filter: %v", ids(list))
	}
}

func ids(js []*model.Journey) []string {
	var out []string
	for _, j := range js {
		out = append(out, j.JourneyID)
	}
	return out
}

// TestLoadDataDir loads the real research snapshot when the repo data/ dir is present.
func TestLoadDataDir(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "data")
	if _, err := os.Stat(filepath.Join(dir, "raw", "iptoasn")); err != nil {
		t.Skip("research data not present")
	}
	r := NewSeededReference()
	rep, err := LoadDataDir(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Warnings) > 0 {
		t.Fatalf("warnings: %v", rep.Warnings)
	}
	if rep.Loaded["asn_ranges"] < 400000 || rep.Loaded["cables"] < 600 || rep.Loaded["aws_prefixes"] < 1000 {
		t.Fatalf("unexpectedly small load: %v", rep.Loaded)
	}
	if g, ok := r.LookupGeo(netip.MustParseAddr("18.96.32.10")); !ok || g.CountryISO2 != "DE" {
		t.Fatalf("AWS eu-central-1 prefix geo = %+v ok=%v", g, ok)
	}
	if c, ok := r.Country("NO"); !ok || c.Continent != "Europe" {
		t.Fatalf("Norway (ISO_A2=-99 in Natural Earth) = %+v ok=%v", c, ok)
	}
	p, ok := r.Policy("eu-gdpr-adequacy")
	if !ok || !p.Allowed["BR"] || !p.Allowed["DE"] || p.Allowed["CN"] {
		t.Fatalf("eu-gdpr-adequacy expansion wrong: %v", p.Allowed)
	}
}
