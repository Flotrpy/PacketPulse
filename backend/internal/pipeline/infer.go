package pipeline

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/store"
)

// InferInfrastructure classifies the transport medium of every segment
// between consecutive responding hops. Traceroute never observes the medium
// directly; it is inferred from geography, RTT and network ownership, and each
// segment carries a confidence.
type InferInfrastructure struct {
	Ref store.ReferenceStore
}

func (InferInfrastructure) Name() string   { return "infer_infrastructure" }
func (InferInfrastructure) Required() bool { return false }

// minLocConfidence is the lowest location confidence used for geography.
const minLocConfidence = 0.5

type node struct {
	hop    int // 0 = source host
	loc    *model.Location
	asn    int
	org    string
	class  string
	minRTT float64
	jitter float64
	loss   float64 // effective loss (see effectiveLoss)
	cloud  bool
	mobile bool
}

func (in InferInfrastructure) Process(_ context.Context, s *State) error {
	j := s.Journey
	nodes := []node{{hop: 0, loc: j.Metadata.Source.Location, class: "private"}}
	for i, h := range j.Hops {
		if !h.Responded {
			continue
		}
		n := node{hop: h.HopNumber, asn: h.ASN, org: h.ASOrg, class: h.AddressClass,
			minRTT: s.hopMinMS[i], jitter: h.RTT.Jitter, loss: effectiveLoss(j.Hops, i)}
		// Registry-country guesses say who holds the address, not where the
		// router is; they must not decide between fiber and subsea.
		if h.Location != nil && h.Location.Confidence >= minLocConfidence {
			n.loc = h.Location
		}
		if h.ASN != 0 {
			_, n.cloud = in.Ref.CloudProvider(h.ASN)
			_, n.mobile = in.Ref.MobileCarrier(h.ASN)
		}
		nodes = append(nodes, n)
	}

	in.detectCellular(s, nodes)

	j.Infrastructure = model.Infrastructure{Segments: []model.Segment{}, MediaSummary: []string{}}
	seen := map[string]bool{}
	anchor := -1 // last node with a usable location
	if nodes[0].loc != nil {
		anchor = 0
	}
	for k := 1; k < len(nodes); k++ {
		a := nodes[k-1]
		// Routers that hide their location (no geo data, MPLS, filtered)
		// sit right before ocean crossings surprisingly often. Measure the
		// link from the last located hop instead of giving up on it.
		if a.loc == nil && nodes[k].loc != nil && anchor >= 0 {
			a.loc, a.minRTT = nodes[anchor].loc, nodes[anchor].minRTT
		}
		seg := in.classify(s, a, nodes[k], len(j.Infrastructure.Segments))
		if nodes[k].loc != nil {
			anchor = k
		}
		j.Infrastructure.Segments = append(j.Infrastructure.Segments, seg)
		if !seen[seg.Medium] {
			seen[seg.Medium] = true
			j.Infrastructure.MediaSummary = append(j.Infrastructure.MediaSummary, seg.Medium)
		}
	}
	if s.cellular != nil {
		j.Metadata.Source.Access = "cellular"
	}
	return nil
}

// effectiveLoss ignores loss that later hops do not show: a router that
// rate-limits ICMP replies drops probes without losing forwarded traffic.
func effectiveLoss(hops []model.Hop, i int) float64 {
	loss := hops[i].LossPct
	for _, h := range hops[i+1:] {
		if h.Responded {
			loss = math.Min(loss, h.LossPct)
		}
	}
	return loss
}

// detectCellular finds the mobile packet gateway: the first public hop owned
// by a known MNO, either declared by the agent (access_type=cellular) or
// implied by CGNAT space in front of it.
func (in InferInfrastructure) detectCellular(s *State, nodes []node) {
	declared := s.Req.AccessType == "cellular"
	sawCGNAT := false
	for _, n := range nodes[1:] {
		if n.class == "cgnat" {
			sawCGNAT = true
			continue
		}
		if n.class != "public" {
			continue
		}
		if c, ok := in.Ref.MobileCarrier(n.asn); ok && (declared || sawCGNAT) {
			s.cellular = &model.CellularHaul{Carrier: c.Name, ASN: c.ASN, Country: c.CountryISO2, GatewayIP: s.Journey.Hops[n.hop-s.Journey.Hops[0].HopNumber].IP, CGNAT: sawCGNAT}
			s.gwHop = n.hop
		}
		break // only the first public hop can be the gateway
	}
	if s.cellular == nil && declared {
		s.cellular = &model.CellularHaul{Carrier: "unknown", CGNAT: sawCGNAT}
	}
}

