package subsea

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
)

const mockPath = "../../../../data/curated/infrastructure/submarine_cables.mock.geojson"

func load(t *testing.T) *Dataset {
	t.Helper()
	ds, err := Load(mockPath)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func names(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.CableID)
	}
	return out
}

func TestMockDatasetShape(t *testing.T) {
	ds := load(t)
	if len(ds.Cables) < 40 {
		t.Fatalf("%d cables", len(ds.Cables))
	}
	c, ok := ds.Cable("imewe")
	if !ok || len(c.Lines) == 0 || !slices.Contains(c.Owners, "Tata Communications") || c.RFSYear != 2010 {
		t.Fatalf("imewe = %+v", c)
	}
	var mumbai bool
	for _, l := range c.Landings {
		mumbai = mumbai || (l.ID == "mumbai-india" && l.Country == "IN")
	}
	if !mumbai {
		t.Fatal("imewe should land in Mumbai")
	}
}

func TestMatchMumbaiMarseille(t *testing.T) {
	m := NewMatcher(load(t))
	m.Year = 2026
	got := m.Match(MatchInput{
		A: gis.LatLon{Lat: 19.0728, Lon: 72.8826}, B: gis.LatLon{Lat: 43.2970, Lon: 5.3811},
		DeltaRTTms: 92.8, OperatorKeywords: []string{"tata"}, AccuracyKM: 50,
	})
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	top := got[0]
	if !slices.Contains([]string{"seamewe-4", "imewe"}, top.CableID) || !top.OwnerMatch {
		t.Fatalf("top %s (owner %v); all %v", top.CableID, top.OwnerMatch, names(got))
	}
	// Red Sea route: Mumbai ... Jeddah ... Suez ... Marseille, in order.
	var ls []string
	for _, l := range top.RouteLandings {
		ls = append(ls, l.Country)
	}
	if ls[0] != "IN" || ls[len(ls)-1] != "FR" || !slices.Contains(ls, "EG") {
		t.Fatalf("route landings %v", ls)
	}
	if top.CablePathKM < 8000 || top.CablePathKM > 10000 || len(top.Route) < 5 {
		t.Fatalf("path %.0f km, %d points", top.CablePathKM, len(top.Route))
	}
	sum := 0.0
	for _, c := range got {
		sum += c.Probability
		if c.CableID == "india-europe-xpress-iex" {
			t.Fatal("IEX (RFS 2027) is not in service")
		}
	}
	if sum > 1.001 {
		t.Fatalf("probabilities sum to %.3f", sum)
	}
}

func TestMatchAcrossAntimeridian(t *testing.T) {
	m := NewMatcher(load(t))
	got := m.Match(MatchInput{A: gis.LatLon{Lat: 35.68, Lon: 139.65}, B: gis.LatLon{Lat: 34.05, Lon: -118.24}, DeltaRTTms: 100})
	if !slices.ContainsFunc(got, func(c Candidate) bool { return c.CableID == "jupiter" || c.CableID == "unity" }) {
		t.Fatalf("Tokyo-LA candidates %v", names(got))
	}
}

func TestRTTImplausibleCablePenalised(t *testing.T) {
	m := NewMatcher(load(t))
	// 40 ms cannot carry Mumbai-Marseille over any cable (~86 ms minimum).
	got := m.Match(MatchInput{A: gis.LatLon{Lat: 19.07, Lon: 72.88}, B: gis.LatLon{Lat: 43.30, Lon: 5.37}, DeltaRTTms: 40})
	for _, c := range got {
		if c.Components.RTTFit > 0.05 {
			t.Fatalf("%s rtt_fit %.3f for an impossible RTT", c.CableID, c.Components.RTTFit)
		}
	}
}

func TestLoadsRawTeleGeographyFile(t *testing.T) {
	const raw = "../../../../data/raw/telegeography/cable_cable-geo.json.gz"
	if _, err := os.Stat(raw); err != nil {
		t.Skip("raw TeleGeography file not present")
	}
	ds, err := Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Cables) < 600 {
		t.Fatalf("%d cables", len(ds.Cables))
	}
	// Geometry-only data still matches (no owners, so no owner component).
	got := NewMatcher(ds).Match(MatchInput{A: gis.LatLon{Lat: 36.85, Lon: -75.98}, B: gis.LatLon{Lat: 43.26, Lon: -2.94}, DeltaRTTms: 64})
	if len(got) == 0 || !strings.Contains(strings.Join(names(got), ","), "marea") {
		t.Fatalf("Virginia Beach-Bilbao: %v", names(got))
	}
}
