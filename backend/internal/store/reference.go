package store

import (
	"net/netip"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// MemoryReference is the in-memory ReferenceStore. Writes (seeding/loading)
// happen before serving; reads are lock-protected so hot reloads are safe.
type MemoryReference struct {
	geoFallback func(netip.Addr) (GeoHit, bool)

	mu         sync.RWMutex
	asn        rangeTable
	geo        []GeoHit // sorted by prefix length, longest first
	cities     map[string]City
	countries  map[string]Country
	cables     map[string]Cable
	byCountry  map[string]map[string]bool // iso2 -> cable ids
	carriers   map[int]Carrier
	clouds     map[int]string
	policies   map[string]Policy
	policyList []string
}

func NewMemoryReference() *MemoryReference {
	return &MemoryReference{
		cities:    map[string]City{},
		countries: map[string]Country{},
		cables:    map[string]Cable{},
		byCountry: map[string]map[string]bool{},
		carriers:  map[int]Carrier{},
		clouds:    map[int]string{},
		policies:  map[string]Policy{},
	}
}

// ---------- writers ----------

func (r *MemoryReference) AddASNRange(start, end netip.Addr, info ASNInfo) {
	r.mu.Lock()
	r.asn.add(start, end, info)
	r.mu.Unlock()
}

// ReplaceASNRanges swaps the whole ASN table (used by bulk loaders).
func (r *MemoryReference) ReplaceASNRanges(t rangeTable) {
	t.sort()
	r.mu.Lock()
	r.asn = t
	r.mu.Unlock()
}

func (r *MemoryReference) AddGeoPrefix(g GeoHit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.geo = append(r.geo, g)
	sortGeoHits(r.geo)
}

// sortGeoHits orders prefixes longest first so the first match is the most specific.
func sortGeoHits(g []GeoHit) {
	sort.SliceStable(g, func(i, j int) bool { return g[i].Prefix.Bits() > g[j].Prefix.Bits() })
}

func (r *MemoryReference) AddCity(c City) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range c.Tokens {
		r.cities[strings.ToLower(t)] = c
	}
}

func (r *MemoryReference) UpsertCountry(c Country) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.countries[c.ISO2]; ok {
		c.Island = c.Island || old.Island
		if c.Continent == "" {
			c.Continent = old.Continent
		}
	}
	r.countries[c.ISO2] = c
}

func (r *MemoryReference) AddCable(c Cable) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.cables[c.ID]; ok {
		for _, l := range old.Landings {
			delete(r.byCountry[l.CountryISO2], c.ID)
		}
	}
	r.cables[c.ID] = c
	for _, l := range c.Landings {
		if r.byCountry[l.CountryISO2] == nil {
			r.byCountry[l.CountryISO2] = map[string]bool{}
		}
		r.byCountry[l.CountryISO2][c.ID] = true
	}
}

func (r *MemoryReference) AddCarrier(c Carrier) { r.mu.Lock(); r.carriers[c.ASN] = c; r.mu.Unlock() }

func (r *MemoryReference) AddCloud(asn int, provider string) {
	r.mu.Lock()
	r.clouds[asn] = provider
	r.mu.Unlock()
}

func (r *MemoryReference) AddPolicy(p Policy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.policies[p.ID]; !ok {
		r.policyList = append(r.policyList, p.ID)
	}
	r.policies[p.ID] = p
}

// ---------- ReferenceStore ----------

func (r *MemoryReference) LookupASN(addr netip.Addr) (ASNInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.asn.lookup(addr.Unmap())
}

func (r *MemoryReference) LookupGeo(addr netip.Addr) (GeoHit, bool) {
	addr = addr.Unmap()
	r.mu.RLock()
	fallback := r.geoFallback
	for _, g := range r.geo {
		if g.Prefix.Contains(addr) {
			r.mu.RUnlock()
			return g, true
		}
	}
	r.mu.RUnlock()
	if fallback != nil {
		return fallback(addr)
	}
	return GeoHit{}, false
}

