package compliance

// Built-in definitions, current as of November 2026. Admins can replace any
// of them at runtime through the API.

var (
	euMembers = []string{"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR", "DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE"}
	// Schengen (29): EU minus IE and CY, plus IS, LI, NO, CH. BG and RO joined fully on 1 Jan 2025.
	schengen = []string{"AT", "BE", "BG", "HR", "CZ", "DK", "EE", "FI", "FR", "DE", "GR", "HU", "IT", "LV", "LT", "LU", "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE", "IS", "LI", "NO", "CH"}
	// Countries with an EU Commission adequacy decision. The US is handled
	// separately: its adequacy covers only EU-US Data Privacy Framework participants.
	gdprAdequate = []string{"AD", "AR", "BR", "CA", "FO", "GG", "IL", "IM", "JP", "JE", "NZ", "KR", "CH", "GB", "UY"}
	dojConcern   = []string{"CN", "HK", "MO", "CU", "IR", "KP", "RU", "VE"}
	eeaMembers   = append(append([]string{}, euMembers...), "IS", "LI", "NO")
)

// DefaultRegions returns the built-in regulatory boundaries.
func DefaultRegions() []Region {
	return []Region{
		{ID: "eu", Name: "European Union", Countries: euMembers, Source: "https://european-union.europa.eu/principles-countries-history/eu-countries_en"},
		{ID: "eea", Name: "European Economic Area (GDPR territory)", Description: "EU + Iceland, Liechtenstein, Norway. GDPR's territorial scope; Chapter V governs transfers out of it.", Countries: eeaMembers, Source: "https://www.efta.int/eea"},
		{ID: "schengen", Name: "Schengen Area", Description: "Border-control area. Not the GDPR boundary: includes CH, excludes IE and CY.", Countries: schengen, Source: "https://home-affairs.ec.europa.eu/policies/schengen-borders-and-visa/schengen-area_en"},
		{ID: "gdpr-adequate", Name: "EU adequacy decisions (Art. 45 GDPR)", Description: "Countries with an adequacy decision (CA: PIPEDA scope). The US (DPF participants only) is region 'us'.", Countries: gdprAdequate, Source: "https://commission.europa.eu/law/law-topic/data-protection/international-dimension-data-protection/adequacy-decisions_en"},
		{ID: "us-ca", Name: "California (state line)", Subdivisions: []string{"US-CA"}, Source: "https://cppa.ca.gov/regulations/"},
		{ID: "us", Name: "United States", Countries: []string{"US"}},
		{ID: "in", Name: "India", Countries: []string{"IN"}},
		{ID: "ru", Name: "Russian Federation", Countries: []string{"RU"}},
		{ID: "cn-mainland", Name: "China (mainland)", Description: "Hong Kong and Macao are separate jurisdictions for PIPL cross-border purposes.", Countries: []string{"CN"}},
		{ID: "us-doj-concern", Name: "US DOJ countries of concern (28 CFR 202.601)", Countries: dojConcern, Source: "https://www.justice.gov/nsd/data-security"},
	}
}

