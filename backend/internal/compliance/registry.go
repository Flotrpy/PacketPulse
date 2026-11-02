// Package compliance is an independent rule engine that evaluates routed
// packets (ordered, located route points) against data-sovereignty
// profiles and raises ComplianceViolationAlerts. It depends on nothing else
// in PacketPulse: adapters (package compliance/adapt) translate enriched
// routes into Packets, and package compliance/api exposes it over HTTP.
package compliance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Region is a regulatory boundary: a union of countries, first-level
// subdivisions (ISO 3166-2) and/or a custom polygon.
type Region struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	Countries    []string  `json:"countries,omitempty"`    // ISO 3166-1 alpha-2
	Subdivisions []string  `json:"subdivisions,omitempty"` // ISO 3166-2, e.g. "US-CA"
	Polygon      *Geometry `json:"polygon,omitempty"`      // GeoJSON Polygon / MultiPolygon
	Source       string    `json:"source,omitempty"`       // legal / membership reference
	Version      int       `json:"version"`
	UpdatedAt    time.Time `json:"updated_at"`
	UpdatedBy    string    `json:"updated_by,omitempty"`
}

// Law identifies the legal provision a profile enforces.
type Law struct {
	Code     string   `json:"code"`     // e.g. GDPR
	Title    string   `json:"title"`    // e.g. General Data Protection Regulation
	Citation string   `json:"citation"` // e.g. Regulation (EU) 2016/679, Chapter V (Art. 44-49)
	Articles []string `json:"articles,omitempty"`
	URL      string   `json:"url,omitempty"`
}

// Mitigation tells the engine how to phrase the recommendation.
// Template placeholders: {{territory}} {{country}} {{proxy}} {{proxy_region}} {{proxy_city}} {{law}}.
type Mitigation struct {
	Action       string   `json:"action"` // reroute | block | review | contract
	Template     string   `json:"template"`
	Alternatives []string `json:"alternatives,omitempty"`
}

// Exception allows (or downgrades) specific territories inside an
// allow-list profile, e.g. EU adequacy decisions.
type Exception struct {
	Region string `json:"region"`
	Effect string `json:"effect"` // allow | downgrade
	Reason string `json:"reason"`
}

// Profile is one compliance rule set.
type Profile struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Description     string      `json:"description,omitempty"`
	Enabled         bool        `json:"enabled"`
	Classifications []string    `json:"classifications"` // data classes this profile governs
	Mode            string      `json:"mode"`            // allow_list | deny_list
	Regions         []string    `json:"regions"`         // allowed (allow_list) or prohibited (deny_list) region ids
	Exceptions      []Exception `json:"exceptions,omitempty"`
	// Scope: all_hops = any route point outside counts (transit included);
	// destination_only = only where the data terminates (storage rules).
	Scope    string `json:"scope"`
	Severity string `json:"severity"` // critical | high | medium | low | info
	// Points located with less confidence raise "suspected" alerts one
	// severity level lower.
	MinLocationConfidence float64    `json:"min_location_confidence"`
	Rule                  string     `json:"rule"` // human-readable statement of the obligation
	Law                   Law        `json:"law"`
	Mitigation            Mitigation `json:"mitigation"`
	Version               int        `json:"version"`
	UpdatedAt             time.Time  `json:"updated_at"`
	UpdatedBy             string     `json:"updated_by,omitempty"`
}

// Proxy is a compliant egress/relay point the engine can recommend.
type Proxy struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Provider string  `json:"provider"`
	Region   string  `json:"cloud_region"`
	City     string  `json:"city"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	// Isolation notes, e.g. "EU Sovereign Cloud" or "EEA-only peering".
	Notes string `json:"notes,omitempty"`
}

// AuditEntry records a registry change.
type AuditEntry struct {
	At       time.Time `json:"at"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"` // put | delete
	Kind     string    `json:"kind"`   // region | profile | proxy
	ID       string    `json:"id"`
	Version  int       `json:"version"`
	Snapshot uint64    `json:"snapshot"`
}

// Snapshot is an immutable view of the registry; evaluations take one and
// never see a half-applied update.
type Snapshot struct {
	Seq      uint64
	Regions  map[string]Region
	Profiles map[string]Profile
	Proxies  map[string]Proxy
	custom   *polygonIndex // custom region polygons, tagged by region id
}

var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("version conflict")
	ErrInvalid       = errors.New("invalid definition")
	ErrInUse         = errors.New("still referenced")
	idPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)
	iso2Pattern      = regexp.MustCompile(`^[A-Z]{2}$`)
	iso3166_2Pattern = regexp.MustCompile(`^[A-Z]{2}-[A-Z0-9]{1,3}$`)
)

var severities = []string{"info", "low", "medium", "high", "critical"}

