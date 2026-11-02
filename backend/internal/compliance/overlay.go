package compliance

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// RegionGeometry returns a region's territory as a GeoJSON
// FeatureCollection (one MultiPolygon feature per member country,
// subdivision or custom polygon), simplified with Douglas-Peucker at
// toleranceDeg. Visualisation clients draw regulatory borders from it.
func (e *Engine) RegionGeometry(regionID string, toleranceDeg float64) (map[string]any, error) {
	snap := e.Registry.Snapshot()
	r, ok := snap.Regions[regionID]
	if !ok {
		return nil, ErrNotFound
	}
	feats := []map[string]any{}
	add := func(ix *polygonIndex, tag, kind string) {
		if ix == nil {
			return
		}
		var mp [][][][2]float64
		name := ""
		for _, id := range ix.byTag[tag] {
			t := ix.polys[id]
			name = t.name
			var poly [][][2]float64
			for i, ring := range t.rings {
				s := simplifyRing(ring, toleranceDeg)
				if len(s) < 4 {
					if i == 0 {
						break // outer ring collapsed: drop the islet
					}
					continue
				}
				poly = append(poly, s)
			}
			if len(poly) > 0 {
				mp = append(mp, poly)
			}
		}
		if len(mp) == 0 {
			return
		}
		feats = append(feats, map[string]any{
			"type":       "Feature",
			"properties": map[string]any{"region": regionID, "member": tag, "kind": kind, "name": name},
			"geometry":   map[string]any{"type": "MultiPolygon", "coordinates": mp},
		})
	}
	for _, cc := range r.Countries {
		add(e.Locator.countries, cc, "country")
	}
	for _, sd := range r.Subdivisions {
		add(e.Locator.subdivisions, sd, "subdivision")
	}
	if r.Polygon != nil {
		add(snap.custom, r.ID, "custom")
	}
	return map[string]any{"type": "FeatureCollection", "region": regionID, "version": r.Version, "tolerance_deg": toleranceDeg, "features": feats}, nil
}

// simplifyRing is iterative Douglas-Peucker in lon/lat degrees; coordinates
// are rounded to 0.001 degrees (~100 m).
func simplifyRing(ring [][2]float64, tol float64) [][2]float64 {
	round := func(p [2]float64) [2]float64 {
		return [2]float64{math.Round(p[0]*1000) / 1000, math.Round(p[1]*1000) / 1000}
	}
	if tol <= 0 || len(ring) < 5 {
		out := make([][2]float64, len(ring))
		for i, p := range ring {
			out[i] = round(p)
		}
		return out
	}
	keep := make([]bool, len(ring))
	keep[0], keep[len(ring)-1] = true, true
	stack := [][2]int{{0, len(ring) - 1}}
	for len(stack) > 0 {
		seg := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		i, j := seg[0], seg[1]
		best, idx := 0.0, -1
		for k := i + 1; k < j; k++ {
			if d := segDist(ring[k], ring[i], ring[j]); d > best {
				best, idx = d, k
			}
		}
		if best > tol {
			keep[idx] = true
			stack = append(stack, [2]int{i, idx}, [2]int{idx, j})
		}
	}
	var out [][2]float64
	for i, p := range ring {
		if keep[i] {
			out = append(out, round(p))
		}
	}
	return out
}

func segDist(p, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = math.Max(0, math.Min(1, ((p[0]-a[0])*dx+(p[1]-a[1])*dy)/l2))
	}
	return math.Hypot(p[0]-a[0]-t*dx, p[1]-a[1]-t*dy)
}

// LawDirectory serves the curated jurisdictions file
// (data/curated/regulations/jurisdictions.json) for the countries a route
// touches. Entries are passed through as stored so the file can grow fields
// without code changes.
type LawDirectory struct {
	AsOf    string
	entries map[string]json.RawMessage // ISO2 (or "EU") -> entry
	names   map[string]string
}

func LoadLawDirectory(path string) (*LawDirectory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		AsOf          string            `json:"as_of"`
		Jurisdictions []json.RawMessage `json:"jurisdictions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("law directory %s: %w", path, err)
	}
	d := &LawDirectory{AsOf: doc.AsOf, entries: map[string]json.RawMessage{}, names: map[string]string{}}
	for _, raw := range doc.Jurisdictions {
		var head struct {
			ISO2 string `json:"iso2"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || head.ISO2 == "" {
			continue
		}
		d.entries[strings.ToUpper(head.ISO2)] = raw
		d.names[strings.ToUpper(head.ISO2)] = head.Name
	}
	return d, nil
}

// LawsFor returns entries for the given countries. EU member states also get
// the "EU" entry (GDPR, NIS2, ...), resolved through the registry's "eu" region.
func (e *Engine) LawsFor(d *LawDirectory, countries []string) (found []json.RawMessage, missing []string) {
	eu := map[string]bool{}
	if r, ok := e.Registry.Snapshot().Regions["eu"]; ok {
		for _, c := range r.Countries {
			eu[c] = true
		}
	}
	seen := map[string]bool{}
	var keys []string
	for _, c := range countries {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		for _, k := range []string{c, map[bool]string{true: "EU"}[eu[c]]} {
			if k != "" && !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i] == "EU" && keys[j] != "EU" })
	for _, k := range keys {
		if raw, ok := d.entries[k]; ok {
			found = append(found, raw)
		} else if k != "EU" {
			missing = append(missing, k)
		}
	}
	return found, missing
}
