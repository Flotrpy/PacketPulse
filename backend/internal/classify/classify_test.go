package classify

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestClassify(t *testing.T) {
	c, err := New("", RuleSet{Rules: []Rule{
		{ID: "pay", Enabled: true, Match: Match{Hosts: []string{"*.payments.example.in"}}, Classifications: []string{"payment_system_data"}},
		{ID: "eu-cloud", Enabled: true, Match: Match{CloudProviders: []string{"aws"}, CIDRs: []string{"18.96.0.0/16"}}, Classifications: []string{"eu_personal_data"}},
		{ID: "crm", Enabled: true, Match: Match{Apps: []string{"crm-eu"}, Ports: []int{443}}, Classifications: []string{"eu_personal_data"}},
		{ID: "off", Enabled: false, Match: Match{Hosts: []string{"*"}}, Classifications: []string{"nope_data"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr("18.96.32.10")
	r := c.Classify(Input{Host: "api.payments.example.in", IP: ip, Cloud: "aws", Port: 443}, nil)
	if !slices.Equal(r.Labels, []string{"eu_personal_data", "payment_system_data"}) || r.Source != "rules" || !slices.Equal(r.RuleIDs, []string{"pay", "eu-cloud"}) {
		t.Fatalf("%+v", r)
	}
	if r := c.Classify(Input{Host: "payments.example.in"}, nil); r.Source != "rules" {
		t.Fatalf("bare domain should match *.: %+v", r)
	}
	if r := c.Classify(Input{Host: "payments.example.in.evil.com"}, nil); r.Source != "none" {
		t.Fatalf("suffix trick matched: %+v", r)
	}
	if r := c.Classify(Input{App: "crm-eu", Port: 80}, nil); r.Source != "none" {
		t.Fatalf("all fields must match: %+v", r)
	}
	if r := c.Classify(Input{Host: "api.payments.example.in"}, []string{"Sensitive EU Citizen Data"}); r.Source != "request" || r.Labels[0] != "sensitive_eu_citizen_data" {
		t.Fatalf("explicit wins: %+v", r)
	}
}

func TestReplaceValidatesAndPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rules.json")
	c, err := New(p, DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	set := c.Rules()
	for _, bad := range []Rule{
		{ID: "x1", Enabled: true, Match: Match{}, Classifications: []string{"a_b"}},
		{ID: "x2", Enabled: true, Match: Match{CIDRs: []string{"10.0.0.0/33"}}, Classifications: []string{"a_b"}},
		{ID: "x3", Enabled: true, Match: Match{Ports: []int{1}}, Classifications: []string{"Bad Label"}},
	} {
		if _, err := c.Replace(RuleSet{Rules: []Rule{bad}}, 0, "t"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", bad.ID, err)
		}
	}
	if _, err := c.Replace(set, set.Version+3, "t"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	set.Rules = append(set.Rules, Rule{ID: "new", Enabled: true, Match: Match{Ports: []int{5432}}, Classifications: []string{"db_replication"}})
	saved, err := c.Replace(set, set.Version, "alice")
	if err != nil || saved.Version != 2 {
		t.Fatalf("%v %+v", err, saved)
	}
	c2, _ := New(p, RuleSet{})
	if r := c2.Classify(Input{Port: 5432}, nil); !slices.Equal(r.Labels, []string{"db_replication"}) {
		t.Fatalf("not persisted: %+v", r)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("rules file mode %v", fi.Mode().Perm())
	}
}