// Registry holds the live definitions. Reads are lock-free (atomic snapshot
// pointer); writes are serialised, validated, persisted, then swapped in.
type Registry struct {
	mu    sync.Mutex
	snap  atomic.Pointer[Snapshot]
	path  string // persistence file ("" = memory only)
	audit []AuditEntry
	now   func() time.Time
}

type persisted struct {
	Regions  []Region     `json:"regions"`
	Profiles []Profile    `json:"profiles"`
	Proxies  []Proxy      `json:"proxies"`
	Audit    []AuditEntry `json:"audit"`
}

// NewRegistry loads definitions from path when it exists, otherwise starts
// from the built-in defaults (and writes them to path).
func NewRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, now: func() time.Time { return time.Now().UTC() }}
	var p persisted
	loaded := false
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, fmt.Errorf("compliance store %s: %w", path, err)
			}
			loaded = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if !loaded {
		p = persisted{Regions: DefaultRegions(), Profiles: DefaultProfiles(), Proxies: DefaultProxies()}
		now := r.now()
		for i := range p.Regions {
			p.Regions[i].Version, p.Regions[i].UpdatedAt, p.Regions[i].UpdatedBy = 1, now, "system"
		}
		for i := range p.Profiles {
			p.Profiles[i].Version, p.Profiles[i].UpdatedAt, p.Profiles[i].UpdatedBy = 1, now, "system"
		}
	}
	s := &Snapshot{Seq: 1, Regions: map[string]Region{}, Profiles: map[string]Profile{}, Proxies: map[string]Proxy{}}
	for _, x := range p.Regions {
		s.Regions[x.ID] = x
	}
	for _, x := range p.Profiles {
		s.Profiles[x.ID] = x
	}
	for _, x := range p.Proxies {
		s.Proxies[x.ID] = x
	}
	for _, rg := range s.Regions {
		if err := validateRegion(rg); err != nil {
			return nil, err
		}
	}
	for _, pf := range s.Profiles {
		if err := validateProfile(pf, s); err != nil {
			return nil, err
		}
	}
	if err := s.compile(); err != nil {
		return nil, err
	}
	r.audit = p.Audit
	r.snap.Store(s)
	if !loaded && path != "" {
		if err := r.persist(s); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Snapshot returns the current immutable view.
func (r *Registry) Snapshot() *Snapshot { return r.snap.Load() }

func (s *Snapshot) compile() error {
	s.custom = newPolygonIndex()
	for _, rg := range s.Regions {
		if rg.Polygon != nil {
			if err := s.custom.addGeometry(rg.ID, rg.Name, *rg.Polygon); err != nil {
				return fmt.Errorf("%w: region %s: %v", ErrInvalid, rg.ID, err)
			}
		}
	}
	return nil
}

func (s *Snapshot) clone() *Snapshot {
	n := &Snapshot{Seq: s.Seq + 1, Regions: make(map[string]Region, len(s.Regions)), Profiles: make(map[string]Profile, len(s.Profiles)), Proxies: make(map[string]Proxy, len(s.Proxies))}
	for k, v := range s.Regions {
		n.Regions[k] = v
	}
	for k, v := range s.Profiles {
		n.Profiles[k] = v
	}
	for k, v := range s.Proxies {
		n.Proxies[k] = v
	}
	return n
}

// ETag returns the entity tag for a version.
func ETag(id string, version int) string { return fmt.Sprintf(`"%s-v%d"`, id, version) }

// mutate applies f to a copy of the snapshot, validates everything, persists, swaps.
func (r *Registry) mutate(actor, action, kind, id string, f func(*Snapshot) (int, error)) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.Snapshot().clone()
	version, err := f(next)
	if err != nil {
		return 0, err
	}
	for _, pf := range next.Profiles {
		if err := validateProfile(pf, next); err != nil {
			return 0, err
		}
	}
	if err := next.compile(); err != nil {
		return 0, err
	}
	entry := AuditEntry{At: r.now(), Actor: actor, Action: action, Kind: kind, ID: id, Version: version, Snapshot: next.Seq}
	r.audit = append(r.audit, entry)
	if len(r.audit) > 5000 {
		r.audit = r.audit[len(r.audit)-5000:]
	}
	if err := r.persist(next); err != nil {
		r.audit = r.audit[:len(r.audit)-1]
		return 0, err
	}
	r.snap.Store(next)
	return version, nil
}

func checkMatch(ifMatch, id string, current int, exists bool) error {
	switch {
	case ifMatch == "":
		return nil
	case ifMatch == "*":
		if !exists {
			return fmt.Errorf("%w: %s does not exist", ErrConflict, id)
		}
	case !exists || ifMatch != ETag(id, current):
		return fmt.Errorf("%w: %s is at %s", ErrConflict, id, ETag(id, current))
	}
	return nil
}

