package subsea

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
)

// MatchInput describes one ocean-crossing link.
type MatchInput struct {
	A, B gis.LatLon
	// AccuracyKM of the hop locations (widens the search).
	AccuracyKM float64
	// DeltaRTTms is RTT(B) - RTT(A); 0 = unknown.
	DeltaRTTms float64
	// OperatorKeywords are distinctive words from the hops' AS organisations
	// (e.g. "tata"), matched against cable owners.
	OperatorKeywords []string
	// WaterSamples are points of the great-circle path that lie over water.
	WaterSamples []gis.LatLon
}

// Candidate is one scored cable.
type Candidate struct {
	CableID       string       `json:"cable_id"`
	Name          string       `json:"name"`
	Probability   float64      `json:"probability"`
	Score         float64      `json:"score"`
	OwnerMatch    bool         `json:"owner_match"`
	MatchedOwners []string     `json:"matched_owners,omitempty"`
	RFSYear       int          `json:"rfs_year,omitempty"`
	LandingA      *Landing     `json:"landing_a,omitempty"`
	LandingB      *Landing     `json:"landing_b,omitempty"`
	BackhaulAKM   float64      `json:"backhaul_a_km"`   // hop A -> cable (terrestrial)
	BackhaulBKM   float64      `json:"backhaul_b_km"`   // cable -> hop B (terrestrial)
	CablePathKM   float64      `json:"cable_path_km"`   // along the cable geometry
	ExpectedRTTms float64      `json:"expected_rtt_ms"` // light-speed RTT over backhaul + cable
	CorridorKM    *float64     `json:"corridor_km,omitempty"`
	Components    Components   `json:"score_components"`
	Route         []gis.LatLon `json:"route,omitempty"` // simplified along-cable path, A side to B side
	// RouteLandings are the cable's landing stations on the matched stretch,
	// in path order: the jurisdictions the traffic physically touches.
	RouteLandings []Landing `json:"route_landings"`
}

type Components struct {
	RTTFit    float64 `json:"rtt_fit"`
	Endpoints float64 `json:"endpoints"`
	Owner     float64 `json:"owner"`
	Corridor  float64 `json:"corridor"`
}

// Matcher scores cables for a link.
type Matcher struct {
	DS *Dataset
	// SearchRadiusKM: max distance from a hop to a cable for it to be considered.
	SearchRadiusKM float64
	// BackhaulStretch: terrestrial backhaul is longer than the straight line.
	BackhaulStretch float64
	// MaxCandidates returned (best first).
	MaxCandidates int
	// Year: cables with RFS after this year are not in service.
	Year int
	// RouteMaxPoints simplifies returned routes.
	RouteMaxPoints int
}

func NewMatcher(ds *Dataset) *Matcher {
	return &Matcher{DS: ds, SearchRadiusKM: 1200, BackhaulStretch: 1.3, MaxCandidates: 5, Year: time.Now().Year(), RouteMaxPoints: 120}
}

// Weights for the score components. RTT fit dominates: it is the only
// signal that observes the actual path.
const (
	wRTT      = 0.35
	wEndpoint = 0.25
	wOwner    = 0.20
	wCorridor = 0.20
)

