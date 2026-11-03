// Package subsea matches hop-to-hop links to submarine cables using a
// dataset in TeleGeography's Submarine Cable Map GeoJSON shape
// (FeatureCollection of MultiLineString features keyed by cable id).
package subsea

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
)

type Landing struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Country string `json:"country"`
	gis.LatLon
}

type Cable struct {
	ID       string
	Name     string
	Color    string
	LengthKM float64
	RFSYear  int
	Planned  bool
	Owners   []string
	Landings []Landing
	Lines    [][]gis.LatLon

	graphOnce sync.Once
	graph     *cableGraph
}

// Dataset is an in-memory cable dataset with a spatial index.
type Dataset struct {
	Name   string
	Cables []*Cable
	byID   map[string]*Cable
	grid   map[[2]int][]int32 // 2-degree cells -> cable indices
}

const cellDeg = 2.0

// Load reads a cable GeoJSON file (optionally gzipped). Accepts both the
// enriched mock (properties include owners, rfs_year, landing_points) and
// TeleGeography's raw cable-geo.json (geometry, id, name, color only).
func Load(path string) (*Dataset, error) {
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
	return Read(r)
}

func Read(r io.Reader) (*Dataset, error) {
	var fc struct {
		Name     string `json:"name"`
		Features []struct {
			Properties struct {
				ID            string    `json:"id"`
				Name          string    `json:"name"`
				Color         string    `json:"color"`
				LengthKM      *float64  `json:"length_km"`
				RFSYear       *int      `json:"rfs_year"`
				IsPlanned     bool      `json:"is_planned"`
				Owners        []string  `json:"owners"`
				LandingPoints []Landing `json:"landing_points"`
			} `json:"properties"`
			Geometry struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.NewDecoder(r).Decode(&fc); err != nil {
		return nil, fmt.Errorf("decode cable geojson: %w", err)
	}
	ds := &Dataset{Name: fc.Name, byID: map[string]*Cable{}, grid: map[[2]int][]int32{}}
	for _, f := range fc.Features {
		p := f.Properties
		var lines [][][2]float64
		switch f.Geometry.Type {
		case "MultiLineString":
			if err := json.Unmarshal(f.Geometry.Coordinates, &lines); err != nil {
				return nil, fmt.Errorf("cable %s: %w", p.ID, err)
			}
		case "LineString":
			var l [][2]float64
			if err := json.Unmarshal(f.Geometry.Coordinates, &l); err != nil {
				return nil, fmt.Errorf("cable %s: %w", p.ID, err)
			}
			lines = [][][2]float64{l}
		default:
			continue
		}
		// TeleGeography splits a cable into several features sharing an id.
		c := ds.byID[p.ID]
		if c == nil {
			c = &Cable{ID: p.ID, Name: p.Name, Color: p.Color, Planned: p.IsPlanned, Owners: p.Owners, Landings: p.LandingPoints}
			if p.LengthKM != nil {
				c.LengthKM = *p.LengthKM
			}
			if p.RFSYear != nil {
				c.RFSYear = *p.RFSYear
			}
			ds.byID[p.ID] = c
			ds.Cables = append(ds.Cables, c)
		}
		for _, l := range lines {
			line := make([]gis.LatLon, 0, len(l))
			for _, xy := range l {
				line = append(line, gis.LatLon{Lat: xy[1], Lon: xy[0]})
			}
			if len(line) >= 2 {
				c.Lines = append(c.Lines, line)
			}
		}
	}
	for i, c := range ds.Cables {
		ds.index(int32(i), c)
	}
	if len(ds.Cables) == 0 {
		return nil, fmt.Errorf("no cables in dataset")
	}
	return ds, nil
}

func cell(lat, lon float64) [2]int {
	return [2]int{int(math.Floor(lat / cellDeg)), int(math.Floor(lon / cellDeg))}
}

func (ds *Dataset) index(id int32, c *Cable) {
	seen := map[[2]int]bool{}
	for _, line := range c.Lines {
		for i := 1; i < len(line); i++ {
			a, b := line[i-1], line[i]
			if math.Abs(a.Lon-b.Lon) > 180 { // antimeridian-crossing segment: index both ends only
				for _, p := range []gis.LatLon{a, b} {
					k := cell(p.Lat, p.Lon)
					if !seen[k] {
						seen[k] = true
						ds.grid[k] = append(ds.grid[k], id)
					}
				}
				continue
			}
			c0, c1 := cell(math.Min(a.Lat, b.Lat), math.Min(a.Lon, b.Lon)), cell(math.Max(a.Lat, b.Lat), math.Max(a.Lon, b.Lon))
			for la := c0[0]; la <= c1[0]; la++ {
				for lo := c0[1]; lo <= c1[1]; lo++ {
					k := [2]int{la, lo}
					if !seen[k] {
						seen[k] = true
						ds.grid[k] = append(ds.grid[k], id)
					}
				}
			}
		}
	}
}

// Cable returns a cable by id.
func (ds *Dataset) Cable(id string) (*Cable, bool) {
	c, ok := ds.byID[id]
	return c, ok
}

// Projection is the closest point on a cable to some location.
type Projection struct {
	DistanceKM float64
	Line, Seg  int     // segment Lines[Line][Seg] -> [Seg+1]
	T          float64 // fraction along the segment
	Point      gis.LatLon
}

// Project finds the closest point of cable c to p.
func (c *Cable) Project(p gis.LatLon) Projection {
	best := Projection{DistanceKM: math.Inf(1)}
	for li, line := range c.Lines {
		for si := 0; si+1 < len(line); si++ {
			d, t := gis.PointSegmentKM(p, line[si], line[si+1])
			if d < best.DistanceKM {
				best = Projection{DistanceKM: d, Line: li, Seg: si, T: t}
			}
		}
	}
	if !math.IsInf(best.DistanceKM, 1) {
		a, b := c.Lines[best.Line][best.Seg], c.Lines[best.Line][best.Seg+1]
		best.Point = gis.LatLon{Lat: a.Lat + (b.Lat-a.Lat)*best.T, Lon: a.Lon + (b.Lon-a.Lon)*best.T}
	}
	return best
}

// Near returns cables with any geometry within radiusKM of p, with their projections.
func (ds *Dataset) Near(p gis.LatLon, radiusKM float64) map[*Cable]Projection {
	dLat := radiusKM / 111.0
	dLon := radiusKM / (111.0 * math.Max(0.1, math.Cos(p.Lat*math.Pi/180)))
	c0, c1 := cell(p.Lat-dLat, p.Lon-dLon), cell(p.Lat+dLat, p.Lon+dLon)
	cand := map[int32]bool{}
	for la := c0[0]; la <= c1[0]; la++ {
		for lo := c0[1]; lo <= c1[1]; lo++ {
			lon := lo
			// wrap longitude cells
			if float64(lon)*cellDeg >= 180 {
				lon -= int(360 / cellDeg)
			} else if float64(lon)*cellDeg < -180 {
				lon += int(360 / cellDeg)
			}
			for _, id := range ds.grid[[2]int{la, lon}] {
				cand[id] = true
			}
		}
	}
	out := map[*Cable]Projection{}
	for id := range cand {
		c := ds.Cables[id]
		if pr := c.Project(p); pr.DistanceKM <= radiusKM {
			out[c] = pr
		}
	}
	return out
}

// NearestLanding returns the landing point of c closest to p.
func (c *Cable) NearestLanding(p gis.LatLon) (Landing, float64) {
	best, bd := Landing{}, math.Inf(1)
	for _, l := range c.Landings {
		if d := gis.HaversineKM(p, l.LatLon); d < bd {
			best, bd = l, d
		}
	}
	return best, bd
}
