package gis

import (
	"math"
	"os"
	"slices"
	"sync"
	"testing"
)

const landPath = "../../../../data/raw/naturalearth/ne_50m_admin_0_countries.geojson.gz"

var (
	landOnce sync.Once
	land     *LandIndex
)

func loadLand(t *testing.T) *LandIndex {
	t.Helper()
	if _, err := os.Stat(landPath); err != nil {
		t.Skip("Natural Earth data not present")
	}
	landOnce.Do(func() {
		var err error
		land, err = LoadLandIndex(landPath)
		if err != nil {
			t.Fatal(err)
		}
	})
	return land
}

var city = map[string]LatLon{
	"mumbai": {19.076, 72.8777}, "mangaluru": {12.9141, 74.856}, "marseille": {43.2965, 5.3698}, "paris": {48.8566, 2.3522},
	"london": {51.5074, -0.1278}, "frankfurt": {50.1109, 8.6821}, "newyork": {40.7128, -74.006}, "moscow": {55.7558, 37.6173},
	"chennai": {13.0827, 80.2707}, "singapore": {1.3521, 103.8198}, "oslo": {59.9139, 10.7522},
}

func TestCountryAt(t *testing.T) {
	idx := loadLand(t)
	for name, want := range map[string]string{"paris": "FR", "mumbai": "IN", "oslo": "NO", "frankfurt": "DE"} {
		if cc, ok := idx.CountryAt(city[name]); !ok || cc != want {
			t.Errorf("%s: got %q ok=%v, want %s", name, cc, ok, want)
		}
	}
	// At 1:50m the lower-Manhattan city-centre coordinate falls in New York
	// Harbor; coastal hubs need a fallback country (the enricher uses GeoIP's).
	if _, ok := idx.CountryAt(city["newyork"]); ok {
		t.Log("note: NYC centre now resolves to land")
	}
	if cc, ok := idx.nearestCountry(city["newyork"]); !ok || cc != "US" {
		t.Errorf("nearest country to NYC = %q", cc)
	}
	if cc, _ := idx.CountryAt(LatLon{Lat: 40.735, Lon: -74.17}); cc != "US" { // Newark
		t.Errorf("Newark = %q", cc)
	}
	for _, p := range []LatLon{{30, -40}, {-20, 80}, {42.5, 50.5} /* Caspian Sea */} {
		if cc, ok := idx.CountryAt(p); ok {
			t.Errorf("%v should be water, got %s", p, cc)
		}
	}
}

func TestOceanCrossing(t *testing.T) {
	idx := loadLand(t)
	cases := []struct {
		a, b  string
		ocean bool
	}{
		{"paris", "frankfurt", false},
		{"frankfurt", "moscow", false},
		{"london", "paris", true}, // English Channel
		{"newyork", "london", true},
		{"chennai", "singapore", true},
		{"mumbai", "marseille", true},
		{"mangaluru", "mumbai", false}, // great circle hugs the Konkan coast: coastal, not a crossing
	}
	for _, c := range cases {
		x := idx.AnalyzeCrossing(city[c.a], city[c.b], CrossingOptions{})
		if x.CrossesOcean != c.ocean {
			t.Errorf("%s->%s crosses_ocean=%v want %v (runs %+v)", c.a, c.b, x.CrossesOcean, c.ocean, x.WaterRuns)
		}
	}
	x := idx.AnalyzeCrossing(city["mangaluru"], city["mumbai"], CrossingOptions{})
	if len(x.WaterRuns) == 0 || !x.WaterRuns[0].Coastal || x.WaterRuns[0].MaxOffshoreKM >= 100 {
		t.Fatalf("expected a coastal run: %+v", x.WaterRuns)
	}
	ny := idx.AnalyzeCrossing(city["newyork"], city["london"], CrossingOptions{})
	if ny.LongestWaterKM < 2500 || !slices.Contains(ny.Countries, "IE") || len(ny.WaterSamples) == 0 {
		t.Fatalf("NY-London: %+v", ny)
	}
}

func TestGeodesy(t *testing.T) {
	if d := HaversineKM(city["mumbai"], city["marseille"]); math.Abs(d-6764) > 10 {
		t.Fatalf("Mumbai-Marseille %.0f km", d)
	}
	mid := Interpolate(city["paris"], city["frankfurt"], 0.5)
	if math.Abs(HaversineKM(city["paris"], mid)-HaversineKM(mid, city["frankfurt"])) > 0.01 {
		t.Fatal("midpoint not equidistant")
	}
	p := Destination(LatLon{0, 0}, 90, 111.195)
	if math.Abs(p.Lon-1) > 1e-3 || math.Abs(p.Lat) > 1e-9 {
		t.Fatalf("destination %+v", p)
	}
	if d, _ := PointSegmentKM(LatLon{1, 0}, LatLon{0, -1}, LatLon{0, 1}); math.Abs(d-111.2) > 0.5 {
		t.Fatalf("point-segment %.2f", d)
	}
	if MinRTTms(204.2) != 2 {
		t.Fatal("fiber constant")
	}
}
