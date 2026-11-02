// Package model holds the wire types for a PacketJourney. The JSON tags here
// are the contract described by schema/packet_journey.schema.json; a test
// validates marshalled journeys against that schema.
package model

import "time"

const SchemaVersion = "1.0.0"

// Journey is one traceroute to one destination, enriched by the pipeline.
type Journey struct {
	SchemaVersion  string         `json:"schema_version"`
	JourneyID      string         `json:"journey_id"`
	Metadata       Metadata       `json:"metadata"`
	Hops           []Hop          `json:"hops"`
	Infrastructure Infrastructure `json:"infrastructure"`
	Compliance     Compliance     `json:"compliance"`
	Pipeline       PipelineTrace  `json:"pipeline"`
}

// ---------- Packet metadata ----------

type Metadata struct {
	Timestamp          time.Time   `json:"timestamp"`
	CompletedAt        time.Time   `json:"completed_at"`
	Protocol           string      `json:"protocol"` // icmp | udp
	IPVersion          int         `json:"ip_version"`
	AgentID            string      `json:"agent_id"`
	Source             Endpoint    `json:"source"`
	Destination        Endpoint    `json:"destination"`
	Probe              ProbeConfig `json:"probe"`
	ReachedDestination bool        `json:"reached_destination"`
	DurationMS         float64     `json:"duration_ms"`
}

type Endpoint struct {
	IP       string    `json:"ip"`
	Hostname string    `json:"hostname,omitempty"`
	ASN      int       `json:"asn,omitempty"`
	ASOrg    string    `json:"as_org,omitempty"`
	Location *Location `json:"location,omitempty"`
	Cloud    string    `json:"cloud_provider,omitempty"`
	Access   string    `json:"access_type,omitempty"` // wired | wifi | cellular | unknown
}

type ProbeConfig struct {
	FirstTTL     int     `json:"first_ttl"`
	MaxHops      int     `json:"max_hops"`
	ProbesPerHop int     `json:"probes_per_hop"`
	TimeoutMS    float64 `json:"timeout_ms"`
	DstPortBase  int     `json:"dst_port_base,omitempty"`
}

// ---------- Hop hierarchy ----------

type Hop struct {
	HopNumber       int       `json:"hop_number"`
	Responded       bool      `json:"responded"`
	IP              string    `json:"ip,omitempty"`
	AlternateIPs    []string  `json:"alternate_ips,omitempty"`
	Hostname        string    `json:"hostname,omitempty"`
	ASN             int       `json:"asn,omitempty"`
	ASOrg           string    `json:"as_org,omitempty"`
	RegistryCountry string    `json:"registry_country,omitempty"`
	AddressClass    string    `json:"address_class"` // public | private | cgnat | reserved | loopback | link_local | unknown
	Role            string    `json:"role"`
	RTT             RTTStats  `json:"rtt_ms"`
	ProbesSent      int       `json:"probes_sent"`
	ProbesReceived  int       `json:"probes_received"`
	LossPct         float64   `json:"loss_pct"`
	ICMPType        string    `json:"icmp_type,omitempty"`
	MPLSLabels      []int     `json:"mpls_labels,omitempty"`
	Location        *Location `json:"location,omitempty"`
	BorderViolation bool      `json:"border_violation"`
}

type RTTStats struct {
	Samples []float64 `json:"samples"`
	Min     float64   `json:"min"`
	Avg     float64   `json:"avg"`
	Max     float64   `json:"max"`
	StdDev  float64   `json:"stdev"`
	Jitter  float64   `json:"jitter"`
}

type Location struct {
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	City        string  `json:"city,omitempty"`
	CountryISO2 string  `json:"country_iso2"`
	AccuracyKM  float64 `json:"accuracy_km"`
	Method      string  `json:"method"` // rdns_hint | prefix_db | registry_country | inherited | configured
	Confidence  float64 `json:"confidence"`
}

