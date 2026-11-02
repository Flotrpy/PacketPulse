package store

import (
	"net/netip"
	"strings"
)

type asnRangeSeed struct {
	start, end string
	asn        int
	cc, name   string
}

// NewSeededReference returns a MemoryReference preloaded with real-world
// fixtures: enough to enrich traces offline. LoadDataDir layers the full
// research datasets on top when they are available.
func NewSeededReference() *MemoryReference {
	r := NewMemoryReference()
	var t rangeTable
	for _, s := range seedASNRanges {
		t.starts = append(t.starts, netip.MustParseAddr(s.start))
		t.ends = append(t.ends, netip.MustParseAddr(s.end))
		t.vals = append(t.vals, ASNInfo{ASN: s.asn, Name: s.name, RegistryCountry: s.cc})
	}
	r.ReplaceASNRanges(t)

	for _, c := range seedCountries {
		r.UpsertCountry(c)
	}
	for _, c := range seedCities {
		r.AddCity(c)
	}
	for _, c := range seedCables {
		r.AddCable(c)
	}
	for _, c := range seedCarriers {
		r.AddCarrier(c)
	}
	for asn, p := range seedClouds {
		r.AddCloud(asn, p)
	}
	for _, g := range seedGeo {
		r.AddGeoPrefix(g)
	}
	for _, p := range seedPolicies() {
		r.AddPolicy(p)
	}
	return r
}