// Match returns cables ranked by probability (empty if none is near both ends).
func (m *Matcher) Match(in MatchInput) []Candidate {
	radius := m.SearchRadiusKM + in.AccuracyKM
	nearA := m.DS.Near(in.A, radius)
	nearB := m.DS.Near(in.B, radius)

	var out []Candidate
	total := 0.0
	for c, pa := range nearA {
		pb, ok := nearB[c]
		if !ok || c.Planned || (c.RFSYear > m.Year) {
			continue
		}
		route, ok := c.RouteBetween(pa, pb)
		if !ok || route.KM < 1 {
			continue // same point or disconnected branches
		}
		cand := Candidate{
			CableID: c.ID, Name: c.Name, RFSYear: c.RFSYear,
			BackhaulAKM: round1(pa.DistanceKM), BackhaulBKM: round1(pb.DistanceKM), CablePathKM: round1(route.KM),
		}
		if la, d := c.NearestLanding(pa.Point); d < 300 {
			l := la
			cand.LandingA = &l
		}
		if lb, d := c.NearestLanding(pb.Point); d < 300 {
			l := lb
			cand.LandingB = &l
		}

		pathKM := m.BackhaulStretch*(pa.DistanceKM+pb.DistanceKM) + route.KM
		cand.ExpectedRTTms = round1(gis.MinRTTms(pathKM))
		comp := Components{RTTFit: 0.5, Corridor: 0.5}
		if in.DeltaRTTms > 0 {
			diff := in.DeltaRTTms - cand.ExpectedRTTms
			comp.RTTFit = math.Exp(-math.Abs(diff) / (0.15*cand.ExpectedRTTms + 3))
			if diff < -0.1*cand.ExpectedRTTms-2 {
				// Observed RTT is faster than light along this cable: implausible.
				comp.RTTFit *= 0.1
			}
		}
		comp.Endpoints = math.Exp(-(pa.DistanceKM + pb.DistanceKM) / 500)
		cand.MatchedOwners = matchOwners(c.Owners, in.OperatorKeywords)
		if len(cand.MatchedOwners) > 0 {
			cand.OwnerMatch, comp.Owner = true, 1
		}
		if len(in.WaterSamples) > 0 {
			med := medianDistance(c, in.WaterSamples)
			cand.CorridorKM = &med
			comp.Corridor = math.Exp(-med / 400)
		}
		cand.Components = Components{RTTFit: round3(comp.RTTFit), Endpoints: round3(comp.Endpoints), Owner: comp.Owner, Corridor: round3(comp.Corridor)}
		cand.Score = round3(wRTT*comp.RTTFit + wEndpoint*comp.Endpoints + wOwner*comp.Owner + wCorridor*comp.Corridor)
		cand.Route = simplify(route.Path, m.RouteMaxPoints)
		cand.RouteLandings = landingsOnRoute(c, route.Path)
		total += cand.Score
		out = append(out, cand)
	}
	for i := range out {
		out[i].Probability = round3(out[i].Score / total)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].CableID < out[j].CableID
	})
	if m.MaxCandidates > 0 && len(out) > m.MaxCandidates {
		out = out[:m.MaxCandidates]
	}
	return out
}

func matchOwners(owners, keys []string) []string {
	var hits []string
	for _, o := range owners {
		lo := strings.ToLower(o)
		for _, k := range keys {
			if k != "" && strings.Contains(lo, strings.ToLower(k)) {
				hits = append(hits, o)
				break
			}
		}
	}
	return hits
}

// medianDistance of (up to 40) water samples to the cable, in km.
func medianDistance(c *Cable, samples []gis.LatLon) float64 {
	step := 1
	if len(samples) > 40 {
		step = len(samples) / 40
	}
	var ds []float64
	for i := 0; i < len(samples); i += step {
		ds = append(ds, c.Project(samples[i]).DistanceKM)
	}
	sort.Float64s(ds)
	return round1(ds[len(ds)/2])
}

// simplify keeps at most n points, evenly by index, always keeping both ends.
func simplify(p []gis.LatLon, n int) []gis.LatLon {
	if n <= 2 || len(p) <= n {
		out := make([]gis.LatLon, len(p))
		for i, q := range p {
			out[i] = gis.LatLon{Lat: round5(q.Lat), Lon: round5(q.Lon)}
		}
		return out
	}
	out := make([]gis.LatLon, 0, n)
	for i := 0; i < n; i++ {
		q := p[i*(len(p)-1)/(n-1)]
		out = append(out, gis.LatLon{Lat: round5(q.Lat), Lon: round5(q.Lon)})
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round5(v float64) float64 { return math.Round(v*1e5) / 1e5 }

// landingsOnRoute returns landings within 30 km of the route, ordered along it.
func landingsOnRoute(c *Cable, path []gis.LatLon) []Landing {
	type hit struct {
		l   Landing
		pos float64
	}
	var hits []hit
	for _, l := range c.Landings {
		best, pos, along := math.Inf(1), 0.0, 0.0
		for i := 0; i+1 < len(path); i++ {
			d, t := gis.PointSegmentKM(l.LatLon, path[i], path[i+1])
			seg := gis.HaversineKM(path[i], path[i+1])
			if d < best {
				best, pos = d, along+t*seg
			}
			along += seg
		}
		if best <= 30 {
			hits = append(hits, hit{l, pos})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	out := make([]Landing, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.l)
	}
	return out
}
