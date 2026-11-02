package gis

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// LandIndex answers "which country (if any) is this point in?" using Natural
// Earth admin-0 polygons and a 1x1 degree grid of candidate polygons.
// Anything not inside a country polygon is water (oceans, seas, and the few
// large lakes Natural Earth leaves outside countries, e.g. the Caspian).
type LandIndex struct {
	polys []polygon
	grid  map[[2]int][]int32
}

type polygon struct {
	iso2           string
	minLat, maxLat float64
	minLon, maxLon float64
	rings          [][]LatLon // rings[0] outer, rest holes
}

// LoadLandIndex reads a Natural Earth admin-0 GeoJSON (optionally .gz).
func LoadLandIndex(path string) (*LandIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	return ReadLandIndex(r)
}

func ReadLandIndex(r io.Reader) (*LandIndex, error) {
	var fc struct {
		Features []struct {
			Properties map[string]any `json:"properties"`
			Geometry   struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.NewDecoder(r).Decode(&fc); err != nil {
		return nil, fmt.Errorf("decode land geojson: %w", err)
	}
	idx := &LandIndex{grid: map[[2]int][]int32{}}
	for _, f := range fc.Features {
		iso, _ := f.Properties["ISO_A2"].(string)
		if iso == "-99" || iso == "" {
			iso, _ = f.Properties["ISO_A2_EH"].(string)
		}
		if iso == "-99" {
			iso = ""
		}
		var polys [][][][2]float64
		switch f.Geometry.Type {
		case "Polygon":
			var p [][][2]float64
			if err := json.Unmarshal(f.Geometry.Coordinates, &p); err != nil {
				return nil, err
			}
			polys = [][][][2]float64{p}
		case "MultiPolygon":
			if err := json.Unmarshal(f.Geometry.Coordinates, &polys); err != nil {
				return nil, err
			}
		default:
			continue
		}
		for _, p := range polys {
			idx.add(iso, p)
		}
	}
	if len(idx.polys) == 0 {
		return nil, fmt.Errorf("no polygons in land data")
	}
	return idx, nil
}

func (idx *LandIndex) add(iso string, rings [][][2]float64) {
	pg := polygon{iso2: iso, minLat: 90, maxLat: -90, minLon: 180, maxLon: -180}
	for _, r := range rings {
		ring := make([]LatLon, len(r))
		for i, c := range r {
			ring[i] = LatLon{Lat: c[1], Lon: c[0]}
		}
		pg.rings = append(pg.rings, ring)
	}
	for _, c := range pg.rings[0] {
		pg.minLat, pg.maxLat = math.Min(pg.minLat, c.Lat), math.Max(pg.maxLat, c.Lat)
		pg.minLon, pg.maxLon = math.Min(pg.minLon, c.Lon), math.Max(pg.maxLon, c.Lon)
	}
	id := int32(len(idx.polys))
	idx.polys = append(idx.polys, pg)
	for la := int(math.Floor(pg.minLat)); la <= int(math.Floor(pg.maxLat)); la++ {
		for lo := int(math.Floor(pg.minLon)); lo <= int(math.Floor(pg.maxLon)); lo++ {
			k := [2]int{la, lo}
			idx.grid[k] = append(idx.grid[k], id)
		}
	}
}

// CountryAt returns the ISO2 code of the country containing p and whether p
// is on land. Land without a code (e.g. disputed territory) returns ("", true).
func (idx *LandIndex) CountryAt(p LatLon) (string, bool) {
	k := [2]int{int(math.Floor(p.Lat)), int(math.Floor(wrapLon(p.Lon)))}
	for _, id := range idx.grid[k] {
		pg := &idx.polys[id]
		if p.Lat < pg.minLat || p.Lat > pg.maxLat || p.Lon < pg.minLon || p.Lon > pg.maxLon {
			continue
		}
		if inRing(p, pg.rings[0]) {
			inHole := false
			for _, h := range pg.rings[1:] {
				if inRing(p, h) {
					inHole = true
					break
				}
			}
			if !inHole {
				return pg.iso2, true
			}
		}
	}
	return "", false
}

// inRing: even-odd ray casting in lon/lat space. Natural Earth splits
// polygons at the antimeridian, so no ring crosses +-180.
func inRing(p LatLon, ring []LatLon) bool {
	in := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		a, b := ring[i], ring[j]
		if (a.Lat > p.Lat) != (b.Lat > p.Lat) {
			x := (b.Lon-a.Lon)*(p.Lat-a.Lat)/(b.Lat-a.Lat) + a.Lon
			if p.Lon < x {
				in = !in
			}
		}
	}
	return in
}

// PolygonCount is the number of indexed polygons.
func (idx *LandIndex) PolygonCount() int { return len(idx.polys) }