// PutProfile creates or replaces a profile. ifMatch ("" = unconditional)
// guards against lost updates.
func (r *Registry) PutProfile(p Profile, ifMatch, actor string) (Profile, error) {
	// Callers often edit a profile taken from the live snapshot; its slices
	// share memory with what concurrent evaluations are reading. Work on a
	// deep copy so nothing published is ever mutated.
	p = cloneProfile(p)
	var out Profile
	_, err := r.mutate(actor, "put", "profile", p.ID, func(s *Snapshot) (int, error) {
		old, exists := s.Profiles[p.ID]
		if err := checkMatch(ifMatch, p.ID, old.Version, exists); err != nil {
			return 0, err
		}
		normalizeProfile(&p)
		p.Version, p.UpdatedAt, p.UpdatedBy = old.Version+1, r.now(), actor
		if err := validateProfile(p, s); err != nil {
			return 0, err
		}
		s.Profiles[p.ID] = p
		out = p
		return p.Version, nil
	})
	return out, err
}

func (r *Registry) DeleteProfile(id, ifMatch, actor string) error {
	_, err := r.mutate(actor, "delete", "profile", id, func(s *Snapshot) (int, error) {
		old, ok := s.Profiles[id]
		if !ok {
			return 0, ErrNotFound
		}
		if err := checkMatch(ifMatch, id, old.Version, true); err != nil {
			return 0, err
		}
		delete(s.Profiles, id)
		return old.Version, nil
	})
	return err
}

func (r *Registry) PutRegion(rg Region, ifMatch, actor string) (Region, error) {
	rg = cloneRegion(rg)
	var out Region
	_, err := r.mutate(actor, "put", "region", rg.ID, func(s *Snapshot) (int, error) {
		old, exists := s.Regions[rg.ID]
		if err := checkMatch(ifMatch, rg.ID, old.Version, exists); err != nil {
			return 0, err
		}
		for i := range rg.Countries {
			rg.Countries[i] = strings.ToUpper(strings.TrimSpace(rg.Countries[i]))
		}
		for i := range rg.Subdivisions {
			rg.Subdivisions[i] = strings.ToUpper(strings.TrimSpace(rg.Subdivisions[i]))
		}
		rg.Version, rg.UpdatedAt, rg.UpdatedBy = old.Version+1, r.now(), actor
		if err := validateRegion(rg); err != nil {
			return 0, err
		}
		s.Regions[rg.ID] = rg
		out = rg
		return rg.Version, nil
	})
	return out, err
}

func (r *Registry) DeleteRegion(id, ifMatch, actor string) error {
	_, err := r.mutate(actor, "delete", "region", id, func(s *Snapshot) (int, error) {
		old, ok := s.Regions[id]
		if !ok {
			return 0, ErrNotFound
		}
		if err := checkMatch(ifMatch, id, old.Version, true); err != nil {
			return 0, err
		}
		for _, p := range s.Profiles {
			if contains(p.Regions, id) || slicesAny(p.Exceptions, func(e Exception) bool { return e.Region == id }) {
				return 0, fmt.Errorf("%w: region %s is used by profile %s", ErrInUse, id, p.ID)
			}
		}
		delete(s.Regions, id)
		return old.Version, nil
	})
	return err
}

func (r *Registry) PutProxy(p Proxy, actor string) (Proxy, error) {
	_, err := r.mutate(actor, "put", "proxy", p.ID, func(s *Snapshot) (int, error) {
		if !idPattern.MatchString(p.ID) || p.Name == "" || p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
			return 0, fmt.Errorf("%w: proxy needs an id, a name and valid coordinates", ErrInvalid)
		}
		s.Proxies[p.ID] = p
		return 1, nil
	})
	return p, err
}

func (r *Registry) DeleteProxy(id, actor string) error {
	_, err := r.mutate(actor, "delete", "proxy", id, func(s *Snapshot) (int, error) {
		if _, ok := s.Proxies[id]; !ok {
			return 0, ErrNotFound
		}
		delete(s.Proxies, id)
		return 1, nil
	})
	return err
}

// Audit returns the most recent n audit entries, newest first.
func (r *Registry) Audit(n int) []AuditEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AuditEntry, 0, n)
	for i := len(r.audit) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, r.audit[i])
	}
	return out
}

