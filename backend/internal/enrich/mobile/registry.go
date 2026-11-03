// Package mobile identifies mobile network operators (MNOs), their regional
// gateway hubs, and cell tower positions for the cellular first leg of a path.
package mobile

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Operator is one MNO network (an ASN that carries handset traffic).
type Operator struct {
	ASN        int      `json:"asn"`
	Name       string   `json:"name"`
	Brand      string   `json:"brand,omitempty"`
	Country    string   `json:"country"`
	MCC        string   `json:"mcc,omitempty"`
	MNCs       []string `json:"mncs,omitempty"`
	Source     string   `json:"source"` // seed | peeringdb_mccmnc | maxmind_isp | device
	Confidence float64  `json:"confidence"`
}

// Registry indexes operators by ASN and by MCC-MNC.
type Registry struct {
	byASN    map[int][]Operator
	byMCCMNC map[string][]Operator
}

func NewRegistry() *Registry {
	return &Registry{byASN: map[int][]Operator{}, byMCCMNC: map[string][]Operator{}}
}

func (r *Registry) Add(op Operator) {
	if op.ASN != 0 {
		r.byASN[op.ASN] = append(r.byASN[op.ASN], op)
		sort.SliceStable(r.byASN[op.ASN], func(i, j int) bool { return r.byASN[op.ASN][i].Confidence > r.byASN[op.ASN][j].Confidence })
	}
	for _, mnc := range op.MNCs {
		k := op.MCC + "-" + mnc
		r.byMCCMNC[k] = append(r.byMCCMNC[k], op)
		sort.SliceStable(r.byMCCMNC[k], func(i, j int) bool { return r.byMCCMNC[k][i].Confidence > r.byMCCMNC[k][j].Confidence })
	}
}

// ByASN returns the most confident operator entry for an ASN.
func (r *Registry) ByASN(asn int) (Operator, bool) {
	ops := r.byASN[asn]
	if len(ops) == 0 {
		return Operator{}, false
	}
	return ops[0], true
}

// ByMCCMNC returns operators registered for a mobile network code.
func (r *Registry) ByMCCMNC(mcc, mnc string) []Operator {
	return r.byMCCMNC[mcc+"-"+mnc]
}

func (r *Registry) Len() int { return len(r.byASN) }

// Seed returns hand-verified MNO ASNs (names checked against iptoasn
// 2026-11-02) with their MCC/MNC allocations.
func Seed() []Operator {
	s := func(asn int, name, brand, cc, mcc string, mncs ...string) Operator {
		return Operator{ASN: asn, Name: name, Brand: brand, Country: cc, MCC: mcc, MNCs: mncs, Source: "seed", Confidence: 0.95}
	}
	return []Operator{
		s(55836, "Reliance Jio Infocomm", "Jio", "IN", "405", "840", "854", "855", "856", "857", "858", "859", "860", "861", "862", "863", "864", "865", "866", "867", "868", "869", "870", "871", "872", "873", "874"),
		s(45609, "Bharti Airtel (GPRS/mobile)", "Airtel", "IN", "404", "10", "45", "49", "90", "92", "93", "94", "95", "96", "97", "98"),
		s(38266, "Vodafone Idea", "Vi", "IN", "404", "20", "11", "05", "27", "43", "46", "60", "84", "86"),
		s(21928, "T-Mobile USA", "T-Mobile", "US", "310", "260"),
		s(6167, "Verizon Wireless (Cellco)", "Verizon", "US", "311", "480"),
		s(22394, "Verizon Wireless (Cellco)", "Verizon", "US", "311", "480"),
		s(20057, "AT&T Mobility", "AT&T", "US", "310", "410"),
		s(25135, "Vodafone UK", "Vodafone", "GB", "234", "15"),
		s(16135, "Turkcell", "Turkcell", "TR", "286", "01"),
		s(23693, "Telkomsel", "Telkomsel", "ID", "510", "10"),
		s(24203, "XL Axiata", "XL", "ID", "510", "11"),
		s(132199, "Globe Telecom (mobile)", "Globe", "PH", "515", "02"),
		s(9808, "China Mobile (Guangdong)", "China Mobile", "CN", "460", "00", "02", "07"),
	}
}

