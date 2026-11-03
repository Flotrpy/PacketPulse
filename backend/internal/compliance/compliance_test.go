package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flotrpy/packetpulse/backend/schema"
)

const ne = "../../../data/raw/naturalearth/"

var (
	locOnce sync.Once
	loc     *Locator
)

func locator(t *testing.T) *Locator {
	t.Helper()
	if _, err := os.Stat(ne + "ne_50m_admin_1_states_provinces.geojson.gz"); err != nil {
		t.Skip("Natural Earth data not present")
	}
	locOnce.Do(func() {
		var err error
		loc, err = LoadLocator(ne+"ne_50m_admin_0_countries.geojson.gz", ne+"ne_50m_admin_1_states_provinces.geojson.gz")
		if err != nil {
			t.Fatal(err)
		}
	})
	return loc
}

func engine(t *testing.T) (*Engine, *AlertStore) {
	t.Helper()
	reg, err := NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	store := NewAlertStore(100)
	return &Engine{Registry: reg, Locator: locator(t), Sink: store}, store
}

var t0 = time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)

func pt(seq int, kind, label string, lat, lon float64, ms float64) RoutePoint {
	return RoutePoint{Seq: seq, TTL: seq, Kind: kind, Label: label, Lat: lat, Lon: lon, LocationConfidence: 0.85, LocationSource: "rdns_hint",
		Timestamp: t0.Add(time.Duration(ms * float64(time.Millisecond))), TimestampSource: "probe_sent_plus_half_rtt"}
}

// Paris -> Frankfurt -> London -> Ashburn: a typical EU -> US path via LINX.
func parisToAshburn(class string) Packet {
	return Packet{PacketID: "pkt-eu-001", Timestamp: t0, Classifications: []string{class}, Route: []RoutePoint{
		pt(0, "source", "Paris", 48.8566, 2.3522, 0),
		pt(1, "hop", "Frankfurt", 50.1109, 8.6821, 2.6),
		pt(2, "hop", "London", 51.5074, -0.1278, 6.9),
		pt(3, "destination", "Ashburn", 39.0438, -77.4874, 38.4),
	}}
}

func TestLocatorAndRegions(t *testing.T) {
	l := locator(t)
	reg, _ := NewRegistry("")
	snap := reg.Snapshot()
	cases := []struct {
		name    string
		p       LatLon
		cc, sub string
		in, out []string
	}{
		{"Zurich", LatLon{47.3769, 8.5417}, "CH", "", []string{"schengen", "gdpr-adequate"}, []string{"eu", "eea"}},
		{"Dublin", LatLon{53.3498, -6.2603}, "IE", "", []string{"eu", "eea"}, []string{"schengen"}},
		{"Oslo", LatLon{59.9139, 10.7522}, "NO", "", []string{"eea", "schengen"}, []string{"eu"}},
		{"San Francisco", LatLon{37.7749, -122.4194}, "US", "US-CA", []string{"us", "us-ca"}, []string{"eea"}},
		{"Las Vegas", LatLon{36.1699, -115.1398}, "US", "US-NV", []string{"us"}, []string{"us-ca"}},
		{"Hong Kong", LatLon{22.3193, 114.1694}, "HK", "", []string{"us-doj-concern"}, []string{"cn-mainland"}},
	}
	for _, c := range cases {
		got := l.Locate(c.p, "")
		if got.CountryISO2 != c.cc || got.Subdivision != c.sub {
			t.Errorf("%s: %+v", c.name, got)
		}
		rs := snap.RegionsAt(got, c.p)
		for _, r := range c.in {
			if !slices.Contains(rs, r) {
				t.Errorf("%s should be in %s (%v)", c.name, r, rs)
			}
		}
		for _, r := range c.out {
			if slices.Contains(rs, r) {
				t.Errorf("%s should not be in %s", c.name, r)
			}
		}
	}
	// Mid-Atlantic is nobody's territory; a landing station a little offshore snaps to the coast.
	if got := l.Locate(LatLon{40, -40}, ""); got.CountryISO2 != "" {
		t.Errorf("mid-Atlantic located in %s", got.CountryISO2)
	}
	if got := l.Locate(LatLon{40, -40}, "US"); got.Source != "hint" {
		t.Errorf("hint not used: %+v", got)
	}
}

