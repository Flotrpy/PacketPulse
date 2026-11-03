package pipeline

import (
	"context"
	"math"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/store"
)

// ReverseResolver does PTR lookups. *net.Resolver satisfies it.
type ReverseResolver interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Resolve enriches every hop with ASN, reverse DNS, geolocation and role.
type Resolve struct {
	Ref         store.ReferenceStore
	DNS         ReverseResolver // nil disables reverse DNS
	DNSTimeout  time.Duration
	DNSParallel int
	MinRTTSlack float64 // extra km tolerated by the RTT feasibility check
}

func (Resolve) Name() string   { return "resolve" }
func (Resolve) Required() bool { return false }

// Transit-free networks per CAIDA's clique (20261001), plus Hurricane Electric.
var backboneASNs = map[int]bool{
	174: true, 209: true, 286: true, 701: true, 1299: true, 2828: true, 2914: true, 3257: true,
	3320: true, 3356: true, 3491: true, 5511: true, 6453: true, 6461: true, 6762: true, 6830: true,
	7018: true, 12956: true, 6939: true,
}

func (r Resolve) Process(ctx context.Context, s *State) error {
	j := s.Journey
	r.reverseDNS(ctx, s)

	// Endpoints.
	if a, err := netip.ParseAddr(j.Metadata.Source.IP); err == nil && addressClass(a) == "public" {
		if info, ok := r.Ref.LookupASN(a); ok {
			j.Metadata.Source.ASN, j.Metadata.Source.ASOrg = info.ASN, info.Name
		}
	}
	dst := s.Raw.Target
	if info, ok := r.Ref.LookupASN(dst); ok {
		j.Metadata.Destination.ASN, j.Metadata.Destination.ASOrg = info.ASN, info.Name
		if p, ok := r.Ref.CloudProvider(info.ASN); ok {
			j.Metadata.Destination.Cloud = p
		}
	}
	if g, ok := r.Ref.LookupGeo(dst); ok {
		j.Metadata.Destination.Location = &model.Location{Lat: g.Lat, Lon: g.Lon, City: g.City, CountryISO2: g.CountryISO2,
			AccuracyKM: g.AccuracyKM, Method: "prefix_db", Confidence: 0.7}
	}

	src := j.Metadata.Source.Location
	// The destination's database location must be reachable at light speed
	// in its RTT too: anycast services (1.1.1.1, 8.8.8.8, CDNs) answer from
	// a nearby site while databases list a single registered city.
	if dl := j.Metadata.Destination.Location; dl != nil && src != nil {
		if rtt := destMinRTT(s); rtt > 0 {
			if !r.feasible(src, dl, rtt) {
				j.Metadata.Destination.Location = nil
			}
		} else {
			// Not reached: nothing bounds the distance, so don't draw it as fact.
			dl.Confidence = min(dl.Confidence, 0.4)
		}
	}
	env := rttFloor(s)
	var prev *model.Location // last located public hop (the path so far)
	seenMobile := false
	for i := range j.Hops {
		h := &j.Hops[i]
		if !h.Responded {
			continue
		}
		addr := s.hopAddr[i]
		if h.AddressClass != "public" {
			if src != nil {
				l := *src
				l.Method, l.Confidence, l.AccuracyKM = "inherited", 0.5, max(src.AccuracyKM, 30)
				h.Location = &l
			}
			switch {
			case h.Role == "destination":
			case h.AddressClass == "cgnat":
				h.Role = "cgnat"
			case i == 0:
				h.Role = "lan_gateway"
			default:
				h.Role = "private"
			}
			continue
		}

		info, hasASN := r.Ref.LookupASN(addr)
		if hasASN {
			h.ASN, h.ASOrg, h.RegistryCountry = info.ASN, info.Name, info.RegistryCountry
		}
		h.Location = r.locate(addr, h, src, prev, env[i])
		if h.Location != nil && h.Location.Confidence >= 0.5 {
			prev = h.Location
		}
		if h.Location == nil && h.Role == "destination" && j.Metadata.Destination.Location != nil {
			l := *j.Metadata.Destination.Location
			h.Location = &l
		}

		if h.Role == "destination" {
			continue
		}
		if _, ok := r.Ref.MobileCarrier(h.ASN); ok && hasASN {
			if !seenMobile {
				h.Role = "mno_gateway"
				seenMobile = true
			} else {
				h.Role = "mno_core"
			}
		} else if _, ok := r.Ref.CloudProvider(h.ASN); ok && hasASN {
			h.Role = "cloud_edge"
		} else if backboneASNs[h.ASN] {
			h.Role = "backbone"
		} else if hasASN {
			h.Role = "isp"
		}
	}
	r.dropDetours(s, env)
	if src != nil {
		j.Metadata.Source.Location = src
	}
	return nil
}

// rttFloor[i] is the smallest min-RTT measured at hop i or any later hop.
// RTT noise (slow ICMP generation, queuing) only ever adds delay, and the
// path to a later hop passes through hop i, so this is a tighter and more
// robust bound on how far away hop i can be than hop i's own RTT.
func rttFloor(s *State) []float64 {
	out := make([]float64, len(s.Journey.Hops))
	floor := math.Inf(1)
	for i := len(s.Journey.Hops) - 1; i >= 0; i-- {
		if s.Journey.Hops[i].Responded && s.hopMinMS[i] > 0 {
			floor = math.Min(floor, s.hopMinMS[i])
		}
		if math.IsInf(floor, 1) {
			out[i] = 0
		} else {
			out[i] = floor
		}
	}
	return out
}