// --- PeeringDB x MCC-MNC name matching ---

var tokenRE = regexp.MustCompile(`[a-z0-9]+`)

var stopWords = map[string]bool{
	"telecom": true, "telecommunications": true, "telecomunicaciones": true, "telecomunicacoes": true, "mobile": true,
	"communications": true, "limited": true, "ltd": true, "inc": true, "corp": true, "corporation": true, "group": true,
	"networks": true, "network": true, "wireless": true, "cellular": true, "telefonica": true, "plc": true, "llc": true,
	"gmbh": true, "the": true, "and": true, "company": true, "services": true, "international": true, "global": true,
	"digital": true, "tel": true, "net": true, "data": true, "broadband": true, "operator": true, "national": true,
}

func distinctive(s string) string {
	for _, w := range tokenRE.FindAllString(strings.ToLower(s), -1) {
		if len(w) >= 4 && !stopWords[w] {
			return w
		}
	}
	return ""
}

type mccmncRow struct {
	CountryCode string `json:"countryCode"`
	MCC         string `json:"mcc"`
	MNC         string `json:"mnc"`
	Brand       string `json:"brand"`
	Operator    string `json:"operator"`
	Status      string `json:"status"`
}

type pdbNet struct {
	ASN      int    `json:"asn"`
	OrgID    int    `json:"org_id"`
	Name     string `json:"name"`
	AKA      string `json:"aka"`
	NameLong string `json:"name_long"`
	InfoType string `json:"info_type"`
}

type pdbOrg struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Country string `json:"country"`
}

// LoadPeeringDBMatches adds operators found by matching operational MCC-MNC
// entries (brand/operator name) to PeeringDB networks registered in the same
// country. PeeringDB has no "mobile" network type, so this is a name match:
// confidence 0.55, below the hand-verified seed.
func (r *Registry) LoadPeeringDBMatches(mccmncPath, netPath, orgPath string) (int, error) {
	var rows []mccmncRow
	if err := readJSON(mccmncPath, &rows); err != nil {
		return 0, err
	}
	var nets struct{ Data []pdbNet }
	if err := readJSON(netPath, &nets); err != nil {
		return 0, err
	}
	var orgs struct{ Data []pdbOrg }
	if err := readJSON(orgPath, &orgs); err != nil {
		return 0, err
	}
	orgByID := map[int]pdbOrg{}
	for _, o := range orgs.Data {
		orgByID[o.ID] = o
	}
	// (country, token) -> nets
	idx := map[[2]string][]pdbNet{}
	for _, n := range nets.Data {
		switch n.InfoType {
		case "Cable/DSL/ISP", "NSP", "":
		default:
			continue
		}
		cc := orgByID[n.OrgID].Country
		seen := map[string]bool{}
		for _, s := range []string{n.Name, n.AKA, n.NameLong, orgByID[n.OrgID].Name} {
			if t := distinctive(s); t != "" && !seen[t] {
				seen[t] = true
				idx[[2]string{cc, t}] = append(idx[[2]string{cc, t}], n)
			}
		}
	}
	type key struct {
		asn int
		mcc string
	}
	found := map[key]*Operator{}
	for _, row := range rows {
		cc := row.CountryCode
		if row.Status != "Operational" || len(cc) != 2 {
			continue
		}
		for _, t := range []string{distinctive(row.Brand), distinctive(row.Operator)} {
			if t == "" {
				continue
			}
			for _, n := range idx[[2]string{cc, t}] {
				if _, seeded := r.byASN[n.ASN]; seeded {
					continue
				}
				k := key{n.ASN, row.MCC}
				op := found[k]
				if op == nil {
					op = &Operator{ASN: n.ASN, Name: n.Name, Brand: row.Brand, Country: cc, MCC: row.MCC, Source: "peeringdb_mccmnc", Confidence: 0.55}
					found[k] = op
				}
				op.MNCs = appendUnique(op.MNCs, row.MNC)
			}
		}
	}
	for _, op := range found {
		r.Add(*op)
	}
	return len(found), nil
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var rd io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		rd = gz
	}
	return json.NewDecoder(rd).Decode(v)
}
