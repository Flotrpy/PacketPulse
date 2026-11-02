package mobile

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
)

// Hub is a candidate regional gateway site where an MNO's packet core
// (PGW/UPF) hands traffic to the internet.
type Hub struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	City       string  `json:"city"`
	Country    string  `json:"country"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	Kind       string  `json:"kind"` // operator_facility | operator_ixp | country_interconnection_metro
	Facilities int     `json:"facilities"`
	Confidence float64 `json:"confidence"`
}

// HubDirectory derives gateway hubs from PeeringDB:
//  1. facilities where the operator's ASN is present (netfac), grouped by metro;
//  2. else facilities of the IXPs the ASN peers at (netixlan -> ixfac);
//  3. else the country's main interconnection metros (most PeeringDB
//     facilities). Operators concentrate packet gateways there, but this is a
//     country-level estimate and is labelled as such.
type HubDirectory struct {
	facs        map[int]facility
	netFacs     map[int][]int // asn -> fac ids
	netIXs      map[int][]int // asn -> ix ids
	ixFacs      map[int][]int // ix id -> fac ids
	countryHubs map[string][]Hub
}

type facility struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	City     string  `json:"city"`
	Country  string  `json:"country"`
	Lat      float64 `json:"latitude"`
	Lon      float64 `json:"longitude"`
	hasCoord bool
}

// LoadHubDirectory reads PeeringDB fac, netfac, netixlan and ixfac dumps (optionally .gz).
func LoadHubDirectory(facPath, netfacPath, netixlanPath, ixfacPath string) (*HubDirectory, error) {
	var facs struct {
		Data []struct {
			ID        int      `json:"id"`
			Name      string   `json:"name"`
			City      string   `json:"city"`
			Country   string   `json:"country"`
			Latitude  *float64 `json:"latitude"`
			Longitude *float64 `json:"longitude"`
		}
	}
	if err := readJSON(facPath, &facs); err != nil {
		return nil, fmt.Errorf("fac: %w", err)
	}
	var netfac struct {
		Data []struct {
			LocalASN int `json:"local_asn"`
			FacID    int `json:"fac_id"`
		}
	}
	if err := readJSON(netfacPath, &netfac); err != nil {
		return nil, fmt.Errorf("netfac: %w", err)
	}
	var netix struct {
		Data []struct {
			ASN  int `json:"asn"`
			IXID int `json:"ix_id"`
		}
	}
	if err := readJSON(netixlanPath, &netix); err != nil {
		return nil, fmt.Errorf("netixlan: %w", err)
	}
	var ixfac struct {
		Data []struct {
			IXID  int `json:"ix_id"`
			FacID int `json:"fac_id"`
		}
	}
	if err := readJSON(ixfacPath, &ixfac); err != nil {
		return nil, fmt.Errorf("ixfac: %w", err)
	}

	d := &HubDirectory{facs: map[int]facility{}, netFacs: map[int][]int{}, netIXs: map[int][]int{}, ixFacs: map[int][]int{}}
	for _, f := range facs.Data {
		fc := facility{ID: f.ID, Name: f.Name, City: strings.TrimSpace(f.City), Country: f.Country}
		if f.Latitude != nil && f.Longitude != nil && (*f.Latitude != 0 || *f.Longitude != 0) {
			fc.Lat, fc.Lon, fc.hasCoord = *f.Latitude, *f.Longitude, true
		}
		d.facs[f.ID] = fc
	}
	for _, x := range netfac.Data {
		d.netFacs[x.LocalASN] = append(d.netFacs[x.LocalASN], x.FacID)
	}
	for _, x := range netix.Data {
		d.netIXs[x.ASN] = appendUniqueInt(d.netIXs[x.ASN], x.IXID)
	}
	for _, x := range ixfac.Data {
		d.ixFacs[x.IXID] = append(d.ixFacs[x.IXID], x.FacID)
	}
	d.countryHubs = d.buildCountryHubs(6)
	return d, nil
}

func appendUniqueInt(s []int, v int) []int {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// metroGroup clusters facilities by (country, city) and returns hubs at the
// facility centroid of each metro.
func (d *HubDirectory) metroGroup(facIDs []int, country, kind string, conf float64) []Hub {
	type acc struct {
		lat, lon float64
		n        int
		names    []string
		city, cc string
	}
	groups := map[string]*acc{}
	for _, id := range facIDs {
		f, ok := d.facs[id]
		if !ok || !f.hasCoord || (country != "" && f.Country != country) {
			continue
		}
		k := f.Country + "|" + strings.ToLower(f.City)
		g := groups[k]
		if g == nil {
			g = &acc{city: f.City, cc: f.Country}
			groups[k] = g
		}
		g.lat += f.Lat
		g.lon += f.Lon
		g.n++
		if len(g.names) < 3 {
			g.names = append(g.names, f.Name)
		}
	}
	var out []Hub
	for k, g := range groups {
		out = append(out, Hub{
			ID: strings.NewReplacer("|", "-", " ", "-").Replace(strings.ToLower(k)), Name: strings.Join(g.names, "; "), City: g.city, Country: g.cc,
			Lat: round5(g.lat / float64(g.n)), Lon: round5(g.lon / float64(g.n)), Kind: kind, Facilities: g.n, Confidence: conf,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Facilities != out[j].Facilities {
			return out[i].Facilities > out[j].Facilities
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (d *HubDirectory) buildCountryHubs(top int) map[string][]Hub {
	byCC := map[string][]int{}
	for id, f := range d.facs {
		byCC[f.Country] = append(byCC[f.Country], id)
	}
	out := map[string][]Hub{}
	for cc, ids := range byCC {
		hubs := d.metroGroup(ids, cc, "country_interconnection_metro", 0.35)
		if len(hubs) > top {
			hubs = hubs[:top]
		}
		for i := range hubs {
			hubs[i].Name = fmt.Sprintf("%s interconnection metro (%d PeeringDB facilities)", hubs[i].City, hubs[i].Facilities)
		}
		out[cc] = hubs
	}
	return out
}

// HubsFor returns gateway hub candidates for an operator, most specific
// evidence first (see HubDirectory).
func (d *HubDirectory) HubsFor(asn int, country string) []Hub {
	if h := d.metroGroup(d.netFacs[asn], country, "operator_facility", 0.8); len(h) > 0 {
		return h
	}
	var ixf []int
	for _, ix := range d.netIXs[asn] {
		ixf = append(ixf, d.ixFacs[ix]...)
	}
	if h := d.metroGroup(ixf, country, "operator_ixp", 0.6); len(h) > 0 {
		return h
	}
	return d.countryHubs[country]
}

// Nearest returns the hub closest to p and its distance.
func Nearest(hubs []Hub, p gis.LatLon) (Hub, float64, bool) {
	best, bd := Hub{}, math.Inf(1)
	for _, h := range hubs {
		if dd := gis.HaversineKM(p, gis.LatLon{Lat: h.Lat, Lon: h.Lon}); dd < bd {
			best, bd = h, dd
		}
	}
	return best, math.Round(bd*10) / 10, !math.IsInf(bd, 1)
}

func round5(v float64) float64 { return math.Round(v*1e5) / 1e5 }
