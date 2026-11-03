package store

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LoadReport says what LoadDataDir loaded and what it skipped.
type LoadReport struct {
	Loaded   map[string]int
	Warnings []string
}

func (r *LoadReport) warn(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// LoadDataDir layers the research snapshot (the repo's data/ directory) on
// top of whatever is already in r. Missing files are reported, not fatal, so
// the seed fixtures keep working without the full datasets.
func LoadDataDir(r *MemoryReference, dataDir string) (*LoadReport, error) {
	rep := &LoadReport{Loaded: map[string]int{}}
	if st, err := os.Stat(dataDir); err != nil || !st.IsDir() {
		return rep, fmt.Errorf("data dir %q not found", dataDir)
	}
	raw := filepath.Join(dataDir, "raw")

	names, err := loadCountries(r, filepath.Join(raw, "naturalearth", "ne_50m_admin_0_countries.geojson.gz"))
	note(rep, "country_names", len(names), err)

	n, err := loadIPToASN(r, filepath.Join(raw, "iptoasn", "ip2asn-combined.tsv.gz"))
	note(rep, "asn_ranges", n, err)

	n, err = loadCables(r, filepath.Join(raw, "telegeography"), names)
	note(rep, "cables", n, err)

	n, err = loadAWS(r, filepath.Join(raw, "cloud", "aws-ip-ranges.json.gz"))
	note(rep, "aws_prefixes", n, err)
	n, err = loadGCP(r, filepath.Join(raw, "cloud", "gcp-cloud.json"))
	note(rep, "gcp_prefixes", n, err)

	n, err = loadPolicies(r, filepath.Join(dataDir, "curated", "regulations", "compliance_zones.json"))
	note(rep, "policies", n, err)
	return rep, nil
}

func note(rep *LoadReport, what string, n int, err error) {
	if err != nil {
		rep.warn("%s: %v", what, err)
		return
	}
	rep.Loaded[what] = n
}

func openMaybeGzip(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{gz, closers{gz, f}}, nil
}

type closers []io.Closer

func (c closers) Close() error {
	var errs []error
	for _, x := range c {
		errs = append(errs, x.Close())
	}
	return errors.Join(errs...)
}

// ---------- Natural Earth countries ----------

func loadCountries(r *MemoryReference, path string) (map[string]string, error) {
	rc, err := openMaybeGzip(path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var fc struct {
		Features []struct {
			Properties map[string]any `json:"properties"`
		} `json:"features"`
	}
	if err := json.NewDecoder(rc).Decode(&fc); err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, f := range fc.Features {
		p := f.Properties
		iso, _ := p["ISO_A2"].(string)
		if iso == "-99" || iso == "" {
			iso, _ = p["ISO_A2_EH"].(string)
		}
		if iso == "-99" || iso == "" {
			continue
		}
		name, _ := p["NAME"].(string)
		cont, _ := p["CONTINENT"].(string)
		lat, _ := p["LABEL_Y"].(float64)
		lon, _ := p["LABEL_X"].(float64)
		r.UpsertCountry(Country{ISO2: iso, Name: name, Continent: cont, Lat: lat, Lon: lon})
		for _, k := range []string{"NAME", "NAME_LONG", "ADMIN", "FORMAL_EN", "NAME_SORT", "BRK_NAME", "GEOUNIT"} {
			if v, ok := p[k].(string); ok && v != "" {
				names[strings.ToLower(v)] = iso
			}
		}
	}
	for k, v := range countryAliases {
		names[k] = v
	}
	return names, nil
}

// TeleGeography country spellings that differ from Natural Earth.
var countryAliases = map[string]string{
	"united states": "US", "russia": "RU", "korea, rep.": "KR", "congo, dem. rep.": "CD",
	"congo, rep.": "CG", "côte d'ivoire": "CI", "egypt": "EG", "iran": "IR", "hong kong": "HK",
	"macau": "MO", "taiwan": "TW", "syria": "SY", "yemen": "YE", "gambia": "GM", "bahamas": "BS",
	"micronesia": "FM", "turkey": "TR", "türkiye": "TR", "vietnam": "VN", "brunei": "BN", "laos": "LA",
	"virgin islands (u.s.)": "VI", "virgin islands (u.k.)": "VG", "saint martin": "MF", "sint maarten": "SX",
}

// ---------- iptoasn ----------

func loadIPToASN(r *MemoryReference, path string) (int, error) {
	rc, err := openMaybeGzip(path)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	var t rangeTable
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), "\t", 5)
		if len(f) != 5 || f[2] == "0" {
			continue
		}
		s, err1 := netip.ParseAddr(f[0])
		e, err2 := netip.ParseAddr(f[1])
		asn, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		t.starts = append(t.starts, s)
		t.ends = append(t.ends, e)
		t.vals = append(t.vals, ASNInfo{ASN: asn, RegistryCountry: f[3], Name: f[4]})
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	r.ReplaceASNRanges(t)
	return len(t.starts), nil
}

