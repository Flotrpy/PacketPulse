package enrich

import (
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/geoip"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/mobile"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/subsea"
)

const InfraMapSchemaVersion = "1.0.0"

// InfraMap is the fully enriched network-to-infrastructure document for one
// trace (schema/infra_map.schema.json).
type InfraMap struct {
	SchemaVersion string         `json:"schema_version"`
	MapID         string         `json:"map_id"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Trace         TraceInfo      `json:"trace"`
	Device        *DeviceInfo    `json:"device"`
	Hops          []HopEnriched  `json:"hops"`
	Links         []LinkEnriched `json:"links"`
	Mobile        *MobileLeg     `json:"mobile"`
	Mapping       []MappingEntry `json:"mapping"`
	Summary       Summary        `json:"summary"`
	Provenance    Provenance     `json:"provenance"`
}

type TraceInfo struct {
	Target             string    `json:"target"`
	Source             string    `json:"source,omitempty"`
	Protocol           string    `json:"protocol"`
	StartedAt          time.Time `json:"started_at"`
	FinishedAt         time.Time `json:"finished_at"`
	ReachedDestination bool      `json:"reached_destination"`
	DestinationTTL     int       `json:"destination_ttl"`
	HopCount           int       `json:"hop_count"`
}

type DeviceInfo struct {
	Location   *Point       `json:"location"`
	Country    string       `json:"country,omitempty"`
	AccessType string       `json:"access_type"`
	MCC        string       `json:"mcc,omitempty"`
	MNC        string       `json:"mnc,omitempty"`
	Cell       *mobile.Cell `json:"serving_cell,omitempty"`
	CellError  string       `json:"serving_cell_error,omitempty"`
}

// Point is a located position with provenance.
type Point struct {
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	AccuracyKM float64 `json:"accuracy_km"`
	Source     string  `json:"source"` // device_gps | opencellid | maxmind | ipinfo | rdns_hint | ...
}

type RTT struct {
	Min     float64   `json:"min"`
	Avg     float64   `json:"avg"`
	Max     float64   `json:"max"`
	Jitter  float64   `json:"jitter"`
	Samples []float64 `json:"samples"`
}

type HopEnriched struct {
	TTL            int           `json:"ttl"`
	Responded      bool          `json:"responded"`
	IP             string        `json:"ip,omitempty"`
	Alternates     []string      `json:"alternate_ips,omitempty"`
	AddressClass   string        `json:"address_class"`
	Hostname       string        `json:"hostname,omitempty"`
	RTT            RTT           `json:"rtt_ms"`
	LossPct        float64       `json:"loss_pct"`
	ASN            int           `json:"asn,omitempty"`
	ASOrg          string        `json:"as_org,omitempty"`
	ISP            string        `json:"isp,omitempty"`
	Geo            *geoip.Record `json:"geo"`
	LocationStatus string        `json:"location_status"` // accepted | rejected_light_speed | rejected_detour | no_data | non_routable | no_reply
	LocationNote   string        `json:"location_note,omitempty"`
	Role           string        `json:"role"`
}

type Endpoint struct {
	TTL   int     `json:"ttl"` // 0 = device
	IP    string  `json:"ip,omitempty"`
	Label string  `json:"label"`
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	ISO2  string  `json:"country_iso2,omitempty"`
}

type Operator struct {
	ASN  int    `json:"asn"`
	Name string `json:"name"`
}

type SubseaResult struct {
	Selected     *subsea.Candidate  `json:"selected"`
	Alternatives []subsea.Candidate `json:"alternatives"`
	Note         string             `json:"note,omitempty"`
}

type LinkEnriched struct {
	Index            int           `json:"index"`
	From             Endpoint      `json:"from"`
	To               Endpoint      `json:"to"`
	ViaUnlocatedTTLs []int         `json:"via_unlocated_ttls,omitempty"`
	DistanceKM       float64       `json:"distance_km"`
	DeltaRTTms       float64       `json:"delta_rtt_ms"`
	MinFeasibleRTTms float64       `json:"min_feasible_rtt_ms"`
	Medium           string        `json:"medium"` // cellular_backhaul | metro_fiber | terrestrial_fiber | subsea_cable
	Confidence       float64       `json:"confidence"`
	Operator         *Operator     `json:"operator,omitempty"`
	Ocean            *gis.Crossing `json:"ocean"`
	Subsea           *SubseaResult `json:"subsea,omitempty"`
	Cellular         *CellularLink `json:"cellular,omitempty"`
}

type CellularLink struct {
	Operator string `json:"operator"`
	ASN      int    `json:"asn"`
	HubID    string `json:"gateway_hub_id,omitempty"`
}

type Evidence struct {
	Signal     string  `json:"signal"`
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
}

type MobileLeg struct {
	Operator       mobile.Operator `json:"operator"`
	Evidence       []Evidence      `json:"evidence"`
	GatewayTTL     int             `json:"gateway_ttl"`
	GatewayIP      string          `json:"gateway_ip"`
	CGNATTTLs      []int           `json:"cgnat_ttls"`
	GatewayHub     *mobile.Hub     `json:"gateway_hub"`
	HubDistanceKM  float64         `json:"hub_distance_km"`
	HubAnchor      string          `json:"hub_anchor"` // serving_cell | device_location | gateway_geoip
	HubsConsidered int             `json:"hubs_considered"`
	LinkIndexes    []int           `json:"link_indexes"`
	Confidence     float64         `json:"confidence"`
}

// MappingEntry pairs one network element (hop or link) with the
// infrastructure it was resolved to. Ordered along the path.
type MappingEntry struct {
	Seq            int            `json:"seq"`
	Network        NetworkRef     `json:"network"`
	Infrastructure Infrastructure `json:"infrastructure"`
}

type NetworkRef struct {
	Kind    string `json:"kind"` // device | hop | link
	TTL     int    `json:"ttl,omitempty"`
	FromTTL *int   `json:"from_ttl,omitempty"`
	ToTTL   int    `json:"to_ttl,omitempty"`
	IP      string `json:"ip,omitempty"`
	ASN     int    `json:"asn,omitempty"`
}

type Infrastructure struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Operator    string   `json:"operator,omitempty"`
	Location    string   `json:"location,omitempty"`
	Countries   []string `json:"countries,omitempty"`
	CableID     string   `json:"cable_id,omitempty"`
	LandingA    string   `json:"landing_a,omitempty"`
	LandingB    string   `json:"landing_b,omitempty"`
	HubID       string   `json:"hub_id,omitempty"`
	Probability float64  `json:"probability,omitempty"`
	Confidence  float64  `json:"confidence"`
}

type Summary struct {
	Media              []string `json:"media"`
	HopCountries       []string `json:"hop_countries"`
	CountriesTraversed []string `json:"countries_traversed"` // overland: under the great circle; subsea: cable landings on the route
	SubseaCables       []string `json:"subsea_cables"`
	OceanCrossings     int      `json:"ocean_crossings"`
	Cellular           bool     `json:"cellular"`
	LocatedHops        int      `json:"located_hops"`
	RejectedLocations  int      `json:"rejected_locations"`
	PathKM             float64  `json:"path_km"`
}

type Provenance struct {
	GeoIP        string            `json:"geoip"`
	Datasets     map[string]string `json:"datasets"`
	EnrichmentMS float64           `json:"enrichment_ms"`
}
