package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/store"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
	"github.com/flotrpy/packetpulse/backend/schema"
)

// fakeDNS answers PTR lookups from a fixed table (real Tata hostnames,
// geolocated by the CAIDA Hoiho API during research).
type fakeDNS map[string]string

func (f fakeDNS) LookupAddr(_ context.Context, addr string) ([]string, error) {
	if n, ok := f[addr]; ok {
		return []string{n + "."}, nil
	}
	return nil, errors.New("no PTR")
}

var mumbaiDNS = fakeDNS{
	"180.87.38.1":  "if-ae-2-2.tcore1.mlv-mumbai.as6453.net",
	"80.231.200.1": "if-be-2-2.ecore1.emrs2-marseille.as6453.net",
	"80.231.153.1": "if-ae-11-2.tcore1.pvu-paris.as6453.net",
}

type hopSpec struct {
	ttl  int
	from string // "" = no answer
	rtts []float64
	kind traceroute.ReplyKind
}

func buildTrace(target string, hops []hopSpec) *traceroute.RawTrace {
	t0 := time.Date(2026, 11, 2, 13, 5, 1, 0, time.UTC)
	opts := traceroute.Options{Protocol: traceroute.ICMP, MaxHops: 30, ProbesPerHop: 3}.WithDefaults()
	rt := &traceroute.RawTrace{Target: netip.MustParseAddr(target), Source: netip.MustParseAddr("100.72.14.9"), Options: opts, StartedAt: t0, FinishedAt: t0.Add(4 * time.Second)}
	for _, h := range hops {
		for a := 0; a < 3; a++ {
			p := traceroute.ProbeResult{TTL: h.ttl, Attempt: a, SentAt: t0}
			if h.from != "" && a < len(h.rtts) {
				p.Responded, p.From, p.Kind = true, netip.MustParseAddr(h.from), h.kind
				p.RTT = time.Duration(h.rtts[a] * float64(time.Millisecond))
			}
			rt.Probes = append(rt.Probes, p)
		}
		if h.kind.FromTarget() {
			rt.Reached, rt.DestinationTTL = true, h.ttl
		}
	}
	return rt
}

const te, echo = traceroute.ReplyTimeExceeded, traceroute.ReplyEcho

// Jio 5G (Mumbai) -> Tata AS6453 -> subsea -> Marseille -> Paris -> AWS Frankfurt.
func mumbaiTrace() *traceroute.RawTrace {
	return buildTrace("18.96.32.10", []hopSpec{
		{1, "100.72.0.1", []float64{9.8, 14.2, 21.0}, te},
		{2, "", nil, 0},
		{3, "49.36.0.1", []float64{18.1, 19.4, 21.2}, te},
		{4, "180.87.38.1", []float64{19.6, 20.3, 21.4}, te},
		{5, "80.231.200.1", []float64{112.4, 113.9, 118.8}, te},
		{6, "80.231.153.1", []float64{121.0, 128.7, 141.5}, te},
		{7, "52.93.0.1", []float64{127.9, 129.2, 131.0}, te},
		{8, "18.96.32.10", []float64{128.6, 129.9, 132.2}, echo},
	})
}

var mumbai = &model.Location{Lat: 19.0596, Lon: 72.8295, City: "Mumbai", CountryISO2: "IN", AccuracyKM: 5, Method: "configured", Confidence: 1}

