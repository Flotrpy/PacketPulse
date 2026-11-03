// Package enrich decorates raw traceroute hops with infrastructure telemetry:
// IP geolocation and ASN (MaxMind / IPinfo via geoip.Resolver), ocean
// crossing detection against Natural Earth land polygons, submarine cable
// matching against a TeleGeography-shaped GeoJSON dataset, and cellular
// first-leg detection with regional MNO gateway hubs (PeeringDB, MCC-MNC,
// OpenCellID).
package enrich

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/geoip"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/mobile"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/subsea"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

// HostnameLocator turns router hostnames into a city location (e.g. CAIDA
// Hoiho rules or store.MemoryReference.MatchHostname).
type HostnameLocator interface {
	LocateHostname(host string) (lat, lon float64, city, iso2 string, ok bool)
}

// ReverseResolver does PTR lookups (*net.Resolver satisfies it).
type ReverseResolver interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Enricher holds the data sources. Every field except GeoIP and Land is
// optional; missing sources simply leave their parts of the map empty.
type Enricher struct {
	GeoIP     geoip.Resolver
	Land      *gis.LandIndex
	Cables    *subsea.Matcher
	MNO       *mobile.Registry
	Hubs      *mobile.HubDirectory
	Cells     mobile.CellLocator
	Hostnames HostnameLocator
	DNS       ReverseResolver
	Datasets  map[string]string // provenance labels

	Concurrency  int
	DNSTimeout   time.Duration
	SlackKM      float64 // tolerance added to light-speed checks
	MetroKM      float64 // links shorter than this are metro fiber
	CrossingOpts gis.CrossingOptions
}

// Device is what the agent knows about the measuring device.
type Device struct {
	Lat, Lon   float64
	HasFix     bool
	AccuracyKM float64
	Country    string
	AccessType string // wired | wifi | cellular | unknown
	MCC, MNC   string
	Cell       *mobile.CellKey
}

// Input is one trace to enrich.
type Input struct {
	Trace     *traceroute.RawTrace
	Device    *Device
	Hostnames map[string]string // optional pre-resolved PTR names by IP
}

type node struct {
	hopIdx int // index into hops, -1 for the device
	ttl    int
	ip     string
	label  string
	p      gis.LatLon
	acc    float64
	iso2   string
	rttMS  float64
	asn    int
	org    string
}

// Enrich builds the InfraMap for one trace.
func (e *Enricher) Enrich(ctx context.Context, in Input) (*InfraMap, error) {
	if in.Trace == nil {
		return nil, errors.New("enrich: nil trace")
	}
	if e.GeoIP == nil || e.Land == nil {
		return nil, errors.New("enrich: GeoIP resolver and land index are required")
	}
	t0 := time.Now()
	rt := in.Trace
	m := &InfraMap{
		SchemaVersion: InfraMapSchemaVersion, MapID: newID(), GeneratedAt: time.Now().UTC(),
		Trace: TraceInfo{Target: rt.Target.String(), Protocol: string(rt.Options.Protocol), StartedAt: rt.StartedAt.UTC(),
			FinishedAt: rt.FinishedAt.UTC(), ReachedDestination: rt.Reached, DestinationTTL: rt.DestinationTTL},
		Links: []LinkEnriched{}, Mapping: []MappingEntry{},
		Provenance: Provenance{GeoIP: e.GeoIP.Name(), Datasets: e.Datasets},
	}
	if rt.Source.IsValid() {
		m.Trace.Source = rt.Source.String()
	}

	sums := rt.Hops()
	m.Trace.HopCount = len(sums)
	m.Hops = make([]HopEnriched, len(sums))
	for i, hs := range sums {
		m.Hops[i] = baseHop(hs)
	}
	m.Device = e.device(ctx, in.Device)

	e.resolveHops(ctx, m.Hops, sums, in.Hostnames)
	e.checkLocations(m)
	m.Mobile = e.detectMobile(m, in.Device)
	e.assignRoles(m)
	e.buildLinks(m)
	e.buildMapping(m)
	e.summarize(m)
	m.Provenance.EnrichmentMS = math.Round(float64(time.Since(t0).Microseconds())) / 1000
	return m, nil
}

