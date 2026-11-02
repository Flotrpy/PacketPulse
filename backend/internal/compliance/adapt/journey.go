package adapt

import (
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/compliance"
	"github.com/flotrpy/packetpulse/backend/internal/model"
)

// MinJourneyConfidence matches the globe: hop locations below it (e.g.
// registry-country guesses) are not placed on the route.
const MinJourneyConfidence = 0.5

// FromJourney builds the compliance Packet of an ingested PacketJourney,
// classified with the labels assigned at ingestion. Points are the located
// source, every responding hop with a confident location (consecutive hops
// at the same coordinate are merged) and the destination endpoint, each
// timestamped at trace start + min RTT / 2.
func FromJourney(j *model.Journey) compliance.Packet {
	labels := []string{}
	if dc := j.Metadata.DataClassification; dc != nil {
		labels = append(labels, dc.Labels...)
	}
	p := compliance.Packet{PacketID: j.JourneyID, Timestamp: j.Metadata.Timestamp, Classifications: labels, Source: "ingestd"}
	start := j.Metadata.Timestamp
	at := func(rtt float64) time.Time { return start.Add(time.Duration(rtt / 2 * float64(time.Millisecond))) }
	add := func(rp compliance.RoutePoint) {
		if n := len(p.Route); n > 0 && p.Route[n-1].Lat == rp.Lat && p.Route[n-1].Lon == rp.Lon {
			// Same point: keep the later arrival (and the destination role).
			last := &p.Route[n-1]
			if rp.Kind == "destination" {
				last.Kind = "destination"
			}
			return
		}
		rp.Seq = len(p.Route)
		p.Route = append(p.Route, rp)
	}
	if l := j.Metadata.Source.Location; l != nil {
		add(compliance.RoutePoint{Kind: "source", IP: j.Metadata.Source.IP, ASN: j.Metadata.Source.ASN, Operator: j.Metadata.Source.ASOrg,
			Label: label(l.City, l.CountryISO2), Lat: l.Lat, Lon: l.Lon, CountryHint: l.CountryISO2,
			Timestamp: start, TimestampSource: "trace_start", LocationConfidence: l.Confidence, LocationSource: l.Method})
	}
	lastRTT := 0.0
	for _, h := range j.Hops {
		l := h.Location
		if !h.Responded || l == nil || l.Confidence < MinJourneyConfidence {
			continue
		}
		lastRTT = h.RTT.Min
		kind := "hop"
		if h.Role == "destination" {
			kind = "destination"
		}
		add(compliance.RoutePoint{TTL: h.HopNumber, Kind: kind, IP: h.IP, ASN: h.ASN, Operator: h.ASOrg,
			Label: label(l.City, l.CountryISO2), Lat: l.Lat, Lon: l.Lon, CountryHint: l.CountryISO2,
			Timestamp: at(h.RTT.Min), TimestampSource: "probe_sent_plus_half_rtt", LocationConfidence: l.Confidence, LocationSource: l.Method})
	}
	if l := j.Metadata.Destination.Location; l != nil && l.Confidence >= MinJourneyConfidence {
		add(compliance.RoutePoint{Kind: "destination", IP: j.Metadata.Destination.IP, ASN: j.Metadata.Destination.ASN, Operator: j.Metadata.Destination.ASOrg,
			Label: label(l.City, l.CountryISO2), Lat: l.Lat, Lon: l.Lon, CountryHint: l.CountryISO2,
			Timestamp: at(lastRTT), TimestampSource: "probe_sent_plus_half_rtt", LocationConfidence: l.Confidence, LocationSource: l.Method})
	}
	if n := len(p.Route); n > 1 && p.Route[n-1].Kind == "hop" {
		p.Route[n-1].Kind = "destination"
	}
	return p
}