// Continents follow Natural Earth's CONTINENT field. Island = long-haul
// traffic to other countries must use subsea cable (KR is included because
// its only land border is closed).
var seedCountries = []Country{
	{"US", "United States", "North America", 39.8, -98.6, false},
	{"CA", "Canada", "North America", 56.1, -106.3, false},
	{"MX", "Mexico", "North America", 23.6, -102.6, false},
	{"CU", "Cuba", "North America", 21.5, -79.0, true},
	{"BR", "Brazil", "South America", -10.3, -53.1, false},
	{"AR", "Argentina", "South America", -38.4, -63.6, false},
	{"CL", "Chile", "South America", -35.7, -71.5, false},
	{"CO", "Colombia", "South America", 4.6, -74.3, false},
	{"PE", "Peru", "South America", -9.2, -75.0, false},
	{"VE", "Venezuela", "South America", 6.4, -66.6, false},
	{"GB", "United Kingdom", "Europe", 54.0, -2.0, true},
	{"IE", "Ireland", "Europe", 53.4, -8.2, true},
	{"IS", "Iceland", "Europe", 64.9, -19.0, true},
	{"FR", "France", "Europe", 46.6, 2.4, false},
	{"DE", "Germany", "Europe", 51.2, 10.4, false},
	{"NL", "Netherlands", "Europe", 52.1, 5.3, false},
	{"BE", "Belgium", "Europe", 50.5, 4.5, false},
	{"LU", "Luxembourg", "Europe", 49.8, 6.1, false},
	{"ES", "Spain", "Europe", 40.4, -3.7, false},
	{"PT", "Portugal", "Europe", 39.4, -8.2, false},
	{"IT", "Italy", "Europe", 42.8, 12.6, false},
	{"MT", "Malta", "Europe", 35.9, 14.4, true},
	{"CH", "Switzerland", "Europe", 46.8, 8.2, false},
	{"AT", "Austria", "Europe", 47.5, 14.6, false},
	{"SE", "Sweden", "Europe", 60.1, 18.6, false},
	{"NO", "Norway", "Europe", 60.5, 8.5, false},
	{"FI", "Finland", "Europe", 61.9, 25.7, false},
	{"DK", "Denmark", "Europe", 56.3, 9.5, false},
	{"PL", "Poland", "Europe", 51.9, 19.1, false},
	{"CZ", "Czechia", "Europe", 49.8, 15.5, false},
	{"HU", "Hungary", "Europe", 47.2, 19.5, false},
	{"RO", "Romania", "Europe", 45.9, 25.0, false},
	{"BG", "Bulgaria", "Europe", 42.7, 25.5, false},
	{"GR", "Greece", "Europe", 39.1, 21.8, false},
	{"UA", "Ukraine", "Europe", 48.4, 31.2, false},
	{"BY", "Belarus", "Europe", 53.7, 27.9, false},
	{"RU", "Russia", "Europe", 61.5, 105.3, false},
	{"CY", "Cyprus", "Asia", 35.1, 33.4, true},
	{"TR", "Turkey", "Asia", 39.0, 35.2, false},
	{"IL", "Israel", "Asia", 31.0, 34.9, false},
	{"SA", "Saudi Arabia", "Asia", 23.9, 45.1, false},
	{"AE", "United Arab Emirates", "Asia", 23.4, 53.8, false},
	{"QA", "Qatar", "Asia", 25.4, 51.2, false},
	{"BH", "Bahrain", "Asia", 26.0, 50.6, false},
	{"OM", "Oman", "Asia", 21.5, 55.9, false},
	{"YE", "Yemen", "Asia", 15.6, 48.5, false},
	{"IR", "Iran", "Asia", 32.4, 53.7, false},
	{"KZ", "Kazakhstan", "Asia", 48.0, 66.9, false},
	{"PK", "Pakistan", "Asia", 30.4, 69.3, false},
	{"IN", "India", "Asia", 22.0, 79.0, false},
	{"LK", "Sri Lanka", "Asia", 7.9, 80.8, true},
	{"BD", "Bangladesh", "Asia", 23.7, 90.4, false},
	{"SG", "Singapore", "Asia", 1.35, 103.8, false},
	{"MY", "Malaysia", "Asia", 4.2, 102.0, false},
	{"ID", "Indonesia", "Asia", -2.5, 118.0, true},
	{"TH", "Thailand", "Asia", 15.9, 101.0, false},
	{"VN", "Vietnam", "Asia", 14.1, 108.3, false},
	{"PH", "Philippines", "Asia", 12.9, 121.8, true},
	{"HK", "Hong Kong", "Asia", 22.3, 114.2, false},
	{"MO", "Macao", "Asia", 22.2, 113.5, false},
	{"CN", "China", "Asia", 35.9, 104.2, false},
	{"TW", "Taiwan", "Asia", 23.7, 121.0, true},
	{"JP", "Japan", "Asia", 36.2, 138.3, true},
	{"KR", "South Korea", "Asia", 35.9, 127.8, true},
	{"KP", "North Korea", "Asia", 40.3, 127.5, false},
	{"AU", "Australia", "Oceania", -25.3, 133.8, true},
	{"NZ", "New Zealand", "Oceania", -40.9, 174.9, true},
	{"EG", "Egypt", "Africa", 26.8, 30.8, false},
	{"DJ", "Djibouti", "Africa", 11.8, 42.6, false},
	{"KE", "Kenya", "Africa", -0.02, 37.9, false},
	{"NG", "Nigeria", "Africa", 9.1, 8.7, false},
	{"ZA", "South Africa", "Africa", -30.6, 22.9, false},
	{"MA", "Morocco", "Africa", 31.8, -7.1, false},
}

func city(name, cc string, lat, lon float64, tokens ...string) City {
	t := append([]string{strings.ReplaceAll(strings.ToLower(name), " ", "")}, tokens...)
	return City{Name: name, CountryISO2: cc, Lat: lat, Lon: lon, Tokens: t}
}