func (r *Registry) persist(s *Snapshot) error {
	if r.path == "" {
		return nil
	}
	p := persisted{Audit: r.audit}
	for _, v := range s.Regions {
		p.Regions = append(p.Regions, v)
	}
	for _, v := range s.Profiles {
		p.Profiles = append(p.Profiles, v)
	}
	for _, v := range s.Proxies {
		p.Proxies = append(p.Proxies, v)
	}
	sort.Slice(p.Regions, func(i, j int) bool { return p.Regions[i].ID < p.Regions[j].ID })
	sort.Slice(p.Profiles, func(i, j int) bool { return p.Profiles[i].ID < p.Profiles[j].ID })
	sort.Slice(p.Proxies, func(i, j int) bool { return p.Proxies[i].ID < p.Proxies[j].ID })
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// Atomic replace: readers of the file never see a partial write.
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".compliance-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), r.path)
}

// ---------- validation ----------

func normalizeProfile(p *Profile) {
	if p.Mode == "" {
		p.Mode = "allow_list"
	}
	if p.Scope == "" {
		p.Scope = "all_hops"
	}
	if p.Severity == "" {
		p.Severity = "high"
	}
	if p.MinLocationConfidence == 0 {
		p.MinLocationConfidence = 0.5
	}
	for i, c := range p.Classifications {
		p.Classifications[i] = normalizeClass(c)
	}
}

func normalizeClass(c string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(c)), " ", "_")
}

func validateProfile(p Profile, s *Snapshot) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: profile %s: %s", ErrInvalid, p.ID, fmt.Sprintf(format, a...))
	}
	switch {
	case !idPattern.MatchString(p.ID):
		return bad("id must match %s", idPattern)
	case p.Name == "":
		return bad("name is required")
	case len(p.Classifications) == 0:
		return bad("at least one data classification is required")
	case p.Mode != "allow_list" && p.Mode != "deny_list":
		return bad("mode must be allow_list or deny_list")
	case p.Scope != "all_hops" && p.Scope != "destination_only":
		return bad("scope must be all_hops or destination_only")
	case !contains(severities, p.Severity):
		return bad("severity must be one of %v", severities)
	case len(p.Regions) == 0:
		return bad("at least one region is required")
	case p.MinLocationConfidence < 0 || p.MinLocationConfidence > 1:
		return bad("min_location_confidence must be in [0,1]")
	case p.Law.Code == "" || p.Law.Citation == "":
		return bad("law.code and law.citation are required")
	case p.Mitigation.Template == "":
		return bad("mitigation.template is required")
	}
	if !contains([]string{"reroute", "block", "review", "contract"}, p.Mitigation.Action) {
		return bad("mitigation.action must be reroute, block, review or contract")
	}
	for _, id := range p.Regions {
		if _, ok := s.Regions[id]; !ok {
			return bad("unknown region %q", id)
		}
	}
	for _, e := range p.Exceptions {
		if _, ok := s.Regions[e.Region]; !ok {
			return bad("exception references unknown region %q", e.Region)
		}
		if e.Effect != "allow" && e.Effect != "downgrade" {
			return bad("exception effect must be allow or downgrade")
		}
	}
	return nil
}

func validateRegion(r Region) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: region %s: %s", ErrInvalid, r.ID, fmt.Sprintf(format, a...))
	}
	if !idPattern.MatchString(r.ID) {
		return bad("id must match %s", idPattern)
	}
	if r.Name == "" {
		return bad("name is required")
	}
	if len(r.Countries) == 0 && len(r.Subdivisions) == 0 && r.Polygon == nil {
		return bad("needs countries, subdivisions or a polygon")
	}
	for _, c := range r.Countries {
		if !iso2Pattern.MatchString(c) {
			return bad("country %q is not ISO 3166-1 alpha-2", c)
		}
	}
	for _, c := range r.Subdivisions {
		if !iso3166_2Pattern.MatchString(c) {
			return bad("subdivision %q is not ISO 3166-2", c)
		}
	}
	if r.Polygon != nil {
		if err := newPolygonIndex().addGeometry(r.ID, r.Name, *r.Polygon); err != nil {
			return bad("%v", err)
		}
	}
	return nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func slicesAny[T any](s []T, f func(T) bool) bool {
	for _, x := range s {
		if f(x) {
			return true
		}
	}
	return false
}

func cloneProfile(p Profile) Profile {
	p.Classifications = append([]string(nil), p.Classifications...)
	p.Regions = append([]string(nil), p.Regions...)
	p.Exceptions = append([]Exception(nil), p.Exceptions...)
	p.Law.Articles = append([]string(nil), p.Law.Articles...)
	p.Mitigation.Alternatives = append([]string(nil), p.Mitigation.Alternatives...)
	return p
}

func cloneRegion(r Region) Region {
	r.Countries = append([]string(nil), r.Countries...)
	r.Subdivisions = append([]string(nil), r.Subdivisions...)
	if r.Polygon != nil {
		g := Geometry{Type: r.Polygon.Type, Coordinates: append([]byte(nil), r.Polygon.Coordinates...)}
		r.Polygon = &g
	}
	return r
}
