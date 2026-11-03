// Package classify tags traced traffic with data classifications
// (e.g. "payment_system_data", "sensitive_eu_citizen_data") at ingestion,
// so the compliance engine knows which sovereignty profiles apply.
//
// Rules match on what the ingestion side knows about a flow: destination
// hostname (glob), destination IP range, destination ASN, cloud provider,
// port, the agent's application label and TLS SNI. Every matching rule adds
// its labels (a payments API serving EU customers can be both). An explicit
// classification on the request always wins over rules.
package classify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Match lists the conditions of a rule. Within one field any value may
// match; across fields all non-empty fields must match.
type Match struct {
	Hosts          []string `json:"hosts,omitempty"`           // globs: "*.payments.example.in", "api.example.com"
	SNI            []string `json:"sni,omitempty"`             // globs on the TLS server name reported by the agent
	CIDRs          []string `json:"cidrs,omitempty"`           // destination IP ranges
	ASNs           []int    `json:"asns,omitempty"`            // destination network
	CloudProviders []string `json:"cloud_providers,omitempty"` // aws, gcp, azure, ...
	Ports          []int    `json:"ports,omitempty"`
	Apps           []string `json:"apps,omitempty"` // agent application labels, e.g. "payments-api"
}

type Rule struct {
	ID              string   `json:"id"`
	Description     string   `json:"description,omitempty"`
	Enabled         bool     `json:"enabled"`
	Match           Match    `json:"match"`
	Classifications []string `json:"classifications"`
}

// RuleSet is the persisted document.
type RuleSet struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	Rules     []Rule    `json:"rules"`
	// Default labels for traffic no rule matches (usually empty: untagged
	// traffic is not governed by any profile).
	Default []string `json:"default,omitempty"`
}

// Input is what is known about one traced flow.
type Input struct {
	Host  string // destination hostname as submitted (may be empty for raw IPs)
	SNI   string
	IP    netip.Addr
	ASN   int
	Cloud string
	Port  int
	App   string
}

// Result says which labels apply and why.
type Result struct {
	Labels  []string `json:"labels"`
	Source  string   `json:"source"` // request | rules | default | none
	RuleIDs []string `json:"rule_ids,omitempty"`
}