// seedCities is the router-hostname gazetteer: city names, IATA codes, CLLI-6
// codes and common carrier abbreviations. Ambiguous short tokens (e.g. "tor",
// which usually means top-of-rack, or "los") are deliberately excluded.
var seedCities = []City{
	city("London", "GB", 51.5074, -0.1278, "lon", "ldn", "lhr", "londen"),
	city("Manchester", "GB", 53.4808, -2.2426, "man", "mnc"),
	city("Dublin", "IE", 53.3498, -6.2603, "dub"),
	city("Paris", "FR", 48.8566, 2.3522, "par", "cdg", "prs"),
	city("Marseille", "FR", 43.2965, 5.3698, "mrs", "marseilles"),
	city("Frankfurt", "DE", 50.1109, 8.6821, "fra", "ffm", "frf", "frnkge"),
	city("Berlin", "DE", 52.5200, 13.4050, "ber"),
	city("Munich", "DE", 48.1351, 11.5820, "muc", "muenchen"),
	city("Dusseldorf", "DE", 51.2277, 6.7735, "dus", "duesseldorf"),
	city("Hamburg", "DE", 53.5511, 9.9937, "ham"),
	city("Amsterdam", "NL", 52.3676, 4.9041, "ams", "amstnl"),
	city("Brussels", "BE", 50.8503, 4.3517, "bru"),
	city("Madrid", "ES", 40.4168, -3.7038, "mad"),
	city("Barcelona", "ES", 41.3874, 2.1686, "bcn"),
	city("Bilbao", "ES", 43.2630, -2.9350),
	city("Lisbon", "PT", 38.7223, -9.1393, "lis", "lisboa"),
	city("Milan", "IT", 45.4642, 9.1900, "mil", "mxp", "milano"),
	city("Rome", "IT", 41.9028, 12.4964, "rom", "roma"),
	city("Palermo", "IT", 38.1157, 13.3615, "pmo"),
	city("Zurich", "CH", 47.3769, 8.5417, "zrh"),
	city("Geneva", "CH", 46.2044, 6.1432, "gva"),
	city("Vienna", "AT", 48.2082, 16.3738, "vie", "wien"),
	city("Stockholm", "SE", 59.3293, 18.0686, "sto", "arn", "stk", "sthlm"),
	city("Oslo", "NO", 59.9139, 10.7522, "osl"),
	city("Copenhagen", "DK", 55.6761, 12.5683, "cph", "kbh"),
	city("Helsinki", "FI", 60.1699, 24.9384, "hel"),
	city("Warsaw", "PL", 52.2297, 21.0122, "waw", "warszawa"),
	city("Prague", "CZ", 50.0755, 14.4378, "prg", "praha"),
	city("Budapest", "HU", 47.4979, 19.0402, "bud"),
	city("Bucharest", "RO", 44.4268, 26.1025, "buh", "otp"),
	city("Sofia", "BG", 42.6977, 23.3219, "sof"),
	city("Athens", "GR", 37.9838, 23.7275, "ath"),
	city("Istanbul", "TR", 41.0082, 28.9784, "ist"),
	city("Moscow", "RU", 55.7558, 37.6173, "mow", "msk", "moskva"),
	city("Saint Petersburg", "RU", 59.9311, 30.3609, "spb", "led"),
	city("Kyiv", "UA", 50.4501, 30.5234, "kiev", "iev"),
	city("Tel Aviv", "IL", 32.0853, 34.7818, "tlv"),
	city("Cairo", "EG", 30.0444, 31.2357, "cai"),
	city("Alexandria", "EG", 31.2001, 29.9187),
	city("Jeddah", "SA", 21.4858, 39.1925, "jed"),
	city("Riyadh", "SA", 24.7136, 46.6753, "ruh"),
	city("Dubai", "AE", 25.2048, 55.2708, "dxb"),
	city("Fujairah", "AE", 25.1288, 56.3265, "fjr"),
	city("Doha", "QA", 25.2854, 51.5310, "doh"),
	city("Muscat", "OM", 23.5880, 58.3829, "mct"),
	city("Karachi", "PK", 24.8607, 67.0011, "khi"),
	city("Mumbai", "IN", 19.0760, 72.8777, "bom", "bombay"),
	city("Chennai", "IN", 13.0827, 80.2707, "maa", "madras"),
	city("Delhi", "IN", 28.7041, 77.1025, "del", "newdelhi"),
	city("Bangalore", "IN", 12.9716, 77.5946, "blr", "bengaluru"),
	city("Colombo", "LK", 6.9271, 79.8612, "cmb"),
	city("Singapore", "SG", 1.3521, 103.8198, "sin", "sgp"),
	city("Kuala Lumpur", "MY", 3.1390, 101.6869, "kul"),
	city("Jakarta", "ID", -6.2088, 106.8456, "jkt", "cgk"),
	city("Bangkok", "TH", 13.7563, 100.5018, "bkk"),
	city("Ho Chi Minh City", "VN", 10.8231, 106.6297, "sgn", "hcm", "hochiminh"),
	city("Hanoi", "VN", 21.0278, 105.8342, "han"),
	city("Manila", "PH", 14.5995, 120.9842, "mnl"),
	city("Hong Kong", "HK", 22.3193, 114.1694, "hkg"),
	city("Shanghai", "CN", 31.2304, 121.4737, "sha", "pvg"),
	city("Beijing", "CN", 39.9042, 116.4074, "bjs", "pek"),
	city("Guangzhou", "CN", 23.1291, 113.2644),
	city("Taipei", "TW", 25.0330, 121.5654, "tpe"),
	city("Tokyo", "JP", 35.6762, 139.6503, "tyo", "nrt", "hnd", "tokyjp"),
	city("Osaka", "JP", 34.6937, 135.5023, "osa", "kix"),
	city("Seoul", "KR", 37.5665, 126.9780, "sel", "icn"),
	city("Sydney", "AU", -33.8688, 151.2093, "syd"),
	city("Melbourne", "AU", -37.8136, 144.9631, "mel"),
	city("Perth", "AU", -31.9505, 115.8605),
	city("Auckland", "NZ", -36.8485, 174.7633, "akl"),
	city("Johannesburg", "ZA", -26.2041, 28.0473, "jnb", "jhb"),
	city("Cape Town", "ZA", -33.9249, 18.4241, "cpt"),
	city("Lagos", "NG", 6.5244, 3.3792),
	city("Nairobi", "KE", -1.2921, 36.8219, "nbo"),
	city("Mombasa", "KE", -4.0435, 39.6682, "mba"),
	city("Djibouti", "DJ", 11.5721, 43.1456, "jib"),
	city("New York", "US", 40.7128, -74.0060, "nyc", "jfk", "nycmny", "nyk"),
	city("Ashburn", "US", 39.0438, -77.4874, "iad", "asbnva", "ash"),
	city("Washington", "US", 38.9072, -77.0369, "wdc", "dca", "wash"),
	city("Chicago", "US", 41.8781, -87.6298, "chi", "ord", "chcgil"),
	city("Dallas", "US", 32.7767, -96.7970, "dfw", "dal", "dllstx"),
	city("Atlanta", "US", 33.7490, -84.3880, "atl", "atlnga"),
	city("Miami", "US", 25.7617, -80.1918, "mia", "miamfl"),
	city("Los Angeles", "US", 34.0522, -118.2437, "lax", "lsanca"),
	city("San Jose", "US", 37.3382, -121.8863, "sjc", "snjsca"),
	city("Seattle", "US", 47.6062, -122.3321, "sea", "sttlwa"),
	city("Denver", "US", 39.7392, -104.9903, "den", "dnvrco"),
	city("Toronto", "CA", 43.6532, -79.3832, "yyz"),
	city("Montreal", "CA", 45.5017, -73.5673, "yul", "mtl"),
	city("Vancouver", "CA", 49.2827, -123.1207, "yvr"),
	city("Mexico City", "MX", 19.4326, -99.1332, "mex"),
	city("Sao Paulo", "BR", -23.5505, -46.6333, "gru", "spo", "saopaulo"),
	city("Fortaleza", "BR", -3.7319, -38.5267),
	city("Rio de Janeiro", "BR", -22.9068, -43.1729, "gig"),
	city("Buenos Aires", "AR", -34.6037, -58.3816, "eze", "bue"),
	city("Santiago", "CL", -33.4489, -70.6693, "scl"),
	city("Bogota", "CO", 4.7110, -74.0721, "bog"),
	city("Lima", "PE", -12.0464, -77.0428, "lim"),
}