// dropDetours removes database locations that send the path on a detour
// the RTTs can't pay for: e.g. London -> "Paris" -> London within 1 ms, or
// Stockholm -> "Phoenix" -> Helsinki. Router-name hints are kept.
func (r Resolve) dropDetours(s *State, env []float64) {
	hops := s.Journey.Hops
	located := func(i int) bool {
		return hops[i].Responded && hops[i].Location != nil && hops[i].Location.Confidence >= 0.5 && hops[i].AddressClass == "public"
	}
	for i := range hops {
		if !located(i) || hops[i].Location.Method != "prefix_db" {
			continue
		}
		p, n := -1, -1
		for k := i - 1; k >= 0; k-- {
			if located(k) {
				p = k
				break
			}
		}
		for k := i + 1; k < len(hops); k++ {
			if located(k) {
				n = k
				break
			}
		}
		if p < 0 || n < 0 {
			continue
		}
		lp, li, ln := hops[p].Location, hops[i].Location, hops[n].Location
		detour := haversineKM(lp.Lat, lp.Lon, li.Lat, li.Lon) + haversineKM(li.Lat, li.Lon, ln.Lat, ln.Lon) - haversineKM(lp.Lat, lp.Lon, ln.Lat, ln.Lon)
		budget := math.Max(0, env[n]-env[p])/2*fiberKMPerMS + 300 + li.AccuracyKM + math.Max(lp.AccuracyKM, ln.AccuracyKM)
		if detour > budget {
			hops[i].Location = nil
		}
	}
}

// locate picks the best location evidence that passes the RTT check:
// router hostname hint, then prefix database, then registry-country centroid.
func (r Resolve) locate(addr netip.Addr, h *model.Hop, src, prev *model.Location, minRTT float64) *model.Location {
	var cands []*model.Location
	if h.Hostname != "" {
		if c, _, ok := r.Ref.MatchHostname(h.Hostname); ok {
			cands = append(cands, &model.Location{Lat: c.Lat, Lon: c.Lon, City: c.Name, CountryISO2: c.CountryISO2, AccuracyKM: 30, Method: "rdns_hint", Confidence: 0.85})
		}
	}
	if g, ok := r.Ref.LookupGeo(addr); ok {
		cands = append(cands, &model.Location{Lat: g.Lat, Lon: g.Lon, City: g.City, CountryISO2: g.CountryISO2, AccuracyKM: g.AccuracyKM, Method: "prefix_db", Confidence: 0.7})
	}
	if h.RegistryCountry != "" {
		if c, ok := r.Ref.Country(h.RegistryCountry); ok {
			cands = append(cands, &model.Location{Lat: c.Lat, Lon: c.Lon, CountryISO2: c.ISO2, AccuracyKM: 1000, Method: "registry_country", Confidence: 0.2})
		}
	}
	for _, c := range cands {
		if src == nil || minRTT <= 0 {
			return c
		}
		if !r.feasible(src, c, minRTT) {
			continue
		}
		// The packet reached this router via the previous one, so the
		// whole path source -> previous -> here must fit in the RTT too.
		// Router-name hints are trusted over a possibly wrong previous hop;
		// registry-country guesses are not physical locations at all.
		if prev != nil && c.Method == "prefix_db" {
			via := haversineKM(src.Lat, src.Lon, prev.Lat, prev.Lon) + haversineKM(prev.Lat, prev.Lon, c.Lat, c.Lon)
			if via > minRTT/2*fiberKMPerMS+c.AccuracyKM+prev.AccuracyKM+src.AccuracyKM+r.MinRTTSlack {
				continue
			}
		}
		return c
	}
	return nil
}

// feasible: a reply cannot come from farther than light in fiber reaches in minRTT/2.
func (r Resolve) feasible(src, c *model.Location, minRTT float64) bool {
	maxKM := minRTT/2*fiberKMPerMS + c.AccuracyKM + src.AccuracyKM + r.MinRTTSlack
	return haversineKM(src.Lat, src.Lon, c.Lat, c.Lon) <= maxKM
}

// destMinRTT is the min RTT of the hop that answered as the destination (0 if none).
func destMinRTT(s *State) float64 {
	for i, h := range s.Journey.Hops {
		if h.Role == "destination" && h.Responded && s.hopMinMS[i] > 0 {
			return s.hopMinMS[i]
		}
	}
	return 0
}

func (r Resolve) reverseDNS(ctx context.Context, s *State) {
	if r.DNS == nil {
		return
	}
	timeout := r.DNSTimeout
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	par := r.DNSParallel
	if par <= 0 {
		par = 16
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i := range s.Journey.Hops {
		h := &s.Journey.Hops[i]
		if !h.Responded || h.AddressClass != "public" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(h *model.Hop, ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if names, err := r.DNS.LookupAddr(cctx, ip); err == nil && len(names) > 0 {
				h.Hostname = strings.TrimSuffix(names[0], ".")
			}
		}(h, h.IP)
	}
	wg.Wait()
}

// SystemResolver returns the OS resolver for reverse DNS.
func SystemResolver() ReverseResolver { return net.DefaultResolver }