// DefaultProfiles returns the built-in rule sets.
func DefaultProfiles() []Profile {
	gdprLaw := Law{
		Code: "GDPR", Title: "General Data Protection Regulation",
		Citation: "Regulation (EU) 2016/679, Chapter V (Art. 44-49): transfers of personal data to third countries",
		Articles: []string{"Art. 44 (general principle for transfers)", "Art. 45 (adequacy)", "Art. 46 (appropriate safeguards)"},
		URL:      "https://eur-lex.europa.eu/eli/reg/2016/679/oj",
	}
	return []Profile{
		{
			ID: "gdpr-ch5-strict", Name: "GDPR Chapter V: sensitive EU citizen data stays in the EEA", Enabled: true,
			Description:     "Any route point outside the EEA is treated as an unauthorised transfer. Strict: adequacy decisions are not applied (see gdpr-ch5-adequacy).",
			Classifications: []string{"sensitive_eu_citizen_data", "eu_special_category_data"},
			Mode:            "allow_list", Regions: []string{"eea"}, Scope: "all_hops", Severity: "critical", MinLocationConfidence: 0.5,
			Rule: "Personal data may leave the EEA only under an adequacy decision (Art. 45), appropriate safeguards (Art. 46) or a derogation (Art. 49). This profile allows none in transit.",
			Law:  gdprLaw,
			Mitigation: Mitigation{
				Action:   "reroute",
				Template: "Reroute via EU-isolated {{proxy}} ({{proxy_region}}, {{proxy_city}}) and pin the flow to EEA-only transit; hold transfers until a re-trace shows no hop in {{territory}}.",
				Alternatives: []string{
					"Terminate TLS inside the EEA and forward over a private interconnect that peers only at EEA IXPs.",
					"If the recipient must be outside the EEA, document an Art. 46 transfer (SCCs + transfer impact assessment) before resuming.",
				},
			},
		},
		{
			ID: "gdpr-ch5-adequacy", Name: "GDPR Chapter V: EU personal data, EEA + adequate countries", Enabled: true,
			Description:     "Allows the EEA and countries with an EU adequacy decision; US hops are downgraded because adequacy only covers DPF-certified recipients.",
			Classifications: []string{"eu_personal_data"},
			Mode:            "allow_list", Regions: []string{"eea"},
			Exceptions: []Exception{
				{Region: "gdpr-adequate", Effect: "allow", Reason: "Art. 45 adequacy decision"},
				{Region: "us", Effect: "downgrade", Reason: "EU-US Data Privacy Framework covers only certified recipients: verify the operator's DPF listing"},
			},
			Scope: "all_hops", Severity: "high", MinLocationConfidence: 0.5,
			Rule: "Transfers outside the EEA need an adequacy decision, Art. 46 safeguards or an Art. 49 derogation.",
			Law:  gdprLaw,
			Mitigation: Mitigation{Action: "reroute",
				Template: "Reroute via {{proxy}} ({{proxy_region}}) so the path stays inside the EEA or adequate countries; {{territory}} has no adequacy decision."},
		},
		{
			ID: "ccpa-california-residency", Name: "CCPA/CPRA: California consumer data leaving California", Enabled: true,
			Description:     "Informational. CCPA does not require data to stay in California; data leaving the state means a service provider or contractor outside it is handling it, which needs the contract terms CCPA requires.",
			Classifications: []string{"california_consumer_data"},
			Mode:            "allow_list", Regions: []string{"us-ca"}, Scope: "all_hops", Severity: "info", MinLocationConfidence: 0.5,
			Rule: "Recipients outside California must be bound as service providers/contractors (Cal. Civ. Code §1798.100(d), §1798.140(ag), (j)).",
			Law: Law{Code: "CCPA/CPRA", Title: "California Consumer Privacy Act as amended by the CPRA", Citation: "Cal. Civ. Code §1798.100 et seq.; 11 CCR §7050-7053 (service provider contracts)",
				URL: "https://leginfo.legislature.ca.gov/faces/codes_displayText.xhtml?division=3.&part=4.&lawCode=CIV&title=1.81.5"},
			Mitigation: Mitigation{Action: "contract",
				Template: "Confirm the operator handling traffic in {{territory}} is under a CCPA service-provider/contractor agreement; if not, route via {{proxy}} ({{proxy_region}})."},
		},
		{
			ID: "in-rbi-payment-localisation", Name: "RBI: payment system data stored only in India", Enabled: true,
			Classifications: []string{"payment_system_data"},
			Mode:            "allow_list", Regions: []string{"in"}, Scope: "destination_only", Severity: "high", MinLocationConfidence: 0.5,
			Rule: "Entire payment data must be stored only in India; foreign processing must be deleted abroad and returned within one business day / 24 h.",
			Law:  Law{Code: "RBI", Title: "Storage of Payment System Data", Citation: "RBI/2017-18/153, DPSS.CO.OD No 2785/06.08.005/2017-18 (6 Apr 2018)", URL: "https://www.rbi.org.in/Scripts/NotificationUser.aspx?Id=11244"},
			Mitigation: Mitigation{Action: "reroute",
				Template: "Terminate payment flows in India: point the client at {{proxy}} ({{proxy_region}}) instead of the endpoint in {{territory}}."},
		},
		{
			ID: "ru-242fz-localisation", Name: "Russia 242-FZ: primary database of Russian citizens' data in Russia", Enabled: true,
			Classifications: []string{"ru_citizen_personal_data"},
			Mode:            "allow_list", Regions: []string{"ru"}, Scope: "destination_only", Severity: "high", MinLocationConfidence: 0.5,
			Rule: "Recording, systematisation and storage of Russian citizens' personal data must use databases located in Russia (152-FZ Art. 18(5)).",
			Law:  Law{Code: "242-FZ", Title: "Federal Law No. 242-FZ (data localisation)", Citation: "Federal Law No. 152-FZ 'On Personal Data', Art. 18(5), as amended by No. 242-FZ"},
			Mitigation: Mitigation{Action: "reroute",
				Template: "Write the primary record in Russia first via {{proxy}} ({{proxy_region}}); the destination in {{territory}} may only receive a secondary copy after Roskomnadzor cross-border notification."},
		},
		{
			ID: "cn-pipl-important-data", Name: "China DSL/PIPL: important data stays in mainland China", Enabled: true,
			Classifications: []string{"cn_important_data"},
			Mode:            "allow_list", Regions: []string{"cn-mainland"}, Scope: "all_hops", Severity: "critical", MinLocationConfidence: 0.5,
			Rule: "Important data and CIIO personal information must be stored in mainland China; export requires a CAC security assessment.",
			Law:  Law{Code: "DSL/PIPL", Title: "Data Security Law; Personal Information Protection Law", Citation: "DSL Art. 31; PIPL Art. 38-40; Provisions on Promoting and Regulating Cross-Border Data Flows (2024)"},
			Mitigation: Mitigation{Action: "block",
				Template: "Block the flow at the border egress; keep processing on {{proxy}} ({{proxy_region}}) unless a CAC security assessment covers transfer to {{territory}}."},
		},
		{
			ID: "us-doj-bulk-sensitive", Name: "US DOJ bulk sensitive data: no access by countries of concern", Enabled: true,
			Classifications: []string{"us_bulk_sensitive_data", "us_government_related_data"},
			Mode:            "deny_list", Regions: []string{"us-doj-concern"}, Scope: "all_hops", Severity: "critical", MinLocationConfidence: 0.4,
			Rule: "Covered data transactions that give a country of concern or covered person access to bulk U.S. sensitive personal data are prohibited or restricted.",
			Law:  Law{Code: "28 CFR 202", Title: "DOJ Data Security Program (EO 14117)", Citation: "28 CFR Part 202, §202.301-202.401", URL: "https://www.ecfr.gov/current/title-28/chapter-I/part-202"},
			Mitigation: Mitigation{Action: "block",
				Template: "Block routes through {{territory}} and re-originate via {{proxy}} ({{proxy_region}}); log the attempted transaction for the DOJ compliance programme."},
		},
	}
}

