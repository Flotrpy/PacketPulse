package compliance

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// RoutePoint is one located point on a packet's path, in order.
type RoutePoint struct {
	Seq      int     `json:"seq"`
	TTL      int     `json:"ttl,omitempty"`
	Kind     string  `json:"kind"` // source | hop | subsea_landing | destination
	IP       string  `json:"ip,omitempty"`
	ASN      int     `json:"asn,omitempty"`
	Operator string  `json:"operator,omitempty"`
	Label    string  `json:"label,omitempty"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	// CountryHint is used only when the coordinate does not fall on land (e.g. GeoIP country).
	CountryHint string `json:"country_hint,omitempty"`
	// Timestamp is when the packet reached this point.
	Timestamp time.Time `json:"timestamp"`
	// TimestampSource explains how Timestamp was obtained (e.g. "probe_sent_plus_half_rtt").
	TimestampSource    string  `json:"timestamp_source,omitempty"`
	LocationConfidence float64 `json:"location_confidence"`
	LocationSource     string  `json:"location_source,omitempty"`
}

// Packet is the engine's input: a classified payload and its transit route.
type Packet struct {
	PacketID        string       `json:"packet_id"`
	Timestamp       time.Time    `json:"timestamp"`
	Classifications []string     `json:"classifications"`
	Route           []RoutePoint `json:"route"`
	Source          string       `json:"source,omitempty"` // producer, e.g. "infraenrich"
}

// Territory is where a violation happened.
type Territory struct {
	CountryISO2     string   `json:"country_iso2"`
	CountryName     string   `json:"country_name,omitempty"`
	Subdivision     string   `json:"subdivision,omitempty"`
	SubdivisionName string   `json:"subdivision_name,omitempty"`
	Regions         []string `json:"regions"` // registry regions containing the point
	Lat             float64  `json:"lat"`
	Lon             float64  `json:"lon"`
	LocatedBy       string   `json:"located_by"` // polygon | nearest_coast | hint
}

// PointRef identifies the route point.
type PointRef struct {
	Seq                int     `json:"seq"`
	TTL                int     `json:"ttl,omitempty"`
	Kind               string  `json:"kind"`
	IP                 string  `json:"ip,omitempty"`
	ASN                int     `json:"asn,omitempty"`
	Operator           string  `json:"operator,omitempty"`
	Label              string  `json:"label,omitempty"`
	LocationConfidence float64 `json:"location_confidence"`
	LocationSource     string  `json:"location_source,omitempty"`
}

// CompliantPoint is the last point inside the permitted territory before entry.
type CompliantPoint struct {
	Seq         int       `json:"seq"`
	CountryISO2 string    `json:"country_iso2"`
	Label       string    `json:"label,omitempty"`
	Lat         float64   `json:"lat"`
	Lon         float64   `json:"lon"`
	At          time.Time `json:"at"`
}

// Recommendation is the concrete mitigation.
type Recommendation struct {
	Action         string   `json:"action"`
	Recommendation string   `json:"recommendation"`
	Proxy          *Proxy   `json:"proxy,omitempty"`
	ProxyDistance  float64  `json:"proxy_distance_km,omitempty"`
	Alternatives   []string `json:"alternatives,omitempty"`
}

// ComplianceViolationAlert is raised when a governed payload enters a
// territory its profile does not permit.
type ComplianceViolationAlert struct {
	Type            string          `json:"type"` // always "ComplianceViolationAlert"
	AlertID         string          `json:"alert_id"`
	PacketID        string          `json:"packet_id"`
	ProfileID       string          `json:"profile_id"`
	ProfileVersion  int             `json:"profile_version"`
	Severity        string          `json:"severity"`
	Suspected       bool            `json:"suspected"` // location confidence below the profile minimum
	Status          string          `json:"status"`    // open
	Classification  string          `json:"classification"`
	Territory       Territory       `json:"territory"`
	EnteredAt       time.Time       `json:"entered_at"`
	EntryTimeSource string          `json:"entered_at_source"`
	Point           PointRef        `json:"point"`
	LastCompliant   *CompliantPoint `json:"last_compliant_point"`
	Law             Law             `json:"law"`
	Rule            string          `json:"rule"`
	Exception       string          `json:"exception,omitempty"` // downgraded by this exception
	Mitigation      Recommendation  `json:"mitigation"`
	GeneratedAt     time.Time       `json:"generated_at"`
}

// ProfileResult summarises one profile's evaluation.
type ProfileResult struct {
	ProfileID string `json:"profile_id"`
	Version   int    `json:"version"`
	Matched   string `json:"matched_classification"`
	Result    string `json:"result"` // compliant | violation | suspected_violation
	Alerts    int    `json:"alerts"`
}

// ResolvedPoint is a route point after territory lookup.
type ResolvedPoint struct {
	Seq      int      `json:"seq"`
	TTL      int      `json:"ttl,omitempty"`
	Kind     string   `json:"kind"`
	Label    string   `json:"label,omitempty"`
	Location Location `json:"location"`
	Regions  []string `json:"regions"`
	// Status is the worst verdict any matching profile gave this point:
	// not_evaluated < compliant < suspected < violation. Every point inside a
	// non-permitted territory is marked, not only the entry point that
	// raised the alert, so clients can colour whole stretches of a route.
	Status    string   `json:"status"`
	FlaggedBy []string `json:"flagged_by,omitempty"`
}

var statusRank = map[string]int{"not_evaluated": 0, "compliant": 1, "suspected": 2, "violation": 3}

func (r *ResolvedPoint) mark(status, profileID string) {
	if statusRank[status] > statusRank[r.Status] {
		r.Status = status
	}
	if status == "suspected" || status == "violation" {
		if !contains(r.FlaggedBy, profileID) {
			r.FlaggedBy = append(r.FlaggedBy, profileID)
		}
	}
}

// Evaluation is the engine's verdict for one packet.
type Evaluation struct {
	PacketID        string                     `json:"packet_id"`
	EvaluatedAt     time.Time                  `json:"evaluated_at"`
	RegistrySeq     uint64                     `json:"registry_snapshot"`
	Verdict         string                     `json:"verdict"` // compliant | violation | suspected_violation | not_applicable
	Classifications []string                   `json:"classifications"`
	Profiles        []ProfileResult            `json:"profiles"`
	Alerts          []ComplianceViolationAlert `json:"alerts"`
	Route           []ResolvedPoint            `json:"route"`
	DurationMS      float64                    `json:"duration_ms"`
}

// AlertSink receives alerts as soon as they are generated.
type AlertSink interface {
	Publish(ComplianceViolationAlert)
}

// Engine evaluates packets against the live registry.
type Engine struct {
	Registry *Registry
	Locator  *Locator
	Sink     AlertSink
	Now      func() time.Time
}

var ErrBadPacket = errors.New("invalid packet")

// Evaluate checks a packet against every enabled profile governing one of
// its classifications. It uses a single registry snapshot, so a concurrent
// profile update applies cleanly to the next packet.
func (e *Engine) Evaluate(ctx context.Context, p Packet) (*Evaluation, error) {
	return e.evaluate(ctx, p, true)
}

// Preview evaluates without publishing alerts to the sink. Visualisation
// clients use it to colour routes without flooding the alert stream every
// time they re-check the same traffic.
func (e *Engine) Preview(ctx context.Context, p Packet) (*Evaluation, error) {
	return e.evaluate(ctx, p, false)
}

func (e *Engine) evaluate(ctx context.Context, p Packet, publish bool) (*Evaluation, error) {
	if err := validatePacket(p); err != nil {
		return nil, err
	}
	now := time.Now().UTC
	if e.Now != nil {
		now = e.Now
	}
	t0 := time.Now()
	snap := e.Registry.Snapshot()
	classes := make([]string, len(p.Classifications))
	for i, c := range p.Classifications {
		classes[i] = normalizeClass(c)
	}
	route := append([]RoutePoint(nil), p.Route...)
	sort.SliceStable(route, func(i, j int) bool { return route[i].Seq < route[j].Seq })

	ev := &Evaluation{PacketID: p.PacketID, EvaluatedAt: now(), RegistrySeq: snap.Seq, Verdict: "not_applicable",
		Classifications: classes, Profiles: []ProfileResult{}, Alerts: []ComplianceViolationAlert{}}

	resolved := make([]ResolvedPoint, len(route))
	for i, rp := range route {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		loc := e.Locator.Locate(LatLon{Lat: rp.Lat, Lon: rp.Lon}, rp.CountryHint)
		resolved[i] = ResolvedPoint{Seq: rp.Seq, TTL: rp.TTL, Kind: rp.Kind, Label: rp.Label, Location: loc,
			Regions: snap.regionsAt(loc, LatLon{Lat: rp.Lat, Lon: rp.Lon}), Status: "not_evaluated"}
	}
	ev.Route = resolved

	ids := make([]string, 0, len(snap.Profiles))
	for id := range snap.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		prof := snap.Profiles[id]
		if !prof.Enabled {
			continue
		}
		matched := firstShared(prof.Classifications, classes)
		if matched == "" {
			continue
		}
		res := ProfileResult{ProfileID: prof.ID, Version: prof.Version, Matched: matched, Result: "compliant"}
		alerts := e.evaluateProfile(snap, prof, matched, p, route, resolved, now())
		for _, a := range alerts {
			// Uncertain location, or allowed only pending review (e.g. DPF
			// listing for US recipients): not a confirmed violation.
			if a.Suspected || a.Exception != "" {
				if res.Result == "compliant" {
					res.Result = "suspected_violation"
				}
			} else {
				res.Result = "violation"
			}
		}
		res.Alerts = len(alerts)
		ev.Profiles = append(ev.Profiles, res)
		ev.Alerts = append(ev.Alerts, alerts...)
	}

	if len(ev.Profiles) > 0 {
		ev.Verdict = "compliant"
		for _, r := range ev.Profiles {
			if r.Result == "violation" {
				ev.Verdict = "violation"
				break
			}
			if r.Result == "suspected_violation" {
				ev.Verdict = "suspected_violation"
			}
		}
	}
	if publish && e.Sink != nil {
		for _, a := range ev.Alerts {
			e.Sink.Publish(a)
		}
	}
	ev.DurationMS = math.Round(float64(time.Since(t0).Microseconds())) / 1000
	return ev, nil
}

// evaluateProfile walks the route and raises one alert per entry into a
// non-permitted territory (re-entering the same territory after leaving it
// raises a new alert; staying in it does not).
func (e *Engine) evaluateProfile(snap *Snapshot, prof Profile, class string, p Packet, route []RoutePoint, res []ResolvedPoint, now time.Time) []ComplianceViolationAlert {
	var alerts []ComplianceViolationAlert
	var lastOK *CompliantPoint
	lastBadTerritory := ""
	// State/province detail matters only for profiles drawn on subdivisions
	// (e.g. CCPA's California line). For country-level rules it would only
	// add noise and re-alert at every internal state border.
	useSub := snap.usesSubdivisions(prof)
	start := 0
	if prof.Scope == "destination_only" {
		start = len(route) - 1
		for i := 0; i < start; i++ {
			res[i].mark("compliant", prof.ID) // transit is allowed under storage rules
		}
		// The last compliant point is still useful for the recommendation.
		for i := len(route) - 2; i >= 0; i-- {
			if ok, _ := snap.permitted(prof, res[i]); ok && res[i].Location.CountryISO2 != "" {
				lastOK = compliantPoint(route[i], res[i])
				break
			}
		}
	}
	for i := start; i < len(route); i++ {
		rp, rs := route[i], res[i]
		if rs.Location.CountryISO2 == "" {
			res[i].mark("compliant", prof.ID) // over water or unlocatable: no territory entered
			continue
		}
		ok, downgradedBy := snap.permitted(prof, rs)
		if ok && downgradedBy == "" {
			res[i].mark("compliant", prof.ID)
			lastOK = compliantPoint(rp, rs)
			lastBadTerritory = ""
			continue
		}
		if rp.LocationConfidence < prof.MinLocationConfidence || downgradedBy != "" {
			res[i].mark("suspected", prof.ID)
		} else {
			res[i].mark("violation", prof.ID)
		}
		territoryKey := rs.Location.CountryISO2
		if useSub {
			territoryKey += "/" + rs.Location.Subdivision
		}
		if territoryKey == lastBadTerritory {
			continue
		}
		lastBadTerritory = territoryKey

		severity := prof.Severity
		suspected := rp.LocationConfidence < prof.MinLocationConfidence
		if suspected {
			severity = lowerSeverity(severity)
		}
		if downgradedBy != "" {
			severity = lowerSeverity(severity)
		}
		at, src := rp.Timestamp, rp.TimestampSource
		if at.IsZero() {
			at, src = p.Timestamp, "packet_timestamp"
		} else if src == "" {
			src = "route_point"
		}
		terr := Territory{
			CountryISO2: rs.Location.CountryISO2, CountryName: rs.Location.CountryName,
			Regions: rs.Regions, Lat: rp.Lat, Lon: rp.Lon, LocatedBy: rs.Location.Source,
		}
		if useSub {
			terr.Subdivision, terr.SubdivisionName = rs.Location.Subdivision, rs.Location.SubdivisionName
		}
		a := ComplianceViolationAlert{
			Type: "ComplianceViolationAlert", AlertID: newAlertID(), PacketID: p.PacketID,
			ProfileID: prof.ID, ProfileVersion: prof.Version, Severity: severity, Suspected: suspected, Status: "open",
			Classification: class, Territory: terr, EnteredAt: at.UTC(), EntryTimeSource: src,
			Point: PointRef{Seq: rp.Seq, TTL: rp.TTL, Kind: rp.Kind, IP: rp.IP, ASN: rp.ASN, Operator: rp.Operator, Label: rp.Label,
				LocationConfidence: rp.LocationConfidence, LocationSource: rp.LocationSource},
			LastCompliant: lastOK, Law: prof.Law, Rule: prof.Rule, Exception: downgradedBy, GeneratedAt: now,
		}
		a.Mitigation = e.recommend(snap, prof, terr, lastOK, route)
		alerts = append(alerts, a)
	}
	return alerts
}

func compliantPoint(rp RoutePoint, rs ResolvedPoint) *CompliantPoint {
	return &CompliantPoint{Seq: rp.Seq, CountryISO2: rs.Location.CountryISO2, Label: rp.Label, Lat: rp.Lat, Lon: rp.Lon, At: rp.Timestamp.UTC()}
}

func (s *Snapshot) usesSubdivisions(prof Profile) bool {
	ids := append([]string{}, prof.Regions...)
	for _, e := range prof.Exceptions {
		ids = append(ids, e.Region)
	}
	for _, id := range ids {
		if len(s.Regions[id].Subdivisions) > 0 {
			return true
		}
	}
	return false
}

// permitted reports whether a resolved point is allowed under prof, and the
// exception reason when it is allowed only in downgraded form.
func (s *Snapshot) permitted(prof Profile, rs ResolvedPoint) (bool, string) {
	in := func(ids []string) bool {
		for _, id := range ids {
			if contains(rs.Regions, id) {
				return true
			}
		}
		return false
	}
	if prof.Mode == "deny_list" {
		return !in(prof.Regions), ""
	}
	if in(prof.Regions) {
		return true, ""
	}
	for _, ex := range prof.Exceptions {
		if contains(rs.Regions, ex.Region) {
			if ex.Effect == "allow" {
				return true, ""
			}
			return false, ex.Reason
		}
	}
	return false, ""
}

// RegionsAt returns every registry region containing the location.
func (s *Snapshot) RegionsAt(loc Location, p LatLon) []string { return s.regionsAt(loc, p) }

func (s *Snapshot) regionsAt(loc Location, p LatLon) []string {
	out := []string{}
	custom := s.custom.all(p)
	for id, r := range s.Regions {
		if (loc.CountryISO2 != "" && contains(r.Countries, loc.CountryISO2)) ||
			(loc.Subdivision != "" && contains(r.Subdivisions, loc.Subdivision)) ||
			contains(custom, id) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// recommend picks the nearest proxy inside the profile's permitted territory
// (allow_list) or outside the prohibited one (deny_list), measured from the
// last compliant point (else the route's first point).
func (e *Engine) recommend(snap *Snapshot, prof Profile, terr Territory, lastOK *CompliantPoint, route []RoutePoint) Recommendation {
	rec := Recommendation{Action: prof.Mitigation.Action, Alternatives: prof.Mitigation.Alternatives}
	anchor := LatLon{Lat: route[0].Lat, Lon: route[0].Lon}
	if lastOK != nil {
		anchor = LatLon{Lat: lastOK.Lat, Lon: lastOK.Lon}
	}
	ids := make([]string, 0, len(snap.Proxies))
	for id := range snap.Proxies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	best, bestKM := (*Proxy)(nil), math.Inf(1)
	for _, id := range ids {
		px := snap.Proxies[id]
		loc := e.Locator.Locate(LatLon{Lat: px.Lat, Lon: px.Lon}, "")
		ok, downgraded := snap.permitted(prof, ResolvedPoint{Location: loc, Regions: snap.regionsAt(loc, LatLon{Lat: px.Lat, Lon: px.Lon})})
		if !ok || downgraded != "" {
			continue
		}
		if d := haversineKM(anchor, LatLon{Lat: px.Lat, Lon: px.Lon}); d < bestKM {
			p := px
			best, bestKM = &p, d
		}
	}
	territory := terr.CountryName
	if territory == "" {
		territory = terr.CountryISO2
	}
	if terr.SubdivisionName != "" {
		territory = terr.SubdivisionName + ", " + territory
	}
	repl := []string{"{{territory}}", territory, "{{country}}", terr.CountryISO2, "{{law}}", prof.Law.Code}
	if best != nil {
		rec.Proxy, rec.ProxyDistance = best, math.Round(bestKM*10)/10
		repl = append(repl, "{{proxy}}", best.Name, "{{proxy_region}}", best.Region, "{{proxy_city}}", best.City)
	} else {
		repl = append(repl, "{{proxy}}", "an in-territory relay", "{{proxy_region}}", "no registered proxy", "{{proxy_city}}", "n/a")
	}
	rec.Recommendation = strings.NewReplacer(repl...).Replace(prof.Mitigation.Template)
	return rec
}

func validatePacket(p Packet) error {
	switch {
	case p.PacketID == "":
		return fmt.Errorf("%w: packet_id is required", ErrBadPacket)
	case len(p.Classifications) == 0:
		return fmt.Errorf("%w: at least one classification is required", ErrBadPacket)
	case len(p.Route) == 0:
		return fmt.Errorf("%w: route is empty", ErrBadPacket)
	}
	for _, rp := range p.Route {
		if rp.Lat < -90 || rp.Lat > 90 || rp.Lon < -180 || rp.Lon > 180 || math.IsNaN(rp.Lat) || math.IsNaN(rp.Lon) {
			return fmt.Errorf("%w: route point %d has invalid coordinates", ErrBadPacket, rp.Seq)
		}
		if rp.LocationConfidence < 0 || rp.LocationConfidence > 1 {
			return fmt.Errorf("%w: route point %d confidence out of [0,1]", ErrBadPacket, rp.Seq)
		}
	}
	return nil
}

func firstShared(a, b []string) string {
	for _, x := range a {
		if contains(b, x) {
			return x
		}
	}
	return ""
}

func lowerSeverity(s string) string {
	for i, v := range severities {
		if v == s && i > 0 {
			return severities[i-1]
		}
	}
	return s
}

func newAlertID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
