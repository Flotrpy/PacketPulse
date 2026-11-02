package compliance

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// LatLon is a WGS84 coordinate in degrees.
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// polygonIndex answers point-in-polygon queries for tagged polygons using a
// 1x1 degree grid of candidate polygons. Rings are [lon,lat]; polygons that
// cross the antimeridian must be split (Natural Earth already is).
type polygonIndex struct {
	polys []tagged
	grid  map[[2]int][]int32
	byTag map[string][]int32
}

type tagged struct {
	tag                            string
	name                           string
	minLat, maxLat, minLon, maxLon float64
	rings                          [][][2]float64 // rings[0] outer, rest holes
}

func newPolygonIndex() *polygonIndex {
	return &polygonIndex{grid: map[[2]int][]int32{}, byTag: map[string][]int32{}}
}

func (ix *polygonIndex) add(tag, name string, rings [][][2]float64) {
	if len(rings) == 0 || len(rings[0]) < 3 {
		return
	}
	t := tagged{tag: tag, name: name, minLat: 90, maxLat: -90, minLon: 180, maxLon: -180, rings: rings}
	for _, c := range rings[0] {
		t.minLon, t.maxLon = math.Min(t.minLon, c[0]), math.Max(t.maxLon, c[0])
		t.minLat, t.maxLat = math.Min(t.minLat, c[1]), math.Max(t.maxLat, c[1])
	}
	id := int32(len(ix.polys))
	ix.polys = append(ix.polys, t)
	ix.byTag[tag] = append(ix.byTag[tag], id)
	for la := int(math.Floor(t.minLat)); la <= int(math.Floor(t.maxLat)); la++ {
		for lo := int(math.Floor(t.minLon)); lo <= int(math.Floor(t.maxLon)); lo++ {
			ix.grid[[2]int{la, lo}] = append(ix.grid[[2]int{la, lo}], id)
		}
	}
}

// addGeometry adds a GeoJSON Polygon or MultiPolygon.
func (ix *polygonIndex) addGeometry(tag, name string, g Geometry) error {
	switch g.Type {
	case "Polygon":
		var p [][][2]float64
		if err := json.Unmarshal(g.Coordinates, &p); err != nil {
			return fmt.Errorf("polygon %s: %w", tag, err)
		}
		ix.add(tag, name, p)
	case "MultiPolygon":
		var mp [][][][2]float64
		if err := json.Unmarshal(g.Coordinates, &mp); err != nil {
			return fmt.Errorf("multipolygon %s: %w", tag, err)
		}
		for _, p := range mp {
			ix.add(tag, name, p)
		}
	default:
		return fmt.Errorf("geometry %s: type %q is not Polygon/MultiPolygon", tag, g.Type)
	}
	return nil
}

// at returns the first polygon containing p (tag, name).
func (ix *polygonIndex) at(p LatLon) (string, string, bool) {
	if ix == nil {
		return "", "", false
	}
	for _, id := range ix.grid[[2]int{int(math.Floor(p.Lat)), int(math.Floor(p.Lon))}] {
		t := &ix.polys[id]
		if p.Lat < t.minLat || p.Lat > t.maxLat || p.Lon < t.minLon || p.Lon > t.maxLon || !inRing(p, t.rings[0]) {
			continue
		}
		hole := false
		for _, h := range t.rings[1:] {
			if inRing(p, h) {
				hole = true
				break
			}
		}
		if !hole {
			return t.tag, t.name, true
		}
	}
	return "", "", false
}

// all returns every polygon tag containing p (custom regions may overlap).
func (ix *polygonIndex) all(p LatLon) []string {
	if ix == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, id := range ix.grid[[2]int{int(math.Floor(p.Lat)), int(math.Floor(p.Lon))}] {
		t := &ix.polys[id]
		if seen[t.tag] || p.Lat < t.minLat || p.Lat > t.maxLat || p.Lon < t.minLon || p.Lon > t.maxLon || !inRing(p, t.rings[0]) {
			continue
		}
		hole := false
		for _, h := range t.rings[1:] {
			if inRing(p, h) {
				hole = true
				break
			}
		}
		if !hole {
			seen[t.tag] = true
			out = append(out, t.tag)
		}
	}
	return out
}

func inRing(p LatLon, ring [][2]float64) bool {
	in := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		a, b := ring[i], ring[j]
		if (a[1] > p.Lat) != (b[1] > p.Lat) {
			if x := (b[0]-a[0])*(p.Lat-a[1])/(b[1]-a[1]) + a[0]; p.Lon < x {
				in = !in
			}
		}
	}
	return in
}

// Geometry is a GeoJSON geometry object.
type Geometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}