func baseHop(hs traceroute.HopSummary) HopEnriched {
	h := HopEnriched{TTL: hs.TTL, Responded: hs.Addr.IsValid(), Role: "unknown", AddressClass: "unknown", LocationStatus: "no_reply",
		RTT: RTT{Samples: []float64{}}}
	if hs.Sent > 0 {
		h.LossPct = round1(100 * float64(hs.Sent-hs.Received) / float64(hs.Sent))
	}
	if !h.Responded {
		return h
	}
	h.IP = hs.Addr.String()
	for _, a := range hs.Alternates {
		h.Alternates = append(h.Alternates, a.String())
	}
	h.AddressClass = addressClass(hs.Addr)
	var sum float64
	for i, r := range hs.RTTs {
		v := round3(float64(r) / float64(time.Millisecond))
		h.RTT.Samples = append(h.RTT.Samples, v)
		sum += v
		if i == 0 || v < h.RTT.Min {
			h.RTT.Min = v
		}
		h.RTT.Max = math.Max(h.RTT.Max, v)
		if i > 0 {
			h.RTT.Jitter += math.Abs(v - h.RTT.Samples[i-1])
		}
	}
	if n := len(hs.RTTs); n > 0 {
		h.RTT.Avg = round3(sum / float64(n))
		if n > 1 {
			h.RTT.Jitter = round3(h.RTT.Jitter / float64(n-1))
		}
	}
	return h
}

func (e *Enricher) device(ctx context.Context, d *Device) *DeviceInfo {
	if d == nil {
		return nil
	}
	info := &DeviceInfo{Country: d.Country, AccessType: d.AccessType, MCC: d.MCC, MNC: d.MNC}
	if info.AccessType == "" {
		info.AccessType = "unknown"
	}
	if d.HasFix {
		acc := d.AccuracyKM
		if acc == 0 {
			acc = 1
		}
		info.Location = &Point{Lat: d.Lat, Lon: d.Lon, AccuracyKM: acc, Source: "device"}
	}
	if d.Cell != nil && e.Cells != nil {
		c, err := e.Cells.Locate(ctx, *d.Cell)
		if err != nil {
			info.CellError = err.Error()
		} else {
			info.Cell = &c
			// The serving tower beats a coarse device fix.
			if info.Location == nil || info.Location.AccuracyKM > float64(c.RangeM)/1000 {
				info.Location = &Point{Lat: c.Lat, Lon: c.Lon, AccuracyKM: math.Max(0.1, float64(c.RangeM)/1000), Source: c.Source}
			}
		}
	}
	return info
}