func run(t *testing.T, raw *traceroute.RawTrace, req Request) (*model.Journey, *store.MemoryRepository) {
	t.Helper()
	repo := store.NewMemoryRepository(10)
	j, err := Default(store.NewSeededReference(), repo, mumbaiDNS).Run(context.Background(), raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(j); err != nil {
		t.Fatalf("journey violates schema: %v", err)
	}
	return j, repo
}

func TestMumbaiToFrankfurt(t *testing.T) {
	j, repo := run(t, mumbaiTrace(), Request{Target: "18.96.32.10", PolicyID: "in-rbi-payments", AgentID: "test", AccessType: "cellular", SourceLocation: mumbai})

	// --- hop hierarchy ---
	if len(j.Hops) != 8 || !j.Metadata.ReachedDestination {
		t.Fatalf("hops=%d reached=%v", len(j.Hops), j.Metadata.ReachedDestination)
	}
	h := j.Hops
	expectRole := map[int]string{1: "cgnat", 3: "mno_gateway", 4: "backbone", 5: "backbone", 7: "cloud_edge", 8: "destination"}
	for n, role := range expectRole {
		if h[n-1].Role != role {
			t.Errorf("hop %d role = %s, want %s", n, h[n-1].Role, role)
		}
	}
	if h[3].ASN != 6453 || h[3].RegistryCountry != "US" || h[3].Location.CountryISO2 != "IN" || h[3].Location.Method != "rdns_hint" {
		t.Errorf("hop 4 = %+v loc=%+v", h[3], h[3].Location)
	}
	if h[4].Location.City != "Marseille" || h[5].Location.City != "Paris" {
		t.Errorf("hop 5/6 cities = %s/%s", h[4].Location.City, h[5].Location.City)
	}
	if h[1].Responded || h[1].LossPct != 100 {
		t.Errorf("hop 2 should be silent: %+v", h[1])
	}
	if r := h[0].RTT; r.Min != 9.8 || r.Max != 21 || r.Jitter == 0 {
		t.Errorf("hop 1 rtt stats = %+v", r)
	}
	// AWS router with no geo evidence falls back to the registry country
	// (US). That is the address holder, not the router's location, so it is
	// low confidence and must not count as a country traversed.
	if l := h[6].Location; l == nil || l.Method != "registry_country" || l.Confidence >= 0.5 {
		t.Errorf("hop 7 location = %+v", l)
	}

	// --- infrastructure layer ---
	segs := j.Infrastructure.Segments
	byTo := map[int]model.Segment{}
	for _, s := range segs {
		byTo[s.ToHop] = s
	}
	if s := byTo[3]; s.Medium != model.MediumCellularBackhaul || s.Cellular == nil || s.Cellular.ASN != 55836 || !s.Cellular.CGNAT || s.Cellular.GatewayIP != "49.36.0.1" {
		t.Errorf("cellular segment = %+v cell=%+v", s, s.Cellular)
	}
	if s := byTo[4]; s.Medium != model.MediumMetroFiber || s.TerrestrialISP == nil || s.TerrestrialISP.ASN != 6453 {
		t.Errorf("Navi Mumbai -> Mumbai segment = %+v", s)
	}
	sub := byTo[5]
	if sub.Medium != model.MediumSubseaCable || sub.SubseaCable == nil {
		t.Fatalf("Mumbai -> Marseille segment = %+v", sub)
	}
	if !slices.Contains([]string{"imewe", "seamewe-4"}, sub.SubseaCable.CableID) || !sub.SubseaCable.OwnerMatch {
		t.Errorf("top cable = %+v (want a Tata-owned Mumbai-Marseille cable)", sub.SubseaCable)
	}
	if sub.SubseaCable.LandingA != "mumbai-india" || sub.SubseaCable.LandingB != "marseille-france" {
		t.Errorf("landings = %s -> %s", sub.SubseaCable.LandingA, sub.SubseaCable.LandingB)
	}
	if sub.DistanceKM < 6500 || sub.DistanceKM > 7000 || sub.MinFeasibleRTTMS < 60 {
		t.Errorf("distance=%v minRTT=%v", sub.DistanceKM, sub.MinFeasibleRTTMS)
	}
	if s := byTo[6]; s.Medium != model.MediumTerrestrialFiber || s.TerrestrialISP.ASN != 6453 {
		t.Errorf("Marseille -> Paris segment = %+v", s)
	}
	// Hop 7 has only a registry guess: Paris -> hop 7 must stay unknown,
	// never a transatlantic cable.
	if s := byTo[7]; s.Medium != model.MediumUnknown || s.SubseaCable != nil {
		t.Errorf("Paris -> unlocated AWS edge = %+v", s)
	}
	if s := byTo[8]; s.Medium != model.MediumCloudBackbone {
		t.Errorf("AWS edge -> destination = %+v", s)
	}
	if slices.Contains(j.Compliance.PossibleTransit, "US") {
		t.Errorf("possible transit must not include US: %v", j.Compliance.PossibleTransit)
	}
	if !slices.Contains(j.Infrastructure.MediaSummary, model.MediumSubseaCable) {
		t.Errorf("media summary = %v", j.Infrastructure.MediaSummary)
	}

	// --- compliance flags ---
	c := j.Compliance
	f := c.Flags
	if !c.Evaluated || c.Verdict != "violation" || !f.BoundaryViolation || !f.DestinationOutsideRegion || !f.LeftAllowedRegion || !f.InternationalSubseaCrossing {
		t.Errorf("compliance = %+v", c)
	}
	if f.TransitedRestrictedCountry {
		t.Error("no restricted countries under in-rbi-payments")
	}
	if !slices.Equal(c.CountriesTraversed, []string{"IN", "FR", "DE"}) {
		t.Errorf("countries = %v", c.CountriesTraversed)
	}
	if !h[4].BorderViolation || h[3].BorderViolation || !h[7].BorderViolation {
		t.Errorf("per-hop border flags: hop4=%v hop5=%v hop8=%v", h[3].BorderViolation, h[4].BorderViolation, h[7].BorderViolation)
	}
	if !slices.Contains(c.PossibleTransit, "EG") {
		t.Errorf("possible transit should include Egypt (Suez crossing): %v", c.PossibleTransit)
	}

	// --- persisted, with a full audit trail ---
	stored, err := repo.Get(context.Background(), j.JourneyID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Pipeline.Stages) != 5 || stored.Pipeline.Stages[4].Name != "persist" {
		t.Errorf("stored stages = %+v", stored.Pipeline.Stages)
	}
	for _, s := range j.Pipeline.Stages {
		if s.Error != "" {
			t.Errorf("stage %s error: %s", s.Name, s.Error)
		}
	}
}

func TestPolicyVariants(t *testing.T) {
	cases := []struct {
		policy, verdict string
		restricted      bool
	}{
		{"eu-gdpr-strict", "risk", false},         // leaves EEA in transit (IN), ends in DE: allowed destination
		{"us-doj-bulk", "compliant", false},       // no country of concern on the path
		{"cn-important-data", "violation", false}, // transit counts as transfer
		{"", "not_evaluated", false},
	}
	for _, tc := range cases {
		j, _ := run(t, mumbaiTrace(), Request{PolicyID: tc.policy, AccessType: "cellular", SourceLocation: mumbai})
		if j.Compliance.Verdict != tc.verdict || j.Compliance.Flags.TransitedRestrictedCountry != tc.restricted {
			t.Errorf("%s: verdict=%s flags=%+v", tc.policy, j.Compliance.Verdict, j.Compliance.Flags)
		}
	}
}

// A hostname hint that is physically impossible given the RTT is dropped.
func TestRTTFeasibilityRejectsImpossibleHint(t *testing.T) {
	raw := buildTrace("8.8.8.8", []hopSpec{
		{1, "180.87.38.1", []float64{2.0, 2.1, 2.2}, te},
		{2, "8.8.8.8", []float64{3.0, 3.1, 3.2}, echo},
	})
	dns := fakeDNS{"180.87.38.1": "if-ae-1.tcore1.ldn-london.as6453.net"} // London, 7,200 km from Mumbai
	j, err := Default(store.NewSeededReference(), store.NewMemoryRepository(1), dns).Run(context.Background(), raw,
		Request{SourceLocation: mumbai, PolicyID: "in-rbi-payments"})
	if err != nil {
		t.Fatal(err)
	}
	if l := j.Hops[0].Location; l != nil && l.City == "London" {
		t.Fatalf("London hint accepted with 2 ms RTT from Mumbai: %+v", l)
	}
	if j.Compliance.Verdict != "inconclusive" {
		t.Fatalf("unplaceable path should be inconclusive, got %s", j.Compliance.Verdict)
	}
}

// Transatlantic: US -> ES must be subsea even without hostname hints thanks
// to the continent rule.
func TestContinentRuleForcesSubsea(t *testing.T) {
	ref := store.NewSeededReference()
	ref.AddGeoPrefix(store.GeoHit{Prefix: netip.MustParsePrefix("4.69.0.0/16"), City: "Virginia Beach", CountryISO2: "US", Lat: 36.8529, Lon: -75.978, AccuracyKM: 20})
	ref.AddGeoPrefix(store.GeoHit{Prefix: netip.MustParsePrefix("62.115.0.0/16"), City: "Bilbao", CountryISO2: "ES", Lat: 43.263, Lon: -2.935, AccuracyKM: 20})
	raw := buildTrace("62.115.0.1", []hopSpec{
		{1, "4.69.140.98", []float64{1.0, 1.1, 1.2}, te},
		{2, "62.115.0.1", []float64{75.0, 76.0, 77.0}, echo},
	})
	src := &model.Location{Lat: 36.85, Lon: -75.98, CountryISO2: "US", AccuracyKM: 5, Method: "configured", Confidence: 1}
	j, err := Default(ref, store.NewMemoryRepository(1), nil).Run(context.Background(), raw, Request{SourceLocation: src})
	if err != nil {
		t.Fatal(err)
	}
	seg := j.Infrastructure.Segments[len(j.Infrastructure.Segments)-1]
	if seg.Medium != model.MediumSubseaCable || seg.SubseaCable == nil {
		t.Fatalf("segment = %+v", seg)
	}
	if !slices.Contains([]string{"marea", "grace-hopper", "amitie"}, seg.SubseaCable.CableID) {
		t.Fatalf("cable = %s", seg.SubseaCable.CableID)
	}
	if err := schema.Validate(j); err != nil {
		t.Fatal(err)
	}
}

func TestRTTStats(t *testing.T) {
	st := rttStats([]float64{10, 14, 12})
	if st.Min != 10 || st.Max != 14 || st.Avg != 12 || st.Jitter != 3 || st.StdDev != 1.633 {
		t.Fatalf("%+v", st)
	}
	if st := rttStats(nil); st.Samples == nil {
		t.Fatal("samples must be [] not null")
	}
}

// TestExampleUpToDate regenerates schema/examples/ with UPDATE_EXAMPLES=1 and
// otherwise checks the committed example still validates.
func TestExampleUpToDate(t *testing.T) {
	path := filepath.Join("..", "..", "schema", "examples", "mumbai-jio-to-aws-frankfurt.journey.json")
	if os.Getenv("UPDATE_EXAMPLES") == "1" {
		j, _ := run(t, mumbaiTrace(), Request{Target: "18.96.32.10", PolicyID: "in-rbi-payments", AgentID: "android-7f3a", AccessType: "cellular", SourceLocation: mumbai})
		b, err := json.MarshalIndent(j, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var j model.Journey
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(&j); err != nil {
		t.Fatalf("committed example violates schema: %v", err)
	}
}