// Location is what the locator knows about a coordinate.
type Location struct {
	CountryISO2     string `json:"country_iso2,omitempty"`
	CountryName     string `json:"country_name,omitempty"`
	Subdivision     string `json:"subdivision,omitempty"` // ISO 3166-2, e.g. US-CA
	SubdivisionName string `json:"subdivision_name,omitempty"`
	// Source: polygon | nearest_coast (point in water, snapped to the
	// nearest country within 50 km) | hint (caller-supplied country) | none
	Source string `json:"source"`
}

// Locator resolves coordinates to countries (Natural Earth admin-0) and
// first-level subdivisions (admin-1).
type Locator struct {
	countries    *polygonIndex
	subdivisions *polygonIndex
}

// LoadLocator reads Natural Earth admin-0 and (optionally) admin-1 GeoJSON
// files; .gz is accepted.
func LoadLocator(admin0Path, admin1Path string) (*Locator, error) {
	l := &Locator{countries: newPolygonIndex(), subdivisions: newPolygonIndex()}
	if err := readFeatures(admin0Path, func(props map[string]any, g Geometry) error {
		iso := str(props["ISO_A2"])
		if iso == "-99" || iso == "" {
			iso = str(props["ISO_A2_EH"])
		}
		if iso == "-99" || iso == "" {
			return nil
		}
		return l.countries.addGeometry(iso, str(props["NAME"]), g)
	}); err != nil {
		return nil, fmt.Errorf("admin-0: %w", err)
	}
	if admin1Path != "" {
		if err := readFeatures(admin1Path, func(props map[string]any, g Geometry) error {
			code := str(props["iso_3166_2"])
			if code == "" || strings.HasSuffix(code, "~") {
				return nil
			}
			return l.subdivisions.addGeometry(code, str(props["name"]), g)
		}); err != nil {
			return nil, fmt.Errorf("admin-1: %w", err)
		}
	}
	return l, nil
}

func str(v any) string { s, _ := v.(string); return s }

func readFeatures(path string, fn func(map[string]any, Geometry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	var fc struct {
		Features []struct {
			Properties map[string]any `json:"properties"`
			Geometry   Geometry       `json:"geometry"`
		} `json:"features"`
	}
	if err := json.NewDecoder(r).Decode(&fc); err != nil {
		return err
	}
	for _, ft := range fc.Features {
		if err := fn(ft.Properties, ft.Geometry); err != nil {
			return err
		}
	}
	return nil
}

// Locate resolves p. Coastal coordinates that fall just offshore at 1:50m
// scale (harbour cities, cable landing stations) snap to the nearest country
// within 50 km; otherwise hint (e.g. the GeoIP country) is used.
func (l *Locator) Locate(p LatLon, hint string) Location {
	if cc, name, ok := l.countries.at(p); ok {
		loc := Location{CountryISO2: cc, CountryName: name, Source: "polygon"}
		if code, sname, ok := l.subdivisions.at(p); ok && strings.HasPrefix(code, cc+"-") {
			loc.Subdivision, loc.SubdivisionName = code, sname
		}
		return loc
	}
	for _, r := range []float64{5, 10, 20, 35, 50} {
		for b := 0; b < 8; b++ {
			q := destination(p, float64(b)*45, r)
			if cc, name, ok := l.countries.at(q); ok {
				loc := Location{CountryISO2: cc, CountryName: name, Source: "nearest_coast"}
				if code, sname, ok := l.subdivisions.at(q); ok && strings.HasPrefix(code, cc+"-") {
					loc.Subdivision, loc.SubdivisionName = code, sname
				}
				return loc
			}
		}
	}
	if hint != "" {
		return Location{CountryISO2: strings.ToUpper(hint), Source: "hint"}
	}
	return Location{Source: "none"}
}

const earthRadiusKM = 6371.0

func destination(p LatLon, bearingDeg, km float64) LatLon {
	d := km / earthRadiusKM
	br := bearingDeg * math.Pi / 180
	la1, lo1 := p.Lat*math.Pi/180, p.Lon*math.Pi/180
	la2 := math.Asin(math.Sin(la1)*math.Cos(d) + math.Cos(la1)*math.Sin(d)*math.Cos(br))
	lo2 := lo1 + math.Atan2(math.Sin(br)*math.Sin(d)*math.Cos(la1), math.Cos(d)-math.Sin(la1)*math.Sin(la2))
	lon := lo2 * 180 / math.Pi
	for lon > 180 {
		lon -= 360
	}
	for lon < -180 {
		lon += 360
	}
	return LatLon{Lat: la2 * 180 / math.Pi, Lon: lon}
}

func haversineKM(a, b LatLon) float64 {
	p1, p2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dp, dl := (b.Lat-a.Lat)*math.Pi/180, (b.Lon-a.Lon)*math.Pi/180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * earthRadiusKM * math.Asin(math.Min(1, math.Sqrt(h)))
}
