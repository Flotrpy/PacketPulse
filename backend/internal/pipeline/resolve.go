package pipeline

import (
	"context"
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
		h.Location = r.locate(addr, h, src, s.hopMinMS[i])
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
	if src != nil {
		j.Metadata.Source.Location = src
	}
	return nil
}

// locate picks the best location evidence that passes the RTT check:
// router hostname hint, then prefix database, then registry-country centroid.
func (r Resolve) locate(addr netip.Addr, h *model.Hop, src *model.Location, minRTT float64) *model.Location {
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
		// A reply cannot come from farther than light reaches in minRTT/2.
		maxKM := minRTT/2*fiberKMPerMS + c.AccuracyKM + src.AccuracyKM + r.MinRTTSlack
		if haversineKM(src.Lat, src.Lon, c.Lat, c.Lon) <= maxKM {
			return c
		}
	}
	return nil
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