// SetGeoFallback plugs in a full IP geolocation database (e.g. DB-IP Lite or
// MaxMind GeoLite2 City) for addresses the curated prefixes don't cover.
func (r *MemoryReference) SetGeoFallback(f func(netip.Addr) (GeoHit, bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.geoFallback = f
}

// MatchHostname looks for a known city token in a router hostname, e.g.
// "if-be-2-2.ecore1.emrs2-marseille.as6453.net" -> Marseille. The registrable
// domain (last two labels) is ignored; trailing digits are stripped
// ("lon13" -> "lon"). Longer tokens win, so full names beat IATA codes.
func (r *MemoryReference) MatchHostname(hostname string) (City, string, bool) {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(hostname), "."), ".")
	if len(labels) <= 2 {
		return City{}, "", false
	}
	labels = labels[:len(labels)-2]

	r.mu.RLock()
	defer r.mu.RUnlock()
	var best City
	bestTok := ""
	for _, label := range labels {
		toks := strings.FieldsFunc(label, func(c rune) bool { return c == '-' || c == '_' })
		if len(toks) > 1 { // "los-angeles" -> also try "losangeles"
			joined := ""
			for _, t := range toks {
				joined += strings.TrimRightFunc(t, unicode.IsDigit)
			}
			toks = append(toks, joined)
		}
		for _, tok := range toks {
			for _, cand := range []string{tok, strings.TrimRightFunc(tok, unicode.IsDigit)} {
				if len(cand) < 3 {
					continue
				}
				if c, ok := r.cities[cand]; ok && len(cand) > len(bestTok) {
					best, bestTok = c, cand
				}
			}
		}
	}
	return best, bestTok, bestTok != ""
}

func (r *MemoryReference) Country(iso2 string) (Country, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.countries[strings.ToUpper(iso2)]
	return c, ok
}

// CablesBetween returns in-service cables with landings in both countries.
func (r *MemoryReference) CablesBetween(a, b string) []Cable {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Cable
	for id := range r.byCountry[a] {
		if r.byCountry[b][id] && !r.cables[id].Planned {
			out = append(out, r.cables[id])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *MemoryReference) MobileCarrier(asn int) (Carrier, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.carriers[asn]
	return c, ok
}

func (r *MemoryReference) CloudProvider(asn int) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.clouds[asn]
	return p, ok
}

func (r *MemoryReference) Policy(id string) (Policy, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.policies[id]
	return p, ok
}

func (r *MemoryReference) Policies() []Policy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Policy, 0, len(r.policyList))
	for _, id := range r.policyList {
		out = append(out, r.policies[id])
	}
	return out
}

// Stats reports table sizes (for startup logs and /healthz).
func (r *MemoryReference) Stats() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return map[string]int{
		"asn_ranges": len(r.asn.starts), "geo_prefixes": len(r.geo), "city_tokens": len(r.cities),
		"countries": len(r.countries), "cables": len(r.cables), "carriers": len(r.carriers),
		"cloud_asns": len(r.clouds), "policies": len(r.policies),
	}
}

// ---------- range table (sorted, non-overlapping ranges; binary search) ----------

type rangeTable struct {
	starts, ends []netip.Addr
	vals         []ASNInfo
}

func (t *rangeTable) add(start, end netip.Addr, v ASNInfo) {
	t.starts = append(t.starts, start.Unmap())
	t.ends = append(t.ends, end.Unmap())
	t.vals = append(t.vals, v)
	t.sort()
}

func (t *rangeTable) sort() {
	if sort.SliceIsSorted(t.starts, func(i, j int) bool { return t.starts[i].Less(t.starts[j]) }) {
		return
	}
	idx := make([]int, len(t.starts))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return t.starts[idx[a]].Less(t.starts[idx[b]]) })
	s, e, v := make([]netip.Addr, len(idx)), make([]netip.Addr, len(idx)), make([]ASNInfo, len(idx))
	for i, k := range idx {
		s[i], e[i], v[i] = t.starts[k], t.ends[k], t.vals[k]
	}
	t.starts, t.ends, t.vals = s, e, v
}

func (t *rangeTable) lookup(a netip.Addr) (ASNInfo, bool) {
	// first start > a, then step back one
	i := sort.Search(len(t.starts), func(i int) bool { return a.Less(t.starts[i]) }) - 1
	if i < 0 || t.ends[i].Less(a) {
		return ASNInfo{}, false
	}
	return t.vals[i], true
}
