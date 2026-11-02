// Package adapt converts enrichment output into compliance Packets, keeping
// the compliance engine itself free of PacketPulse dependencies.
package adapt

import (
	"fmt"
	"math"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/compliance"
	"github.com/flotrpy/packetpulse/backend/internal/enrich"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/subsea"
)

// Location confidence by the enricher's location source.
var sourceConfidence = map[string]float64{
	"device":          1.0,
	"opencellid_api":  0.95,
	"opencellid_dump": 0.95,
	"rdns_hint":       0.85,
	"maxmind":         0.7,
	"ipinfo":          0.7,
	"mno_gateway_hub": 0.6,
	"static":          0.7,
}

// FromInfraMap builds the transit route of an enriched trace:
//   - the device (if located);
//   - every hop whose location the enricher accepted, timestamped at
//     trace start + min RTT / 2 (one-way arrival estimate);
//   - for subsea links, the landing stations on the matched cable stretch,
//     timestamped by interpolation along the link and weighted by how
//     likely the candidates that land in that country are.
func FromInfraMap(m *enrich.InfraMap, packetID string, classifications []string) compliance.Packet {
	if packetID == "" {
		packetID = m.MapID
	}
	p := compliance.Packet{PacketID: packetID, Timestamp: m.Trace.StartedAt, Classifications: classifications, Source: "infraenrich"}
	start := m.Trace.StartedAt
	seq := 0
	add := func(rp compliance.RoutePoint) {
		rp.Seq = seq
		seq++
		p.Route = append(p.Route, rp)
	}
	at := func(rttMS float64) time.Time {
		return start.Add(time.Duration(rttMS / 2 * float64(time.Millisecond)))
	}

	if d := m.Device; d != nil && d.Location != nil {
		add(compliance.RoutePoint{Kind: "source", Label: "device", Lat: d.Location.Lat, Lon: d.Location.Lon, CountryHint: d.Country,
			Timestamp: start, TimestampSource: "trace_start", LocationConfidence: confidence(d.Location.Source), LocationSource: d.Location.Source})
	}

	hopRTT := map[int]float64{}
	for _, h := range m.Hops {
		hopRTT[h.TTL] = h.RTT.Min
	}
	linkInto := map[int]enrich.LinkEnriched{}
	for _, l := range m.Links {
		linkInto[l.To.TTL] = l
	}
	for _, h := range m.Hops {
		if h.LocationStatus != "accepted" || h.Geo == nil {
			continue
		}
		if l, ok := linkInto[h.TTL]; ok && l.Medium == "subsea_cable" && l.Subsea != nil && l.Subsea.Selected != nil {
			addLandings(add, l, at(hopRTT[l.From.TTL]), at(h.RTT.Min))
		}
		kind := "hop"
		if h.Role == "destination" {
			kind = "destination"
		}
		src := h.Geo.Sources["location"]
		add(compliance.RoutePoint{TTL: h.TTL, Kind: kind, IP: h.IP, ASN: h.ASN, Operator: h.ASOrg,
			Label: label(h.Geo.City, h.Geo.CountryISO2), Lat: h.Geo.Lat, Lon: h.Geo.Lon, CountryHint: h.Geo.CountryISO2,
			Timestamp: at(h.RTT.Min), TimestampSource: "probe_sent_plus_half_rtt", LocationConfidence: confidence(src), LocationSource: src})
	}
	return p
}

func addLandings(add func(compliance.RoutePoint), l enrich.LinkEnriched, t0, t1 time.Time) {
	sel := l.Subsea.Selected
	// Probability that traffic touches a country = sum of the probabilities
	// of the candidate cables that land there on their matched stretch.
	countryP := map[string]float64{}
	for _, c := range append([]subsea.Candidate{*sel}, l.Subsea.Alternatives...) {
		seen := map[string]bool{}
		for _, ld := range c.RouteLandings {
			if !seen[ld.Country] {
				seen[ld.Country] = true
				countryP[ld.Country] += c.Probability
			}
		}
	}
	n := len(sel.RouteLandings)
	for i, ld := range sel.RouteLandings {
		// Landings in the link's own end countries add nothing.
		if (i == 0 && ld.Country == l.From.ISO2) || (i == n-1 && ld.Country == l.To.ISO2) {
			continue
		}
		frac := float64(i+1) / float64(n+1)
		ts := t0.Add(time.Duration(frac * float64(t1.Sub(t0))))
		add(compliance.RoutePoint{Kind: "subsea_landing", Operator: sel.Name, Label: fmt.Sprintf("%s (%s landing)", ld.Name, sel.Name),
			Lat: ld.Lat, Lon: ld.Lon, CountryHint: ld.Country, Timestamp: ts, TimestampSource: "interpolated_along_cable",
			LocationConfidence: math.Min(0.95, math.Round(countryP[ld.Country]*1000)/1000), LocationSource: "subsea_cable_match"})
	}
}

func confidence(src string) float64 {
	if c, ok := sourceConfidence[src]; ok {
		return c
	}
	return 0.5
}

func label(city, cc string) string {
	if city == "" {
		return cc
	}
	if cc == "" {
		return city
	}
	return city + ", " + cc
}