func (in InferInfrastructure) classify(s *State, a, b node, segIdx int) model.Segment {
	seg := model.Segment{FromHop: a.hop, ToHop: b.hop, Medium: model.MediumUnknown, Health: "grey", Countries: []string{}}
	seg.DeltaRTTMS = round3(math.Max(0, b.minRTT-a.minRTT))
	located := a.loc != nil && b.loc != nil
	if located {
		seg.DistanceKM = round1(haversineKM(a.loc.Lat, a.loc.Lon, b.loc.Lat, b.loc.Lon))
		seg.MinFeasibleRTTMS = round3(minFeasibleRTT(seg.DistanceKM))
		seg.Countries = uniq(a.loc.CountryISO2, b.loc.CountryISO2)
	}

	switch {
	case s.cellular != nil && (s.gwHop == 0 && b.class != "public" || b.hop <= s.gwHop):
		c := *s.cellular
		seg.Medium, seg.Cellular, seg.Confidence = model.MediumCellularBackhaul, &c, 0.8
		if s.gwHop == 0 {
			seg.Confidence = 0.5
		}
	case b.class != "public" && a.class != "public":
		seg.Medium, seg.Confidence = model.MediumLAN, 0.9
	case a.cloud && b.cloud:
		seg.Medium, seg.Confidence = model.MediumCloudBackbone, 0.9
		seg.TerrestrialISP = &model.Operator{ASN: b.asn, Name: b.org}
	case !located:
		seg.Confidence = 0
	case seg.DistanceKM < 50:
		seg.Medium, seg.Confidence = model.MediumMetroFiber, 0.75
		seg.TerrestrialISP = operator(a, b)
	default:
		in.longHaul(s, &seg, a, b, segIdx)
	}
	seg.Health = health(seg, b)
	return seg
}

// longHaul decides between subsea and terrestrial for segments >= 50 km.
func (in InferInfrastructure) longHaul(s *State, seg *model.Segment, a, b node, segIdx int) {
	ca, cb := a.loc.CountryISO2, b.loc.CountryISO2
	countryA, okA := in.Ref.Country(ca)
	countryB, okB := in.Ref.Country(cb)
	var cables []store.Cable
	if ca != cb {
		cables = in.Ref.CablesBetween(ca, cb)
	}

	mustSea := okA && okB && ca != cb && (countryA.Island || countryB.Island || !continentsLandConnected(countryA.Continent, countryB.Continent))
	maySea := ca != cb && len(cables) > 0 && seg.DistanceKM > 3000

	if !mustSea && !maySea {
		seg.Medium, seg.Confidence = model.MediumTerrestrialFiber, 0.7
		if a.loc.Confidence < 0.5 || b.loc.Confidence < 0.5 {
			seg.Confidence = 0.4
		}
		seg.TerrestrialISP = operator(a, b)
		return
	}

	// Confidence that the medium is subsea; which cable is a separate
	// question answered by subsea_cable.probability.
	seg.Medium, seg.Confidence = model.MediumSubseaCable, 0.75
	if mustSea {
		seg.Confidence = 0.85
	}
	sameCountry := func(cc string) func(string) bool { return func(x string) bool { return x == cc } }
	ranked := rankCables(cables, a, b, seg.DeltaRTTMS, sameCountry(ca), sameCountry(cb))
	if len(ranked) == 0 && mustSea && okA && okB {
		// No cable lands in both countries: the crossing landed elsewhere on
		// the far continent (e.g. New Jersey -> Ireland, then on to Denmark
		// by land). Rank cables from A's country to anywhere on B's continent;
		// far landings score lower through the RTT and proximity terms.
		onContinent := func(cont string) func(string) bool {
			return func(x string) bool { c, ok := in.Ref.Country(x); return ok && c.Continent == cont }
		}
		ranked = rankCables(in.Ref.CablesFrom(ca), a, b, seg.DeltaRTTMS, sameCountry(ca), onContinent(countryB.Continent))
		if len(ranked) == 0 {
			ranked = rankCables(in.Ref.CablesFrom(cb), a, b, seg.DeltaRTTMS, onContinent(countryA.Continent), sameCountry(cb))
		}
	}
	if len(ranked) == 0 {
		return // subsea, but no known cable between these countries
	}
	top := ranked[0]
	sc := &model.SubseaCable{CableID: top.cable.ID, Name: top.cable.Name, LandingA: top.landA.ID, LandingB: top.landB.ID,
		Probability: round3(top.prob), OwnerMatch: top.owner, Alternatives: []model.CableCandidate{}}
	for _, c := range ranked[1:min(len(ranked), 5)] {
		sc.Alternatives = append(sc.Alternatives, model.CableCandidate{CableID: c.cable.ID, Name: c.cable.Name, Probability: round3(c.prob)})
	}
	seg.SubseaCable = sc
	var lc []string
	for _, l := range top.cable.Landings {
		lc = append(lc, l.CountryISO2)
	}
	s.cables[segIdx] = lc
}