// DefaultProxies are cloud regions usable as in-territory relays.
func DefaultProxies() []Proxy {
	aws := func(id, region, city string, lat, lon float64, notes string) Proxy {
		return Proxy{ID: id, Name: "AWS " + region + " proxy", Provider: "AWS", Region: region, City: city, Lat: lat, Lon: lon, Notes: notes}
	}
	return []Proxy{
		aws("aws-eu-central-1", "eu-central-1", "Frankfurt", 50.1109, 8.6821, "EU region; pair with EEA-only peering"),
		aws("aws-eu-west-1", "eu-west-1", "Dublin", 53.3498, -6.2603, "EU region"),
		aws("aws-eu-west-3", "eu-west-3", "Paris", 48.8566, 2.3522, "EU region"),
		aws("aws-eu-south-1", "eu-south-1", "Milan", 45.4642, 9.19, "EU region"),
		aws("aws-eu-south-2", "eu-south-2", "Aragon", 41.6488, -0.8891, "EU region"),
		aws("aws-eu-north-1", "eu-north-1", "Stockholm", 59.3293, 18.0686, "EU region"),
		aws("aws-us-west-1", "us-west-1", "San Jose", 37.3382, -121.8863, "California"),
		aws("aws-ap-south-1", "ap-south-1", "Mumbai", 19.076, 72.8777, "India"),
		aws("aws-ap-south-2", "ap-south-2", "Hyderabad", 17.385, 78.4867, "India"),
		aws("aws-cn-north-1", "cn-north-1", "Beijing", 39.9042, 116.4074, "Operated by Sinnet; separate China partition"),
		{ID: "yandex-ru-central1", Name: "Yandex Cloud ru-central1 relay", Provider: "Yandex Cloud", Region: "ru-central1", City: "Moscow", Lat: 55.7558, Lon: 37.6173},
		aws("aws-us-east-1", "us-east-1", "Ashburn", 39.0438, -77.4874, "US"),
	}
}