// seedCarriers are mobile network operators whose ASNs carry handset traffic
// (ASN names verified against iptoasn 2026-11-02).
var seedCarriers = []Carrier{
	{55836, "Reliance Jio Infocomm", "IN", "405", []string{"840", "854", "855", "856", "857", "858", "859", "860", "861", "862", "863", "864", "865", "866", "867", "868", "869", "870", "871", "872", "873", "874"}},
	{45609, "Bharti Airtel (GPRS/mobile)", "IN", "404", []string{"10", "45", "49", "90", "92", "93", "94", "95", "96", "97", "98"}},
	{38266, "Vodafone Idea", "IN", "404", []string{"20", "11", "05", "27", "43", "46", "60", "84", "86"}},
	{21928, "T-Mobile USA", "US", "310", []string{"260"}},
	{6167, "Verizon Wireless (Cellco)", "US", "311", []string{"480"}},
	{22394, "Verizon Wireless (Cellco)", "US", "311", []string{"480"}},
	{20057, "AT&T Mobility", "US", "310", []string{"410"}},
	{25135, "Vodafone UK", "GB", "234", []string{"15"}},
	{16135, "Turkcell", "TR", "286", []string{"01"}},
	{23693, "Telkomsel", "ID", "510", []string{"10"}},
	{24203, "XL Axiata", "ID", "510", []string{"11"}},
	{132199, "Globe Telecom (mobile)", "PH", "515", []string{"02"}},
	{9808, "China Mobile (Guangdong)", "CN", "460", []string{"00", "02", "07"}},
}