// ---------- Infrastructure layer ----------

type Infrastructure struct {
	Segments     []Segment `json:"segments"`
	MediaSummary []string  `json:"media_summary"`
}

type Segment struct {
	FromHop          int           `json:"from_hop"` // 0 = source host
	ToHop            int           `json:"to_hop"`
	Medium           string        `json:"medium"`
	Confidence       float64       `json:"confidence"`
	DistanceKM       float64       `json:"distance_km"`
	DeltaRTTMS       float64       `json:"delta_rtt_ms"`
	MinFeasibleRTTMS float64       `json:"min_feasible_rtt_ms"`
	Health           string        `json:"health"` // green | amber | red | grey
	Countries        []string      `json:"countries"`
	TerrestrialISP   *Operator     `json:"terrestrial_isp,omitempty"`
	SubseaCable      *SubseaCable  `json:"subsea_cable,omitempty"`
	Cellular         *CellularHaul `json:"cellular_backhaul,omitempty"`
}

type Operator struct {
	ASN  int    `json:"asn"`
	Name string `json:"name"`
}

type SubseaCable struct {
	CableID      string           `json:"cable_id"`
	Name         string           `json:"name"`
	LandingA     string           `json:"landing_a"`
	LandingB     string           `json:"landing_b"`
	Probability  float64          `json:"probability"`
	OwnerMatch   bool             `json:"owner_match"`
	Alternatives []CableCandidate `json:"alternatives"`
}

type CableCandidate struct {
	CableID     string  `json:"cable_id"`
	Name        string  `json:"name"`
	Probability float64 `json:"probability"`
}

type CellularHaul struct {
	Carrier   string `json:"carrier"`
	ASN       int    `json:"asn,omitempty"`
	Country   string `json:"country,omitempty"`
	GatewayIP string `json:"gateway_ip,omitempty"`
	CGNAT     bool   `json:"cgnat_detected"`
}

// ---------- Compliance ----------

type Compliance struct {
	PolicyID           string          `json:"policy_id"`
	Evaluated          bool            `json:"evaluated"`
	AllowedCountries   []string        `json:"allowed_countries"`
	Flags              ComplianceFlags `json:"flags"`
	Verdict            string          `json:"verdict"` // compliant | risk | violation | inconclusive | not_evaluated
	CountriesTraversed []string        `json:"countries_traversed"`
	PossibleTransit    []string        `json:"possible_transit_countries"`
	ViolatingHops      []int           `json:"violating_hops"`
	Alerts             []Alert         `json:"alerts"`
}

type ComplianceFlags struct {
	BoundaryViolation           bool `json:"boundary_violation"`
	LeftAllowedRegion           bool `json:"left_allowed_region"`
	DestinationOutsideRegion    bool `json:"destination_outside_region"`
	TransitedRestrictedCountry  bool `json:"transited_restricted_country"`
	InternationalSubseaCrossing bool `json:"international_subsea_crossing"`
	UnresolvedHopsPresent       bool `json:"unresolved_hops_present"`
}

type Alert struct {
	Severity  string `json:"severity"` // info | warning | critical
	Rule      string `json:"rule"`
	Country   string `json:"country,omitempty"`
	HopNumber int    `json:"hop_number,omitempty"`
	Message   string `json:"message"`
}

// ---------- Pipeline audit ----------

type PipelineTrace struct {
	Stages []StageRun `json:"stages"`
}

type StageRun struct {
	Name       string  `json:"name"`
	DurationMS float64 `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

// Medium values.
const (
	MediumLAN              = "lan"
	MediumCellularBackhaul = "cellular_backhaul"
	MediumMetroFiber       = "metro_fiber"
	MediumTerrestrialFiber = "terrestrial_fiber"
	MediumSubseaCable      = "subsea_cable"
	MediumCloudBackbone    = "cloud_backbone"
	MediumUnknown          = "unknown"
)