func TestGDPRStrictRaisesAlertsOnExit(t *testing.T) {
	e, store := engine(t)
	ev, err := e.Evaluate(context.Background(), parisToAshburn("sensitive_eu_citizen_data"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Verdict != "violation" || len(ev.Alerts) != 2 {
		t.Fatalf("verdict %s, %d alerts: %+v", ev.Verdict, len(ev.Alerts), ev.Alerts)
	}
	gb, us := ev.Alerts[0], ev.Alerts[1]
	if gb.Territory.CountryISO2 != "GB" || us.Territory.CountryISO2 != "US" {
		t.Fatalf("territories %s, %s", gb.Territory.CountryISO2, us.Territory.CountryISO2)
	}
	// Exact entry time = the hop's timestamp (probe sent + RTT/2).
	if !gb.EnteredAt.Equal(t0.Add(6900*time.Microsecond)) || gb.EntryTimeSource != "probe_sent_plus_half_rtt" {
		t.Errorf("entered_at %s (%s)", gb.EnteredAt, gb.EntryTimeSource)
	}
	if gb.Law.Code != "GDPR" || !strings.Contains(gb.Law.Citation, "Chapter V") || gb.Severity != "critical" || gb.ProfileID != "gdpr-ch5-strict" {
		t.Errorf("law/severity %+v %s", gb.Law, gb.Severity)
	}
	if gb.LastCompliant == nil || gb.LastCompliant.Label != "Frankfurt" {
		t.Errorf("last compliant %+v", gb.LastCompliant)
	}
	// Nearest EEA proxy to the last compliant point (Frankfurt) is AWS eu-central-1.
	m := gb.Mitigation
	if m.Action != "reroute" || m.Proxy == nil || m.Proxy.ID != "aws-eu-central-1" || !strings.Contains(m.Recommendation, "Reroute via EU-isolated AWS eu-central-1 proxy") || !strings.Contains(m.Recommendation, "United Kingdom") {
		t.Errorf("mitigation %+v", m)
	}
	for _, a := range ev.Alerts {
		if err := schema.ValidateComplianceAlert(a); err != nil {
			t.Fatalf("alert schema: %v", err)
		}
	}
	if got := store.List(AlertFilter{PacketID: "pkt-eu-001"}); len(got) != 2 {
		t.Fatalf("sink received %d alerts", len(got))
	}
	// Re-entering a territory after leaving it raises a new alert.
	p := parisToAshburn("sensitive_eu_citizen_data")
	p.Route = append(p.Route[:3], pt(3, "hop", "Amsterdam", 52.3676, 4.9041, 9), pt(4, "destination", "London", 51.5074, -0.1278, 12))
	ev, _ = e.Evaluate(context.Background(), p)
	if len(ev.Alerts) != 2 || ev.Alerts[1].Territory.CountryISO2 != "GB" || ev.Alerts[1].LastCompliant.Label != "Amsterdam" {
		t.Fatalf("re-entry: %+v", ev.Alerts)
	}

	if os.Getenv("UPDATE_EXAMPLES") == "1" {
		ev.EvaluatedAt = t0.Add(time.Second)
		ev, _ = e.Evaluate(context.Background(), parisToAshburn("sensitive_eu_citizen_data"))
		b, _ := json.MarshalIndent(ev, "", "  ")
		os.WriteFile("../../schema/examples/gdpr-strict-paris-to-ashburn.evaluation.json", append(b, '\n'), 0o644)
	}
}

func TestAdequacyProfileDowngradesUS(t *testing.T) {
	e, _ := engine(t)
	ev, _ := e.Evaluate(context.Background(), parisToAshburn("eu_personal_data"))
	if len(ev.Alerts) != 1 {
		t.Fatalf("GB is adequate; only the US hop should alert: %+v", ev.Alerts)
	}
	a := ev.Alerts[0]
	if a.Territory.CountryISO2 != "US" || a.Severity != "medium" || !strings.Contains(a.Exception, "Data Privacy Framework") {
		t.Fatalf("%+v", a)
	}
}

func TestDestinationOnlyScope(t *testing.T) {
	e, _ := engine(t)
	p := Packet{PacketID: "pay-1", Timestamp: t0, Classifications: []string{"payment_system_data"}, Route: []RoutePoint{
		pt(0, "source", "Mumbai", 19.076, 72.8777, 0),
		pt(1, "hop", "Marseille", 43.2965, 5.3698, 56),
		pt(2, "destination", "Frankfurt", 50.1109, 8.6821, 64),
	}}
	ev, _ := e.Evaluate(context.Background(), p)
	if len(ev.Alerts) != 1 || ev.Alerts[0].Territory.CountryISO2 != "DE" || ev.Alerts[0].LastCompliant.Label != "Mumbai" {
		t.Fatalf("RBI storage rule should only flag the destination: %+v", ev.Alerts)
	}
	if px := ev.Alerts[0].Mitigation.Proxy; px == nil || px.Region != "ap-south-1" {
		t.Fatalf("proxy %+v", px)
	}
}

func TestDenyListAndCCPA(t *testing.T) {
	e, _ := engine(t)
	via := func(class string, mid RoutePoint) *Evaluation {
		ev, err := e.Evaluate(context.Background(), Packet{PacketID: "p", Timestamp: t0, Classifications: []string{class},
			Route: []RoutePoint{pt(0, "source", "Los Angeles", 34.0522, -118.2437, 0), mid, pt(2, "destination", "Singapore", 1.3521, 103.8198, 180)}})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	if ev := via("us_bulk_sensitive_data", pt(1, "hop", "Hong Kong", 22.3193, 114.1694, 150)); len(ev.Alerts) != 1 || ev.Alerts[0].Territory.CountryISO2 != "HK" || ev.Alerts[0].Mitigation.Action != "block" {
		t.Fatalf("DOJ via HK: %+v", ev.Alerts)
	}
	if ev := via("us_bulk_sensitive_data", pt(1, "hop", "Tokyo", 35.6762, 139.6503, 110)); ev.Verdict != "compliant" {
		t.Fatalf("DOJ via JP: %+v", ev.Alerts)
	}
	ev, _ := e.Evaluate(context.Background(), Packet{PacketID: "ca", Timestamp: t0, Classifications: []string{"California Consumer Data"},
		Route: []RoutePoint{pt(0, "source", "San Francisco", 37.7749, -122.4194, 0), pt(1, "destination", "Las Vegas", 36.1699, -115.1398, 9)}})
	if len(ev.Alerts) != 1 || ev.Alerts[0].Territory.Subdivision != "US-NV" || ev.Alerts[0].Severity != "info" || ev.Alerts[0].Mitigation.Proxy.Region != "us-west-1" {
		t.Fatalf("CCPA state line: %+v", ev.Alerts)
	}
}

func TestLowConfidenceIsSuspected(t *testing.T) {
	e, _ := engine(t)
	p := parisToAshburn("sensitive_eu_citizen_data")
	p.Route[2].LocationConfidence = 0.2
	ev, _ := e.Evaluate(context.Background(), p)
	if !ev.Alerts[0].Suspected || ev.Alerts[0].Severity != "high" {
		t.Fatalf("%+v", ev.Alerts[0])
	}
	if ev.Verdict != "violation" { // the US hop is still confident
		t.Fatalf("verdict %s", ev.Verdict)
	}
}

func TestBadPackets(t *testing.T) {
	e, _ := engine(t)
	for _, p := range []Packet{
		{},
		{PacketID: "x", Classifications: []string{"a"}},
		{PacketID: "x", Classifications: []string{"a"}, Route: []RoutePoint{{Lat: 91}}},
	} {
		if _, err := e.Evaluate(context.Background(), p); !errors.Is(err, ErrBadPacket) {
			t.Errorf("%+v: %v", p, err)
		}
	}
	ev, _ := e.Evaluate(context.Background(), Packet{PacketID: "x", Classifications: []string{"public"}, Route: []RoutePoint{pt(0, "source", "x", 0, 0, 0)}})
	if ev.Verdict != "not_applicable" {
		t.Fatalf("unknown class verdict %s", ev.Verdict)
	}
}

func TestHotUpdatesPersistenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Registry: reg, Locator: locator(t)}
	ctx := context.Background()

	// An admin draws a custom "EU sovereign zone" polygon around Germany and France only.
	poly := json.RawMessage(`[[[-5.5,42.0],[15.5,42.0],[15.5,55.5],[-5.5,55.5],[-5.5,42.0]]]`)
	if _, err := reg.PutRegion(Region{ID: "fr-de-zone", Name: "FR+DE sovereign zone", Polygon: &Geometry{Type: "Polygon", Coordinates: poly}}, "", "alice"); err != nil {
		t.Fatal(err)
	}
	prof := reg.Snapshot().Profiles["gdpr-ch5-strict"]
	prof.Regions = []string{"fr-de-zone"}
	// Stale ETag is refused.
	if _, err := reg.PutProfile(prof, ETag(prof.ID, prof.Version+5), "alice"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale If-Match: %v", err)
	}
	before := reg.Snapshot().Seq
	saved, err := reg.PutProfile(prof, ETag(prof.ID, prof.Version), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 || reg.Snapshot().Seq != before+1 {
		t.Fatalf("version %d seq %d", saved.Version, reg.Snapshot().Seq)
	}
	// Next evaluation uses the new boundary: a Madrid hop is now outside.
	p := Packet{PacketID: "z", Timestamp: t0, Classifications: []string{"sensitive_eu_citizen_data"}, Route: []RoutePoint{
		pt(0, "source", "Paris", 48.8566, 2.3522, 0), pt(1, "destination", "Madrid", 40.4168, -3.7038, 9)}}
	ev, _ := e.Evaluate(ctx, p)
	if len(ev.Alerts) != 1 || ev.Alerts[0].Territory.CountryISO2 != "ES" || ev.Alerts[0].ProfileVersion != 2 {
		t.Fatalf("custom zone: %+v", ev.Alerts)
	}

	// Invalid definitions and in-use deletes are rejected without changing anything.
	bad := prof
	bad.Regions = []string{"atlantis"}
	if _, err := reg.PutProfile(bad, "", "alice"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown region: %v", err)
	}
	if err := reg.DeleteRegion("fr-de-zone", "", "alice"); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete in-use region: %v", err)
	}
	if _, err := reg.PutRegion(Region{ID: "x1", Name: "x", Countries: []string{"France"}}, "", "alice"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad country code: %v", err)
	}

	// Persisted: a new process sees the change and the audit trail.
	reg2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg2.Snapshot().Profiles["gdpr-ch5-strict"]; got.Version != 2 || got.UpdatedBy != "alice" || !slices.Equal(got.Regions, []string{"fr-de-zone"}) {
		t.Fatalf("reloaded %+v", got)
	}
	if a := reg2.Audit(10); len(a) != 2 || a[0].Kind != "profile" || a[1].Kind != "region" {
		t.Fatalf("audit %+v", a)
	}
}

// Evaluations run lock-free on snapshots while admins update profiles.
func TestConcurrentEvaluateAndUpdate(t *testing.T) {
	e, _ := engine(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ev, err := e.Evaluate(ctx, parisToAshburn("sensitive_eu_citizen_data"))
				if err != nil {
					t.Error(err)
					return
				}
				// Either the old (2 alerts) or the new profile (disabled: 0) - never a mix.
				if n := len(ev.Alerts); n != 0 && n != 2 {
					t.Errorf("torn snapshot: %d alerts", n)
					return
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		p := e.Registry.Snapshot().Profiles["gdpr-ch5-strict"]
		p.Enabled = !p.Enabled
		if _, err := e.Registry.PutProfile(p, "", "bob"); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// Country-level profiles ignore internal state lines: Mumbai (Maharashtra) ->
// Ahmedabad (Gujarat) is one entry into India, not two.
func TestCountryProfilesIgnoreSubdivisions(t *testing.T) {
	e, _ := engine(t)
	ev, _ := e.Evaluate(context.Background(), Packet{PacketID: "in", Timestamp: t0, Classifications: []string{"sensitive_eu_citizen_data"}, Route: []RoutePoint{
		pt(0, "source", "Mumbai", 19.076, 72.8777, 0), pt(1, "hop", "Ahmedabad", 23.0225, 72.5714, 8), pt(2, "destination", "Frankfurt", 50.1109, 8.6821, 120)}})
	if len(ev.Alerts) != 1 || ev.Alerts[0].Territory.Subdivision != "" || strings.Contains(ev.Alerts[0].Mitigation.Recommendation, "Maharashtra") {
		t.Fatalf("%+v", ev.Alerts)
	}
	if ev.Route[1].Status != "violation" || ev.Route[1].Location.Subdivision != "IN-GJ" {
		t.Fatalf("route point still carries the subdivision for display: %+v", ev.Route[1])
	}
}

// A path whose only issue is an exception-downgraded territory (US under the
// adequacy profile) is a suspected violation, not a confirmed one.
func TestExceptionOnlyPathIsSuspected(t *testing.T) {
	e, _ := engine(t)
	ev, _ := e.Evaluate(context.Background(), parisToAshburn("eu_personal_data"))
	if ev.Verdict != "suspected_violation" || ev.Route[3].Status != "suspected" {
		t.Fatalf("verdict %s, US point %s", ev.Verdict, ev.Route[3].Status)
	}
}