var (
	ErrInvalid  = errors.New("invalid classification rules")
	ErrConflict = errors.New("version conflict")
	labelRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{1,63}$`)
	idRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)
)

type compiledRule struct {
	Rule
	cidrs []netip.Prefix
}

type compiled struct {
	set   RuleSet
	rules []compiledRule
}

// Classifier holds the live rule set; reads are lock-free and updates
// (validated, persisted) are swapped in atomically.
type Classifier struct {
	mu   sync.Mutex
	cur  atomic.Pointer[compiled]
	path string
}

// New loads rules from path (created from defaults when missing); an empty
// path keeps them in memory only.
func New(path string, defaults RuleSet) (*Classifier, error) {
	c := &Classifier{path: path}
	set := defaults
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(b, &set); err != nil {
				return nil, fmt.Errorf("classification rules %s: %w", path, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if set.Version == 0 {
		set.Version, set.UpdatedAt, set.UpdatedBy = 1, time.Now().UTC(), "system"
	}
	cp, err := compile(set)
	if err != nil {
		return nil, err
	}
	c.cur.Store(cp)
	if path != "" {
		if err := c.persist(set); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Classifier) Rules() RuleSet { return c.cur.Load().set }

// Replace swaps in a new rule set. ifVersion > 0 guards against lost updates.
func (c *Classifier) Replace(set RuleSet, ifVersion int, actor string) (RuleSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.cur.Load().set
	if ifVersion > 0 && ifVersion != cur.Version {
		return RuleSet{}, fmt.Errorf("%w: rules are at version %d", ErrConflict, cur.Version)
	}
	set.Version, set.UpdatedAt, set.UpdatedBy = cur.Version+1, time.Now().UTC(), actor
	cp, err := compile(set)
	if err != nil {
		return RuleSet{}, err
	}
	if err := c.persist(set); err != nil {
		return RuleSet{}, err
	}
	c.cur.Store(cp)
	return set, nil
}

func compile(set RuleSet) (*compiled, error) {
	cp := &compiled{set: set}
	seen := map[string]bool{}
	for _, l := range set.Default {
		if !labelRE.MatchString(l) {
			return nil, fmt.Errorf("%w: default label %q", ErrInvalid, l)
		}
	}
	for _, r := range set.Rules {
		bad := func(f string, a ...any) error {
			return fmt.Errorf("%w: rule %s: %s", ErrInvalid, r.ID, fmt.Sprintf(f, a...))
		}
		if !idRE.MatchString(r.ID) {
			return nil, bad("id must match %s", idRE)
		}
		if seen[r.ID] {
			return nil, bad("duplicate id")
		}
		seen[r.ID] = true
		if len(r.Classifications) == 0 {
			return nil, bad("at least one classification is required")
		}
		for _, l := range r.Classifications {
			if !labelRE.MatchString(l) {
				return nil, bad("label %q must be lower_snake_case", l)
			}
		}
		m := r.Match
		if len(m.Hosts)+len(m.SNI)+len(m.CIDRs)+len(m.ASNs)+len(m.CloudProviders)+len(m.Ports)+len(m.Apps) == 0 {
			return nil, bad("match has no conditions (it would tag all traffic; use default instead)")
		}
		cr := compiledRule{Rule: r}
		for _, g := range append(append([]string{}, m.Hosts...), m.SNI...) {
			if _, err := path.Match(strings.ToLower(g), "x"); err != nil {
				return nil, bad("bad glob %q", g)
			}
		}
		for _, s := range m.CIDRs {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, bad("bad cidr %q", s)
			}
			cr.cidrs = append(cr.cidrs, p.Masked())
		}
		cp.rules = append(cp.rules, cr)
	}
	return cp, nil
}

func (c *Classifier) persist(set RuleSet) error {
	if c.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// Classify returns the labels for a flow. explicit (from the request) wins.
func (c *Classifier) Classify(in Input, explicit []string) Result {
	if len(explicit) > 0 {
		return Result{Labels: Normalize(explicit), Source: "request"}
	}
	cp := c.cur.Load()
	labels := map[string]bool{}
	var ids []string
	for _, r := range cp.rules {
		if r.Enabled && r.matches(in) {
			ids = append(ids, r.ID)
			for _, l := range r.Classifications {
				labels[l] = true
			}
		}
	}
	if len(ids) > 0 {
		return Result{Labels: keys(labels), Source: "rules", RuleIDs: ids}
	}
	if len(cp.set.Default) > 0 {
		return Result{Labels: append([]string(nil), cp.set.Default...), Source: "default"}
	}
	return Result{Labels: []string{}, Source: "none"}
}

func (r compiledRule) matches(in Input) bool {
	m := r.Match
	if len(m.Hosts) > 0 && !globAny(m.Hosts, in.Host) {
		return false
	}
	if len(m.SNI) > 0 && !globAny(m.SNI, in.SNI) {
		return false
	}
	if len(r.cidrs) > 0 {
		ok := false
		for _, p := range r.cidrs {
			if in.IP.IsValid() && p.Contains(in.IP.Unmap()) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(m.ASNs) > 0 && !containsInt(m.ASNs, in.ASN) {
		return false
	}
	if len(m.CloudProviders) > 0 && !containsFold(m.CloudProviders, in.Cloud) {
		return false
	}
	if len(m.Ports) > 0 && !containsInt(m.Ports, in.Port) {
		return false
	}
	if len(m.Apps) > 0 && !containsFold(m.Apps, in.App) {
		return false
	}
	return true
}

// globAny: "*" matches within one DNS label; a leading "*." also matches
// the bare domain and any depth of subdomain ("*.example.in" matches
// "example.in" and "a.b.example.in").
func globAny(globs []string, s string) bool {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if s == "" {
		return false
	}
	for _, g := range globs {
		g = strings.TrimSuffix(strings.ToLower(g), ".")
		if ok, _ := path.Match(g, s); ok {
			return true
		}
		if suffix, ok := strings.CutPrefix(g, "*."); ok && !strings.ContainsAny(suffix, "*?[") && (s == suffix || strings.HasSuffix(s, "."+suffix)) {
			return true
		}
	}
	return false
}

// Normalize lower-cases labels, turns spaces into underscores, dedupes and sorts.
func Normalize(ls []string) []string {
	m := map[string]bool{}
	for _, l := range ls {
		if l = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(l)), " ", "_"); l != "" {
			m[l] = true
		}
	}
	return keys(m)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsFold(s []string, v string) bool {
	for _, x := range s {
		if v != "" && strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// ValidLabel reports whether l is an acceptable classification label.
func ValidLabel(l string) bool { return labelRE.MatchString(l) }