var seedClouds = map[int]string{
	16509: "aws", 14618: "aws",
	15169: "gcp", 396982: "gcp",
	8075: "azure", 8068: "azure",
	31898: "oracle",
	13335: "cloudflare",
	20940: "akamai", 16625: "akamai",
	54113: "fastly",
}

// seedGeo is a small prefix-geolocation table (stand-in for MaxMind City);
// LoadDataDir adds every AWS/GCP regional prefix.
var seedGeo = []GeoHit{
	{Prefix: netip.MustParsePrefix("18.96.32.0/19"), City: "Frankfurt", CountryISO2: "DE", Lat: 50.1109, Lon: 8.6821, AccuracyKM: 50},
	{Prefix: netip.MustParsePrefix("49.36.0.0/24"), City: "Navi Mumbai", CountryISO2: "IN", Lat: 19.0330, Lon: 73.0297, AccuracyKM: 25},
}

// Compliance zones mirror data/curated/regulations/compliance_zones.json.
var (
	zoneEU           = []string{"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR", "DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE"}
	zoneEEA          = append(append([]string{}, zoneEU...), "IS", "LI", "NO")
	zoneGDPRAdequate = []string{"AD", "AR", "BR", "CA", "FO", "GG", "IL", "IM", "JP", "JE", "NZ", "KR", "CH", "GB", "UY", "US"}
	zoneDOJConcern   = []string{"CN", "HK", "MO", "CU", "IR", "KP", "RU", "VE"}
)

func set(groups ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, g := range groups {
		for _, c := range g {
			m[c] = true
		}
	}
	return m
}

func seedPolicies() []Policy {
	return []Policy{
		{ID: "eu-gdpr-strict", Label: "EU personal data – stay in EEA", Allowed: set(zoneEEA)},
		{ID: "eu-gdpr-adequacy", Label: "EU personal data – EEA + adequate countries", Allowed: set(zoneEEA, zoneGDPRAdequate)},
		{ID: "us-doj-bulk", Label: "US bulk sensitive data – avoid countries of concern", Restricted: set(zoneDOJConcern), TransitIsViolation: true},
		{ID: "in-rbi-payments", Label: "India payment system data – store only in India", Allowed: set([]string{"IN"})},
		{ID: "ru-242fz", Label: "Russian citizens' personal data – primary database in Russia", Allowed: set([]string{"RU"})},
		{ID: "cn-important-data", Label: "China important data – stay in mainland China", Allowed: set([]string{"CN"}), TransitIsViolation: true},
	}
}