// ---------- TeleGeography cables ----------

func loadCables(r *MemoryReference, dir string, names map[string]string) (int, error) {
	if names == nil {
		return 0, errors.New("country names unavailable (Natural Earth not loaded)")
	}
	rc, err := openMaybeGzip(filepath.Join(dir, "landing-point_landing-point-geo.json.gz"))
	if err != nil {
		return 0, err
	}
	var lp struct {
		Features []struct {
			Properties struct {
				ID string `json:"id"`
			} `json:"properties"`
			Geometry struct {
				Coordinates []float64 `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	err = json.NewDecoder(rc).Decode(&lp)
	rc.Close()
	if err != nil {
		return 0, err
	}
	coords := map[string][2]float64{}
	for _, f := range lp.Features {
		if len(f.Geometry.Coordinates) == 2 {
			coords[f.Properties.ID] = [2]float64{f.Geometry.Coordinates[1], f.Geometry.Coordinates[0]}
		}
	}

	tf, err := openMaybeGzip(filepath.Join(dir, "cables-detail.tar.gz"))
	if err != nil {
		return 0, err
	}
	defer tf.Close()
	tr := tar.NewReader(tf)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, err
		}
		if h.Typeflag != tar.TypeReg || !strings.HasSuffix(h.Name, ".json") {
			continue
		}
		var c struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Length        string `json:"length"`
			Owners        string `json:"owners"`
			RFSYear       int    `json:"rfs_year"`
			IsPlanned     bool   `json:"is_planned"`
			LandingPoints []struct {
				ID, Name, Country string
			} `json:"landing_points"`
		}
		if err := json.NewDecoder(tr).Decode(&c); err != nil {
			continue
		}
		cable := Cable{ID: c.ID, Name: c.Name, RFSYear: c.RFSYear, Planned: c.IsPlanned}
		if f := strings.Fields(c.Length); len(f) > 0 {
			cable.LengthKM, _ = strconv.ParseFloat(strings.ReplaceAll(f[0], ",", ""), 64)
		}
		for _, o := range strings.Split(c.Owners, ",") {
			if o = strings.TrimSpace(o); o != "" {
				cable.Owners = append(cable.Owners, o)
			}
		}
		for _, l := range c.LandingPoints {
			iso, ok := names[strings.ToLower(l.Country)]
			ll, ok2 := coords[l.ID]
			if !ok || !ok2 {
				continue
			}
			cable.Landings = append(cable.Landings, LandingPoint{ID: l.ID, Name: l.Name, CountryISO2: iso, Lat: ll[0], Lon: ll[1]})
		}
		if len(cable.Landings) >= 2 {
			r.AddCable(cable)
			n++
		}
	}
	return n, nil
}

// ---------- cloud ranges -> prefix geolocation ----------

type regionLoc struct {
	City, CC string
	Lat, Lon float64
}

func addCloudPrefix(r *MemoryReference, p string, region regionLoc) bool {
	pfx, err := netip.ParsePrefix(p)
	if err != nil {
		return false
	}
	r.mu.Lock()
	r.geo = append(r.geo, GeoHit{Prefix: pfx, City: region.City, CountryISO2: region.CC, Lat: region.Lat, Lon: region.Lon, AccuracyKM: 100})
	r.mu.Unlock()
	return true
}

func loadAWS(r *MemoryReference, path string) (int, error) {
	rc, err := openMaybeGzip(path)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	var doc struct {
		Prefixes []struct {
			IPPrefix string `json:"ip_prefix"`
			Region   string `json:"region"`
			Service  string `json:"service"`
		} `json:"prefixes"`
		IPv6Prefixes []struct {
			IPv6Prefix string `json:"ipv6_prefix"`
			Region     string `json:"region"`
			Service    string `json:"service"`
		} `json:"ipv6_prefixes"`
	}
	if err := json.NewDecoder(rc).Decode(&doc); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range doc.Prefixes {
		if loc, ok := awsRegions[p.Region]; ok && p.Service == "EC2" && addCloudPrefix(r, p.IPPrefix, loc) {
			n++
		}
	}
	for _, p := range doc.IPv6Prefixes {
		if loc, ok := awsRegions[p.Region]; ok && p.Service == "EC2" && addCloudPrefix(r, p.IPv6Prefix, loc) {
			n++
		}
	}
	r.sortGeo()
	return n, nil
}

func loadGCP(r *MemoryReference, path string) (int, error) {
	rc, err := openMaybeGzip(path)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	var doc struct {
		Prefixes []struct {
			IPv4  string `json:"ipv4Prefix"`
			IPv6  string `json:"ipv6Prefix"`
			Scope string `json:"scope"`
		} `json:"prefixes"`
	}
	if err := json.NewDecoder(rc).Decode(&doc); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range doc.Prefixes {
		loc, ok := gcpRegions[p.Scope]
		if !ok {
			continue
		}
		for _, s := range []string{p.IPv4, p.IPv6} {
			if s != "" && addCloudPrefix(r, s, loc) {
				n++
			}
		}
	}
	r.sortGeo()
	return n, nil
}

func (r *MemoryReference) sortGeo() {
	r.mu.Lock()
	defer r.mu.Unlock()
	sortGeoHits(r.geo)
}

// ---------- compliance policies ----------

func loadPolicies(r *MemoryReference, path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("%s missing", path)
		}
		return 0, err
	}
	var doc struct {
		Zones map[string]struct {
			Members        []string `json:"members"`
			MembersExtends string   `json:"members_extends"`
			MembersExtra   []string `json:"members_extra"`
		} `json:"zones"`
		PolicyTemplates []struct {
			ID                 string   `json:"id"`
			Label              string   `json:"label"`
			AllowedZones       []string `json:"allowed_zones"`
			AllowedZonesISO    []string `json:"allowed_zones_iso"`
			BlockedZones       []string `json:"blocked_zones"`
			TransitIsViolation bool     `json:"transit_counts_as_transfer"`
		} `json:"policy_templates"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return 0, err
	}
	var expand func(name string, depth int) []string
	expand = func(name string, depth int) []string {
		z, ok := doc.Zones[name]
		if !ok || depth > 5 {
			return nil
		}
		out := append([]string{}, z.Members...)
		if z.MembersExtends != "" {
			out = append(out, expand(z.MembersExtends, depth+1)...)
		}
		return append(out, z.MembersExtra...)
	}
	n := 0
	for _, t := range doc.PolicyTemplates {
		p := Policy{ID: t.ID, Label: t.Label, Allowed: map[string]bool{}, Restricted: map[string]bool{}, TransitIsViolation: t.TransitIsViolation}
		for _, z := range t.AllowedZones {
			for _, c := range expand(z, 0) {
				p.Allowed[c] = true
			}
		}
		for _, c := range t.AllowedZonesISO {
			p.Allowed[c] = true
		}
		for _, z := range t.BlockedZones {
			for _, c := range expand(z, 0) {
				p.Restricted[c] = true
			}
		}
		r.AddPolicy(p)
		n++
	}
	return n, nil
}

var awsRegions = map[string]regionLoc{
	"us-east-1": {"Ashburn", "US", 39.0438, -77.4874}, "us-east-2": {"Columbus", "US", 39.9612, -82.9988},
	"us-west-1": {"San Jose", "US", 37.3382, -121.8863}, "us-west-2": {"Boardman", "US", 45.8399, -119.7006},
	"ca-central-1": {"Montreal", "CA", 45.5017, -73.5673}, "ca-west-1": {"Calgary", "CA", 51.0447, -114.0719},
	"mx-central-1": {"Queretaro", "MX", 20.5888, -100.3899}, "sa-east-1": {"Sao Paulo", "BR", -23.5505, -46.6333},
	"eu-west-1": {"Dublin", "IE", 53.3498, -6.2603}, "eu-west-2": {"London", "GB", 51.5074, -0.1278},
	"eu-west-3": {"Paris", "FR", 48.8566, 2.3522}, "eu-central-1": {"Frankfurt", "DE", 50.1109, 8.6821},
	"eu-central-2": {"Zurich", "CH", 47.3769, 8.5417}, "eu-north-1": {"Stockholm", "SE", 59.3293, 18.0686},
	"eu-south-1": {"Milan", "IT", 45.4642, 9.19}, "eu-south-2": {"Aragon", "ES", 41.6488, -0.8891},
	"il-central-1": {"Tel Aviv", "IL", 32.0853, 34.7818}, "me-south-1": {"Manama", "BH", 26.2285, 50.586},
	"me-central-1": {"Abu Dhabi", "AE", 24.4539, 54.3773}, "af-south-1": {"Cape Town", "ZA", -33.9249, 18.4241},
	"ap-south-1": {"Mumbai", "IN", 19.076, 72.8777}, "ap-south-2": {"Hyderabad", "IN", 17.385, 78.4867},
	"ap-southeast-1": {"Singapore", "SG", 1.3521, 103.8198}, "ap-southeast-2": {"Sydney", "AU", -33.8688, 151.2093},
	"ap-southeast-3": {"Jakarta", "ID", -6.2088, 106.8456}, "ap-southeast-4": {"Melbourne", "AU", -37.8136, 144.9631},
	"ap-southeast-5": {"Kuala Lumpur", "MY", 3.139, 101.6869}, "ap-southeast-7": {"Bangkok", "TH", 13.7563, 100.5018},
	"ap-northeast-1": {"Tokyo", "JP", 35.6762, 139.6503}, "ap-northeast-2": {"Seoul", "KR", 37.5665, 126.978},
	"ap-northeast-3": {"Osaka", "JP", 34.6937, 135.5023}, "ap-east-1": {"Hong Kong", "HK", 22.3193, 114.1694},
	"cn-north-1": {"Beijing", "CN", 39.9042, 116.4074}, "cn-northwest-1": {"Zhongwei", "CN", 37.5149, 105.1968},
}

var gcpRegions = map[string]regionLoc{
	"us-central1": {"Council Bluffs", "US", 41.2619, -95.8608}, "us-east1": {"Moncks Corner", "US", 33.1960, -80.0131},
	"us-east4": {"Ashburn", "US", 39.0438, -77.4874}, "us-east5": {"Columbus", "US", 39.9612, -82.9988},
	"us-west1": {"The Dalles", "US", 45.5946, -121.1787}, "us-west2": {"Los Angeles", "US", 34.0522, -118.2437},
	"us-west3": {"Salt Lake City", "US", 40.7608, -111.891}, "us-west4": {"Las Vegas", "US", 36.1699, -115.1398},
	"us-south1": {"Dallas", "US", 32.7767, -96.797}, "northamerica-northeast1": {"Montreal", "CA", 45.5017, -73.5673},
	"northamerica-northeast2": {"Toronto", "CA", 43.6532, -79.3832}, "southamerica-east1": {"Sao Paulo", "BR", -23.5505, -46.6333},
	"southamerica-west1": {"Santiago", "CL", -33.4489, -70.6693}, "europe-west1": {"St. Ghislain", "BE", 50.4710, 3.8200},
	"europe-west2": {"London", "GB", 51.5074, -0.1278}, "europe-west3": {"Frankfurt", "DE", 50.1109, 8.6821},
	"europe-west4": {"Eemshaven", "NL", 53.4386, 6.8355}, "europe-west6": {"Zurich", "CH", 47.3769, 8.5417},
	"europe-west8": {"Milan", "IT", 45.4642, 9.19}, "europe-west9": {"Paris", "FR", 48.8566, 2.3522},
	"europe-west10": {"Berlin", "DE", 52.52, 13.405}, "europe-west12": {"Turin", "IT", 45.0703, 7.6869},
	"europe-north1": {"Hamina", "FI", 60.5693, 27.1878}, "europe-north2": {"Stockholm", "SE", 59.3293, 18.0686},
	"europe-central2": {"Warsaw", "PL", 52.2297, 21.0122}, "europe-southwest1": {"Madrid", "ES", 40.4168, -3.7038},
	"asia-south1": {"Mumbai", "IN", 19.076, 72.8777}, "asia-south2": {"Delhi", "IN", 28.7041, 77.1025},
	"asia-southeast1": {"Singapore", "SG", 1.3521, 103.8198}, "asia-southeast2": {"Jakarta", "ID", -6.2088, 106.8456},
	"asia-east1": {"Changhua", "TW", 24.0518, 120.5161}, "asia-east2": {"Hong Kong", "HK", 22.3193, 114.1694},
	"asia-northeast1": {"Tokyo", "JP", 35.6762, 139.6503}, "asia-northeast2": {"Osaka", "JP", 34.6937, 135.5023},
	"asia-northeast3": {"Seoul", "KR", 37.5665, 126.978}, "australia-southeast1": {"Sydney", "AU", -33.8688, 151.2093},
	"australia-southeast2": {"Melbourne", "AU", -37.8136, 144.9631}, "me-west1": {"Tel Aviv", "IL", 32.0853, 34.7818},
	"me-central1": {"Doha", "QA", 25.2854, 51.531}, "me-central2": {"Dammam", "SA", 26.4207, 50.0888},
	"africa-south1": {"Johannesburg", "ZA", -26.2041, 28.0473},
}