type rankedCable struct {
	cable        store.Cable
	landA, landB store.LandingPoint
	owner        bool
	prob         float64
}

// rankCables scores every cable connecting the two countries by RTT fit,
// operator ownership and landing proximity, then normalises to probabilities.
func rankCables(cables []store.Cable, a, b node, deltaRTT float64, okA, okB func(string) bool) []rankedCable {
	keys := orgKeywords(a.org, b.org)
	var out []rankedCable
	total := 0.0
	for _, c := range cables {
		la, da := nearestLanding(c, a.loc, okA)
		lb, db := nearestLanding(c, b.loc, okB)
		if la.ID == "" || lb.ID == "" {
			continue
		}
		// Cable routes run ~25% longer than the great circle between landings.
		pathKM := da + 1.25*haversineKM(la.Lat, la.Lon, lb.Lat, lb.Lon) + db
		expected := minFeasibleRTT(pathKM)
		rttFit := math.Exp(-math.Abs(deltaRTT-expected) / (0.25*expected + 5))
		owner := ownerMatch(c.Owners, keys)
		ownerScore := 0.0
		if owner {
			ownerScore = 1
		}
		proximity := math.Exp(-(da + db) / 1000)
		score := 0.45*rttFit + 0.35*ownerScore + 0.15*proximity + 0.05
		out = append(out, rankedCable{cable: c, landA: la, landB: lb, owner: owner, prob: score})
		total += score
	}
	for i := range out {
		out[i].prob /= total
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].prob != out[j].prob {
			return out[i].prob > out[j].prob
		}
		return out[i].cable.ID < out[j].cable.ID
	})
	return out
}

func nearestLanding(c store.Cable, loc *model.Location, ok func(string) bool) (store.LandingPoint, float64) {
	best, bestD := store.LandingPoint{}, math.Inf(1)
	for _, l := range c.Landings {
		if !ok(l.CountryISO2) {
			continue
		}
		if d := haversineKM(loc.Lat, loc.Lon, l.Lat, l.Lon); d < bestD {
			best, bestD = l, d
		}
	}
	return best, bestD
}

var genericOrgWords = map[string]bool{
	"the": true, "inc": true, "llc": true, "ltd": true, "limited": true, "corp": true, "corporation": true,
	"communications": true, "telecom": true, "telecommunications": true, "network": true, "networks": true,
	"global": true, "international": true, "group": true, "company": true, "america": true, "services": true,
}

// orgKeywords pulls distinctive words from iptoasn-style AS names, e.g.
// "AS6453 - TATA COMMUNICATIONS (AMERICA) INC" -> ["tata"].
func orgKeywords(orgs ...string) []string {
	var out []string
	for _, o := range orgs {
		if i := strings.Index(o, " - "); i >= 0 {
			o = o[i+3:]
		}
		for _, w := range strings.FieldsFunc(strings.ToLower(o), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			if len(w) >= 3 && !genericOrgWords[w] && !strings.HasPrefix(w, "as") {
				out = append(out, w)
				break // first distinctive word only
			}
		}
	}
	return out
}

func ownerMatch(owners []string, keys []string) bool {
	for _, o := range owners {
		lo := strings.ToLower(o)
		for _, k := range keys {
			if strings.Contains(lo, k) {
				return true
			}
		}
	}
	return false
}

func operator(a, b node) *model.Operator {
	n := b
	if b.asn == 0 {
		n = a
	}
	if n.asn == 0 {
		return nil
	}
	return &model.Operator{ASN: n.asn, Name: n.org}
}

// health colours a segment: green / amber / red, grey when unknown.
func health(seg model.Segment, b node) string {
	if seg.Medium == model.MediumUnknown && seg.DistanceKM == 0 {
		return "grey"
	}
	excess := 1.0
	if seg.MinFeasibleRTTMS >= 1 {
		excess = seg.DeltaRTTMS / seg.MinFeasibleRTTMS
	} else if seg.DeltaRTTMS > 5 {
		excess = seg.DeltaRTTMS / 5 // short hop that still adds >5 ms
	}
	switch {
	case b.loss < 0.5 && b.jitter < 10 && excess < 2.0:
		return "green"
	case b.loss < 2 && b.jitter < 30 && excess < 3.5:
		return "amber"
	}
	return "red"
}

func uniq(vals ...string) []string {
	out := []string{}
	for _, v := range vals {
		if v != "" && (len(out) == 0 || out[len(out)-1] != v) {
			out = append(out, v)
		}
	}
	return out
}
