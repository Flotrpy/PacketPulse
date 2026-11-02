package enrich

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/geoip"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
	"github.com/flotrpy/packetpulse/backend/schema"
)

const dataDir = "../../../data"

var (
	buildOnce sync.Once
	shared    *Enricher
	buildErr  error
)

// enricher is built once: loading PeeringDB takes a few seconds.
func enricher(t *testing.T) *Enricher {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dataDir, "raw", "peeringdb")); err != nil {
		t.Skip("research data not present")
	}
	buildOnce.Do(func() {
		shared, _, buildErr = Build(Config{
			DataDir:         dataDir,
			StaticGeoIP:     "testdata/geoip_dbip_2026-11.recorded.json", // real DB-IP Lite answers, recorded with -record-geoip
			RouterHostnames: true,
		})
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return shared
}

func readTrace(t *testing.T, path string) *traceroute.RawTrace {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rt traceroute.RawTrace
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	return &rt
}

func mumbaiInput(t *testing.T) Input {
	var ptr map[string]string
	b, _ := os.ReadFile("testdata/mumbai_jio.hostnames.json")
	json.Unmarshal(b, &ptr)
	return Input{
		Trace:     readTrace(t, "testdata/mumbai_jio.rawtrace.json"),
		Hostnames: ptr,
		Device:    &Device{Lat: 19.0596, Lon: 72.8295, HasFix: true, AccuracyKM: 1, Country: "IN", AccessType: "cellular", MCC: "405", MNC: "874"},
	}
}

func TestMumbaiJioToAWSFrankfurt(t *testing.T) {
	e := enricher(t)
	m, err := e.Enrich(context.Background(), mumbaiInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateInfraMap(m); err != nil {
		t.Fatalf("schema: %v", err)
	}

	// 1. IP -> location/ASN (DB-IP via the MaxMind reader, recorded).
	hop := func(ttl int) HopEnriched { return m.Hops[ttl-1] }
	if h := hop(5); h.ASN != 6453 || h.Geo.City != "Marseille" || h.LocationStatus != "accepted" {
		t.Errorf("hop 5 %+v", h)
	}
	if h := hop(1); h.AddressClass != "cgnat" || h.Role != "cgnat" || h.LocationStatus != "non_routable" {
		t.Errorf("hop 1 %+v", h)
	}
	// AWS edge geolocated to Dublin: a 1,391 km detour the 7.6 ms RTT gap can't hold.
	if h := hop(7); h.LocationStatus != "rejected_detour" || h.Role != "cloud_edge" {
		t.Errorf("hop 7 %+v", h)
	}

	// 3. Mobile first leg.
	mob := m.Mobile
	if mob == nil || mob.Operator.ASN != 55836 || mob.GatewayTTL != 3 || !slices.Equal(mob.CGNATTTLs, []int{1}) {
		t.Fatalf("mobile %+v", mob)
	}
	if mob.GatewayHub == nil || mob.GatewayHub.City != "Mumbai" || mob.HubAnchor != "device_location" || mob.HubDistanceKM > 30 {
		t.Fatalf("hub %+v (%s, %.0f km)", mob.GatewayHub, mob.HubAnchor, mob.HubDistanceKM)
	}
	if g := hop(3); g.Geo.Sources["location"] != "mno_gateway_hub" || g.LocationNote == "" {
		t.Errorf("gateway should be placed at the hub with a note: %+v", g)
	}
	if m.Links[0].Medium != "cellular_backhaul" || m.Links[0].Cellular.ASN != 55836 || !slices.Contains(mob.LinkIndexes, 0) {
		t.Errorf("first link %+v", m.Links[0])
	}

	// 2. Ocean crossing -> cable.
	var sub *LinkEnriched
	for i := range m.Links {
		if m.Links[i].Medium == "subsea_cable" {
			sub = &m.Links[i]
		}
	}
	if sub == nil || sub.From.Label != "Mumbai" || sub.To.Label != "Marseille" || !sub.Ocean.CrossesOcean {
		t.Fatalf("subsea link %+v", sub)
	}
	sel := sub.Subsea.Selected
	if sel == nil || !slices.Contains([]string{"seamewe-4", "imewe"}, sel.CableID) || !sel.OwnerMatch || len(sel.Route) < 5 {
		t.Fatalf("selected cable %+v", sel)
	}
	if m.Summary.OceanCrossings != 1 || !slices.Equal(m.Summary.CountriesTraversed[:2], []string{"IN", "SA"}) || !slices.Contains(m.Summary.CountriesTraversed, "EG") {
		t.Errorf("summary %+v", m.Summary)
	}
	for _, l := range m.Links {
		if l.From.Label == "Marseille" && l.Medium != "terrestrial_fiber" {
			t.Errorf("Marseille->Paris %s", l.Medium)
		}
	}

	// 4. Mapping array covers every responding hop and every link, in path order.
	links, hops := 0, 0
	for i, me := range m.Mapping {
		if me.Seq != i+1 {
			t.Fatal("mapping not sequential")
		}
		switch me.Network.Kind {
		case "link":
			links++
		case "hop":
			hops++
		}
	}
	if links != len(m.Links) || hops != 7 {
		t.Errorf("mapping has %d links (want %d) and %d hops (want 7)", links, len(m.Links), hops)
	}

	if os.Getenv("UPDATE_EXAMPLES") == "1" {
		m.MapID = "4f1c2b9e-8d3a-4e57-9b61-2a7c5d0e9f13" // stable id for the committed example
		m.GeneratedAt = time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
		b, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile("../../schema/examples/mumbai-jio-to-aws-frankfurt.inframap.json", append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommittedExampleValidates(t *testing.T) {
	b, err := os.ReadFile("../../schema/examples/mumbai-jio-to-aws-frankfurt.inframap.json")
	if err != nil {
		t.Skip("example not generated")
	}
	var m InfraMap
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateInfraMap(&m); err != nil {
		t.Fatal(err)
	}
}

// A wired device in Virginia Beach reaching Bilbao: subsea (MAREA lands at
// both), and no cellular leg because Lumen/Arelion are not MNOs.
func TestTransatlanticWired(t *testing.T) {
	base := enricher(t)
	e := *base
	e.GeoIP = &geoip.Static{Label: "test", Records: map[netip.Addr]geoip.Record{
		netip.MustParseAddr("4.69.140.98"): {IP: "4.69.140.98", HasLocation: true, Lat: 36.8529, Lon: -75.978, AccuracyKM: 20, City: "Virginia Beach", CountryISO2: "US", ASN: 3356, ASOrg: "Level 3 Parent, LLC"},
		netip.MustParseAddr("62.115.0.1"):  {IP: "62.115.0.1", HasLocation: true, Lat: 43.263, Lon: -2.935, AccuracyKM: 20, City: "Bilbao", CountryISO2: "ES", ASN: 1299, ASOrg: "Arelion Sweden AB"},
	}}
	t0 := time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)
	mk := func(ttl int, ip string, ms float64, k traceroute.ReplyKind) traceroute.ProbeResult {
		return traceroute.ProbeResult{TTL: ttl, SentAt: t0, Responded: true, From: netip.MustParseAddr(ip), RTT: time.Duration(ms * float64(time.Millisecond)), Kind: k}
	}
	rt := &traceroute.RawTrace{Target: netip.MustParseAddr("62.115.0.1"), Options: traceroute.Options{Protocol: traceroute.ICMP, FirstTTL: 1}, StartedAt: t0, FinishedAt: t0,
		Reached: true, DestinationTTL: 3, Probes: []traceroute.ProbeResult{
			mk(1, "192.168.1.1", 0.6, traceroute.ReplyTimeExceeded),
			mk(2, "4.69.140.98", 1.4, traceroute.ReplyTimeExceeded),
			mk(3, "62.115.0.1", 66.2, traceroute.ReplyEcho),
		}}
	m, err := e.Enrich(context.Background(), Input{Trace: rt, Device: &Device{Lat: 36.85, Lon: -75.98, HasFix: true, AccuracyKM: 2, AccessType: "wired"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateInfraMap(m); err != nil {
		t.Fatal(err)
	}
	if m.Mobile != nil {
		t.Fatalf("wired path flagged cellular: %+v", m.Mobile)
	}
	last := m.Links[len(m.Links)-1]
	if last.Medium != "subsea_cable" || last.Subsea.Selected == nil || last.Subsea.Selected.CableID != "marea" {
		t.Fatalf("VB->Bilbao %+v / %+v", last, last.Subsea)
	}
	if m.Links[0].Medium != "metro_fiber" {
		t.Errorf("device->VB router %s", m.Links[0].Medium)
	}
}

// A GeoIP answer farther than light could travel in the hop's RTT is rejected.
func TestLightSpeedRejection(t *testing.T) {
	base := enricher(t)
	e := *base
	e.GeoIP = &geoip.Static{Records: map[netip.Addr]geoip.Record{
		netip.MustParseAddr("80.231.153.1"): {IP: "80.231.153.1", HasLocation: true, Lat: 48.86, Lon: 2.35, AccuracyKM: 50, City: "Paris", CountryISO2: "FR", ASN: 6453},
	}}
	t0 := time.Now()
	rt := &traceroute.RawTrace{Target: netip.MustParseAddr("80.231.153.1"), Options: traceroute.Options{Protocol: traceroute.ICMP, FirstTTL: 1}, StartedAt: t0, FinishedAt: t0, Reached: true, DestinationTTL: 1,
		Probes: []traceroute.ProbeResult{{TTL: 1, Responded: true, From: netip.MustParseAddr("80.231.153.1"), RTT: 5 * time.Millisecond, Kind: traceroute.ReplyEcho}}}
	m, err := e.Enrich(context.Background(), Input{Trace: rt, Device: &Device{Lat: 19.06, Lon: 72.83, HasFix: true}}) // Mumbai
	if err != nil {
		t.Fatal(err)
	}
	if m.Hops[0].LocationStatus != "rejected_light_speed" || len(m.Links) != 0 {
		t.Fatalf("Paris 5 ms from Mumbai accepted: %+v", m.Hops[0])
	}
}