// resolveHops runs PTR and GeoIP lookups for public hops concurrently.
func (e *Enricher) resolveHops(ctx context.Context, hops []HopEnriched, sums []traceroute.HopSummary, ptr map[string]string) {
	par := e.Concurrency
	if par <= 0 {
		par = 16
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i := range hops {
		h := &hops[i]
		if !h.Responded {
			continue
		}
		if h.AddressClass != "public" {
			h.LocationStatus = "non_routable"
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(h *HopEnriched, addr netip.Addr) {
			defer wg.Done()
			defer func() { <-sem }()
			h.Hostname = ptr[h.IP]
			if h.Hostname == "" && e.DNS != nil {
				timeout := e.DNSTimeout
				if timeout == 0 {
					timeout = 1500 * time.Millisecond
				}
				cctx, cancel := context.WithTimeout(ctx, timeout)
				if names, err := e.DNS.LookupAddr(cctx, h.IP); err == nil && len(names) > 0 {
					h.Hostname = strings.TrimSuffix(names[0], ".")
				}
				cancel()
			}
			rec, err := e.GeoIP.Lookup(ctx, addr)
			if err != nil {
				h.LocationStatus = "no_data"
				if !errors.Is(err, geoip.ErrNotFound) {
					h.LocationNote = err.Error()
				}
			} else {
				r := rec
				h.Geo = &r
				h.ASN, h.ASOrg, h.ISP = r.ASN, r.ASOrg, r.ISP
				h.LocationStatus = "accepted"
				if !r.HasLocation {
					h.LocationStatus = "no_data"
				}
			}
			// Router hostnames name their city (ae-1.r20.lsanca07...): for
			// backbone routers this beats registry-driven geolocation.
			if e.Hostnames != nil && h.Hostname != "" {
				if lat, lon, city, iso, ok := e.Hostnames.LocateHostname(h.Hostname); ok {
					if h.Geo == nil {
						h.Geo = &geoip.Record{IP: h.IP}
					}
					if !h.Geo.HasLocation || gis.HaversineKM(gis.LatLon{Lat: lat, Lon: lon}, gis.LatLon{Lat: h.Geo.Lat, Lon: h.Geo.Lon}) > 100 {
						h.Geo.Lat, h.Geo.Lon, h.Geo.HasLocation, h.Geo.AccuracyKM = lat, lon, true, 30
						h.Geo.City, h.Geo.CountryISO2, h.Geo.Region = city, iso, ""
						if h.Geo.Sources == nil {
							h.Geo.Sources = map[string]string{}
						}
						h.Geo.Sources["location"] = "rdns_hint"
					}
					h.LocationStatus = "accepted"
				}
			}
		}(h, sums[i].Addr)
	}
	wg.Wait()
}

func (e *Enricher) slack() float64 {
	if e.SlackKM > 0 {
		return e.SlackKM
	}
	return 100
}

// checkLocations rejects locations that physics rules out.
//  1. light speed: a reply can't come from farther than minRTT/2 of fiber from the device;
//  2. detour: placing hop i between its accepted neighbours must not add more
//     distance than their RTT difference allows.
func (e *Enricher) checkLocations(m *InfraMap) {
	var dev *gis.LatLon
	devAcc := 0.0
	if m.Device != nil && m.Device.Location != nil {
		dev = &gis.LatLon{Lat: m.Device.Location.Lat, Lon: m.Device.Location.Lon}
		devAcc = m.Device.Location.AccuracyKM
	}
	for i := range m.Hops {
		h := &m.Hops[i]
		if h.LocationStatus != "accepted" || dev == nil || h.RTT.Min <= 0 {
			continue
		}
		p := gis.LatLon{Lat: h.Geo.Lat, Lon: h.Geo.Lon}
		maxKM := h.RTT.Min/2*gis.FiberKMPerMS + h.Geo.AccuracyKM + devAcc + e.slack()
		if d := gis.HaversineKM(*dev, p); d > maxKM {
			h.LocationStatus = "rejected_light_speed"
			h.LocationNote = fmt.Sprintf("%.0f km from the device but light covers at most %.0f km in %.1f ms RTT", d, maxKM, h.RTT.Min)
		}
	}

	type pt struct {
		p   gis.LatLon
		rtt float64
		ok  bool
	}
	at := func(i int) pt {
		if i < 0 {
			if dev != nil {
				return pt{*dev, 0, true}
			}
			return pt{}
		}
		h := m.Hops[i]
		if h.LocationStatus != "accepted" {
			return pt{}
		}
		return pt{gis.LatLon{Lat: h.Geo.Lat, Lon: h.Geo.Lon}, h.RTT.Min, true}
	}
	for i := range m.Hops {
		h := &m.Hops[i]
		if h.LocationStatus != "accepted" {
			continue
		}
		prev := pt{}
		for j := i - 1; j >= -1 && !prev.ok; j-- {
			prev = at(j)
		}
		next := pt{}
		for j := i + 1; j < len(m.Hops) && !next.ok; j++ {
			next = at(j)
		}
		if !prev.ok || !next.ok {
			continue
		}
		cur := gis.LatLon{Lat: h.Geo.Lat, Lon: h.Geo.Lon}
		extra := gis.HaversineKM(prev.p, cur) + gis.HaversineKM(cur, next.p) - gis.HaversineKM(prev.p, next.p)
		allowed := math.Max(0, next.rtt-prev.rtt)/2*gis.FiberKMPerMS + 2*h.Geo.AccuracyKM + e.slack()
		if extra > allowed {
			h.LocationStatus = "rejected_detour"
			h.LocationNote = fmt.Sprintf("placing it in %s adds a %.0f km detour between its neighbours; their RTT difference allows %.0f km", nonEmpty(h.Geo.City, h.Geo.CountryISO2), extra, allowed)
		}
	}
}

// detectMobile flags the first leg as cellular when the first public hop
// belongs to a known MNO.
func (e *Enricher) detectMobile(m *InfraMap, d *Device) *MobileLeg {
	var cgnat []int
	gw := -1
	for i, h := range m.Hops {
		if !h.Responded {
			continue
		}
		if h.AddressClass == "cgnat" {
			cgnat = append(cgnat, h.TTL)
		}
		if h.AddressClass == "public" {
			gw = i
			break
		}
	}
	if gw < 0 {
		return nil
	}
	h := m.Hops[gw]
	var op mobile.Operator
	found := false
	var ev []Evidence
	if h.Geo != nil && h.Geo.MobileMCC != "" {
		ev = append(ev, Evidence{"geoip_mobile_codes", h.Geo.MobileMCC + "-" + h.Geo.MobileMNC, 0.9})
		if e.MNO != nil {
			if ops := e.MNO.ByMCCMNC(h.Geo.MobileMCC, h.Geo.MobileMNC); len(ops) > 0 {
				op, found = ops[0], true
			}
		}
		if !found {
			op = mobile.Operator{ASN: h.ASN, Name: nonEmpty(h.ISP, h.ASOrg), Country: h.Geo.CountryISO2, MCC: h.Geo.MobileMCC,
				MNCs: []string{h.Geo.MobileMNC}, Source: "maxmind_isp", Confidence: 0.9}
			found = true
		}
	}
	if e.MNO != nil && h.ASN != 0 {
		if o, ok := e.MNO.ByASN(h.ASN); ok {
			ev = append(ev, Evidence{"mno_asn_registry", fmt.Sprintf("AS%d %s (%s)", o.ASN, o.Name, o.Source), o.Confidence})
			if !found {
				op, found = o, true
			}
		}
	}
	if d != nil && d.MCC != "" && e.MNO != nil {
		for _, o := range e.MNO.ByMCCMNC(d.MCC, d.MNC) {
			agree := o.ASN == h.ASN
			ev = append(ev, Evidence{"device_mcc_mnc", fmt.Sprintf("%s-%s -> AS%d %s (first hop AS%d: %s)", d.MCC, d.MNC, o.ASN, o.Name, h.ASN, map[bool]string{true: "match", false: "mismatch"}[agree]), map[bool]float64{true: 0.9, false: 0.3}[agree]})
			if !found && agree {
				op, found = o, true
			}
			break
		}
	}
	if len(cgnat) > 0 {
		ev = append(ev, Evidence{"cgnat_before_gateway", fmt.Sprintf("TTL %v in 100.64.0.0/10", cgnat), 0.6})
	}
	if d != nil && d.AccessType == "cellular" {
		ev = append(ev, Evidence{"device_access_type", "cellular", 0.7})
	}
	if !found {
		return nil
	}
	conf := op.Confidence
	if len(cgnat) > 0 {
		conf = math.Min(0.99, conf+0.03)
	}
	leg := &MobileLeg{Operator: op, Evidence: ev, GatewayTTL: h.TTL, GatewayIP: h.IP, CGNATTTLs: cgnat, LinkIndexes: []int{}, Confidence: round3(conf)}
	if leg.CGNATTTLs == nil {
		leg.CGNATTTLs = []int{}
	}

	// Anchor: where the subscriber is (tower > device fix), else where GeoIP puts the gateway.
	var anchor *gis.LatLon
	switch {
	case m.Device != nil && m.Device.Cell != nil:
		anchor, leg.HubAnchor = &gis.LatLon{Lat: m.Device.Cell.Lat, Lon: m.Device.Cell.Lon}, "serving_cell"
	case m.Device != nil && m.Device.Location != nil:
		anchor, leg.HubAnchor = &gis.LatLon{Lat: m.Device.Location.Lat, Lon: m.Device.Location.Lon}, "device_location"
	case h.Geo != nil && h.Geo.HasLocation:
		anchor, leg.HubAnchor = &gis.LatLon{Lat: h.Geo.Lat, Lon: h.Geo.Lon}, "gateway_geoip"
	}
	if e.Hubs != nil && anchor != nil {
		country := op.Country
		if country == "" && h.Geo != nil {
			country = h.Geo.CountryISO2
		}
		hubs := e.Hubs.HubsFor(op.ASN, country)
		leg.HubsConsidered = len(hubs)
		if hub, km, ok := mobile.Nearest(hubs, *anchor); ok {
			leg.GatewayHub, leg.HubDistanceKM = &hub, km
			e.placeGatewayAtHub(m, gw, hub)
		}
	}
	return leg
}

// placeGatewayAtHub moves the MNO gateway hop to its regional hub. GeoIP
// places mobile egress pools by registration, often hundreds of km from the
// packet core that serves the subscriber; the hub is the better estimate as
// long as the gateway's RTT allows it.
func (e *Enricher) placeGatewayAtHub(m *InfraMap, gw int, hub mobile.Hub) {
	h := &m.Hops[gw]
	hp := gis.LatLon{Lat: hub.Lat, Lon: hub.Lon}
	if m.Device != nil && m.Device.Location != nil && h.RTT.Min > 0 {
		dev := gis.LatLon{Lat: m.Device.Location.Lat, Lon: m.Device.Location.Lon}
		if gis.HaversineKM(dev, hp) > h.RTT.Min/2*gis.FiberKMPerMS+e.slack() {
			return // the hub is farther than the gateway RTT allows
		}
	}
	rec := geoip.Record{IP: h.IP}
	if h.Geo != nil {
		rec = *h.Geo
		rec.Sources = map[string]string{}
		for k, v := range h.Geo.Sources {
			rec.Sources[k] = v
		}
		if h.Geo.HasLocation {
			d := gis.HaversineKM(gis.LatLon{Lat: h.Geo.Lat, Lon: h.Geo.Lon}, hp)
			if d > 25 {
				h.LocationNote = fmt.Sprintf("GeoIP places this mobile egress address in %s, %.0f km from the assigned hub; mobile address pools are geolocated by registration, so the gateway is placed at the operator's nearest regional hub",
					nonEmpty(h.Geo.City, h.Geo.CountryISO2), d)
			}
		}
	}
	if rec.Sources == nil {
		rec.Sources = map[string]string{}
	}
	rec.Lat, rec.Lon, rec.HasLocation, rec.AccuracyKM = hub.Lat, hub.Lon, true, 50
	rec.City, rec.Region, rec.CountryISO2 = hub.City, "", hub.Country
	rec.Sources["location"] = "mno_gateway_hub"
	h.Geo = &rec
	h.LocationStatus = "accepted"
}

var cloudASNs = map[int]string{16509: "AWS", 14618: "AWS", 15169: "Google", 396982: "Google Cloud", 8075: "Microsoft", 8068: "Microsoft", 31898: "Oracle", 13335: "Cloudflare", 20940: "Akamai", 54113: "Fastly"}

func (e *Enricher) assignRoles(m *InfraMap) {
	target := m.Trace.Target
	for i := range m.Hops {
		h := &m.Hops[i]
		switch {
		case !h.Responded:
			h.Role = "no_reply"
		case h.IP == target:
			h.Role = "destination"
		case h.AddressClass == "cgnat":
			h.Role = "cgnat"
		case h.AddressClass != "public":
			h.Role = "private"
		case m.Mobile != nil && h.TTL == m.Mobile.GatewayTTL:
			h.Role = "mno_gateway"
		case cloudASNs[h.ASN] != "":
			h.Role = "cloud_edge"
		default:
			h.Role = "router"
		}
	}
}

func (e *Enricher) buildLinks(m *InfraMap) {
	var nodes []node
	if m.Device != nil && m.Device.Location != nil {
		nodes = append(nodes, node{hopIdx: -1, ttl: 0, label: "device", p: gis.LatLon{Lat: m.Device.Location.Lat, Lon: m.Device.Location.Lon},
			acc: m.Device.Location.AccuracyKM, iso2: e.countryAt(m.Device.Location.Lat, m.Device.Location.Lon, m.Device.Country)})
	}
	for i, h := range m.Hops {
		if h.LocationStatus != "accepted" {
			continue
		}
		nodes = append(nodes, node{hopIdx: i, ttl: h.TTL, ip: h.IP, label: nonEmpty(h.Geo.City, h.Geo.CountryISO2, h.IP),
			p: gis.LatLon{Lat: h.Geo.Lat, Lon: h.Geo.Lon}, acc: h.Geo.AccuracyKM, iso2: h.Geo.CountryISO2, rttMS: h.RTT.Min, asn: h.ASN, org: h.ASOrg})
	}
	for k := 1; k < len(nodes); k++ {
		a, b := nodes[k-1], nodes[k]
		l := LinkEnriched{
			Index: len(m.Links),
			From:  Endpoint{TTL: a.ttl, IP: a.ip, Label: a.label, Lat: a.p.Lat, Lon: a.p.Lon, ISO2: a.iso2},
			To:    Endpoint{TTL: b.ttl, IP: b.ip, Label: b.label, Lat: b.p.Lat, Lon: b.p.Lon, ISO2: b.iso2},
		}
		for _, h := range m.Hops {
			if h.TTL > a.ttl && h.TTL < b.ttl {
				l.ViaUnlocatedTTLs = append(l.ViaUnlocatedTTLs, h.TTL)
			}
		}
		l.DistanceKM = round1(gis.HaversineKM(a.p, b.p))
		l.DeltaRTTms = round3(math.Max(0, b.rttMS-a.rttMS))
		l.MinFeasibleRTTms = round3(gis.MinRTTms(l.DistanceKM))
		if b.asn != 0 {
			l.Operator = &Operator{ASN: b.asn, Name: b.org}
		}
		crossing := e.Land.AnalyzeCrossing(a.p, b.p, e.CrossingOpts)
		l.Ocean = &crossing

		switch {
		case m.Mobile != nil && b.ttl <= m.Mobile.GatewayTTL:
			l.Medium = "cellular_backhaul"
			l.Confidence = m.Mobile.Confidence
			l.Cellular = &CellularLink{Operator: m.Mobile.Operator.Name, ASN: m.Mobile.Operator.ASN}
			if m.Mobile.GatewayHub != nil {
				l.Cellular.HubID = m.Mobile.GatewayHub.ID
			}
			m.Mobile.LinkIndexes = append(m.Mobile.LinkIndexes, l.Index)
		case crossing.CrossesOcean:
			l.Medium = "subsea_cable"
			l.Confidence = 0.9
			l.Subsea = e.matchCable(a, b, l.DeltaRTTms, crossing)
		case l.DistanceKM < e.metroKM():
			l.Medium, l.Confidence = "metro_fiber", 0.75
		default:
			l.Medium, l.Confidence = "terrestrial_fiber", 0.7
		}
		m.Links = append(m.Links, l)
	}
}

func (e *Enricher) metroKM() float64 {
	if e.MetroKM > 0 {
		return e.MetroKM
	}
	return 50
}

func (e *Enricher) countryAt(lat, lon float64, fallback string) string {
	if cc, ok := e.Land.CountryAt(gis.LatLon{Lat: lat, Lon: lon}); ok && cc != "" {
		return cc
	}
	return fallback
}

func (e *Enricher) matchCable(a, b node, deltaRTT float64, c gis.Crossing) *SubseaResult {
	res := &SubseaResult{Alternatives: []subsea.Candidate{}}
	if e.Cables == nil {
		res.Note = "no cable dataset configured"
		return res
	}
	cands := e.Cables.Match(subsea.MatchInput{
		A: a.p, B: b.p, AccuracyKM: math.Max(a.acc, b.acc), DeltaRTTms: deltaRTT,
		OperatorKeywords: orgKeywords(a.org, b.org), WaterSamples: c.WaterSamples,
	})
	if len(cands) == 0 {
		res.Note = "ocean crossing with no known cable near both ends"
		return res
	}
	sel := cands[0]
	res.Selected = &sel
	for _, alt := range cands[1:] {
		alt.Route = nil // keep the document compact: geometry only for the selected cable
		res.Alternatives = append(res.Alternatives, alt)
	}
	return res
}

func (e *Enricher) buildMapping(m *InfraMap) {
	seq := 0
	add := func(n NetworkRef, inf Infrastructure) {
		seq++
		m.Mapping = append(m.Mapping, MappingEntry{Seq: seq, Network: n, Infrastructure: inf})
	}
	if m.Device != nil {
		inf := Infrastructure{Kind: "device", Name: "measuring device (" + m.Device.AccessType + ")", Confidence: 1}
		if m.Device.Cell != nil {
			inf.Kind, inf.Name = "cell_tower", "serving cell "+m.Device.Cell.CellKey.String()
		}
		if m.Device.Location != nil {
			inf.Location = fmt.Sprintf("%.4f, %.4f", m.Device.Location.Lat, m.Device.Location.Lon)
		}
		add(NetworkRef{Kind: "device"}, inf)
	}
	linkTo := map[int]LinkEnriched{}
	for _, l := range m.Links {
		linkTo[l.To.TTL] = l
	}
	for _, h := range m.Hops {
		if l, ok := linkTo[h.TTL]; ok {
			from := l.From.TTL
			add(NetworkRef{Kind: "link", FromTTL: &from, ToTTL: l.To.TTL}, linkInfra(l, m.Mobile))
		}
		if !h.Responded {
			continue
		}
		inf := Infrastructure{Kind: h.Role, Name: nonEmpty(h.Hostname, h.IP), Operator: h.ASOrg, Confidence: 0.5}
		if h.Geo != nil && h.LocationStatus == "accepted" {
			inf.Location = strings.Trim(strings.Join([]string{h.Geo.City, h.Geo.CountryISO2}, ", "), ", ")
			inf.Confidence = 0.7
			if h.Geo.Sources["location"] == "rdns_hint" {
				inf.Confidence = 0.85
			}
		}
		switch h.Role {
		case "mno_gateway":
			inf.Name = "MNO packet gateway (" + m.Mobile.Operator.Name + ")"
			if hub := m.Mobile.GatewayHub; hub != nil {
				inf.HubID, inf.Location = hub.ID, hub.City+", "+hub.Country
				inf.Confidence = hub.Confidence
			}
		case "cgnat":
			inf.Name, inf.Confidence = "carrier-grade NAT", 0.8
		case "private":
			inf.Name, inf.Confidence = "private network router", 0.8
		}
		add(NetworkRef{Kind: "hop", TTL: h.TTL, IP: h.IP, ASN: h.ASN}, inf)
	}
}

// linkCountries: overland links traverse the countries under the great
// circle; subsea links touch only their ends and the landing stations on the
// matched cable stretch.
func linkCountries(l LinkEnriched) []string {
	var out []string
	add := func(c string) {
		if c != "" && (len(out) == 0 || out[len(out)-1] != c) {
			out = append(out, c)
		}
	}
	if l.Medium == "subsea_cable" {
		add(l.From.ISO2)
		if l.Subsea != nil && l.Subsea.Selected != nil {
			for _, ld := range l.Subsea.Selected.RouteLandings {
				add(ld.Country)
			}
		}
		add(l.To.ISO2)
		return out
	}
	if l.Ocean != nil {
		for _, c := range l.Ocean.Countries {
			add(c)
		}
	}
	if len(out) == 0 {
		add(l.From.ISO2)
		add(l.To.ISO2)
	}
	return out
}

func linkInfra(l LinkEnriched, mob *MobileLeg) Infrastructure {
	inf := Infrastructure{Kind: l.Medium, Confidence: l.Confidence, Countries: linkCountries(l)}
	if l.Operator != nil {
		inf.Operator = l.Operator.Name
	}
	switch l.Medium {
	case "cellular_backhaul":
		inf.Name = "cellular backhaul (" + mob.Operator.Name + ")"
		inf.Operator = mob.Operator.Name
		if mob.GatewayHub != nil {
			inf.HubID = mob.GatewayHub.ID
			inf.Location = mob.GatewayHub.City + ", " + mob.GatewayHub.Country
		}
	case "subsea_cable":
		inf.Name = "unmatched ocean crossing"
		if l.Subsea != nil && l.Subsea.Selected != nil {
			s := l.Subsea.Selected
			inf.Name, inf.CableID, inf.Probability = s.Name, s.CableID, s.Probability
			if s.LandingA != nil {
				inf.LandingA = s.LandingA.Name
			}
			if s.LandingB != nil {
				inf.LandingB = s.LandingB.Name
			}
		}
	case "metro_fiber":
		inf.Name = "metro fiber, " + l.From.Label
	default:
		inf.Name = "terrestrial fiber, " + l.From.Label + " → " + l.To.Label
	}
	return inf
}

func (e *Enricher) summarize(m *InfraMap) {
	s := Summary{Media: []string{}, HopCountries: []string{}, CountriesTraversed: []string{}, SubseaCables: []string{}, Cellular: m.Mobile != nil}
	seen := map[string]map[string]bool{"m": {}, "h": {}, "o": {}, "c": {}}
	add := func(set string, dst *[]string, v string) {
		if v != "" && !seen[set][v] {
			seen[set][v] = true
			*dst = append(*dst, v)
		}
	}
	for _, h := range m.Hops {
		switch h.LocationStatus {
		case "accepted":
			s.LocatedHops++
			add("h", &s.HopCountries, h.Geo.CountryISO2)
		case "rejected_light_speed", "rejected_detour":
			s.RejectedLocations++
		}
	}
	for _, l := range m.Links {
		add("m", &s.Media, l.Medium)
		s.PathKM += l.DistanceKM
		for _, c := range linkCountries(l) {
			add("o", &s.CountriesTraversed, c)
		}
		if l.Medium == "subsea_cable" {
			s.OceanCrossings++
			if l.Subsea != nil && l.Subsea.Selected != nil {
				add("c", &s.SubseaCables, l.Subsea.Selected.Name)
			}
		}
	}
	s.PathKM = round1(s.PathKM)
	m.Summary = s
}

// --- helpers ---

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

func addressClass(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return "loopback"
	case a.IsLinkLocalUnicast():
		return "link_local"
	case a.IsPrivate():
		return "private"
	case a.Is4() && cgnatPrefix.Contains(a):
		return "cgnat"
	case geoip.IsBogon(a):
		return "reserved"
	}
	return "public"
}

var genericOrg = map[string]bool{"the": true, "inc": true, "llc": true, "ltd": true, "limited": true, "corp": true, "corporation": true,
	"communications": true, "telecom": true, "network": true, "networks": true, "global": true, "international": true,
	"group": true, "company": true, "america": true, "services": true}

// orgKeywords: first distinctive word of each AS organisation ("TATA
// COMMUNICATIONS (AMERICA) INC" -> "tata").
func orgKeywords(orgs ...string) []string {
	var out []string
	for _, o := range orgs {
		if i := strings.Index(o, " - "); i >= 0 {
			o = o[i+3:]
		}
		for _, w := range strings.FieldsFunc(strings.ToLower(o), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) {
			if len(w) >= 3 && !genericOrg[w] && !strings.HasPrefix(w, "as") {
				out = append(out, w)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func nonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
