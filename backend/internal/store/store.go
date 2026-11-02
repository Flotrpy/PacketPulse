// Package store is the database service layer. The pipeline depends only on
// the ReferenceStore and JourneyRepository interfaces; this package ships
// in-memory (mock) implementations seeded with real-world fixtures and able
// to bulk-load the research datasets under data/. A PostGIS-backed
// implementation can replace them without touching the pipeline.
package store

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/model"
)

var ErrNotFound = errors.New("not found")

type ASNInfo struct {
	ASN             int
	Name            string
	RegistryCountry string // RIR/registry country: the legal holder, not the physical location
}

type Country struct {
	ISO2      string
	Name      string
	Continent string
	Lat, Lon  float64 // label point / centroid
	Island    bool    // no land border usable by long-haul fiber to other countries
}

type City struct {
	Name        string
	CountryISO2 string
	Lat, Lon    float64
	Tokens      []string // hostname tokens: full name, IATA, CLLI-6, operator abbreviations
}

// GeoHit is a prefix-level geolocation (mock of a MaxMind/IPinfo lookup).
type GeoHit struct {
	Prefix      netip.Prefix
	City        string
	CountryISO2 string
	Lat, Lon    float64
	AccuracyKM  float64
}

type LandingPoint struct {
	ID          string
	Name        string
	CountryISO2 string
	Lat, Lon    float64
}

type Cable struct {
	ID       string
	Name     string
	LengthKM float64
	RFSYear  int
	Planned  bool
	Owners   []string
	Landings []LandingPoint
}

type Carrier struct {
	ASN         int
	Name        string
	CountryISO2 string
	MCC         string
	MNCs        []string
}

type Policy struct {
	ID                 string
	Label              string
	Allowed            map[string]bool // empty = no allow-list (only Restricted applies)
	Restricted         map[string]bool
	TransitIsViolation bool
}

// ReferenceStore answers enrichment lookups. Implementations must be safe for
// concurrent reads.
type ReferenceStore interface {
	LookupASN(addr netip.Addr) (ASNInfo, bool)
	LookupGeo(addr netip.Addr) (GeoHit, bool)
	MatchHostname(hostname string) (City, string, bool) // city, matched token
	Country(iso2 string) (Country, bool)
	CablesBetween(countryA, countryB string) []Cable
	MobileCarrier(asn int) (Carrier, bool)
	CloudProvider(asn int) (string, bool)
	Policy(id string) (Policy, bool)
	Policies() []Policy
}

type JourneyFilter struct {
	DestinationIP string
	Verdict       string
	Since         time.Time
	Limit         int
}

// JourneyRepository persists enriched journeys.
type JourneyRepository interface {
	Save(ctx context.Context, j *model.Journey) error
	Get(ctx context.Context, id string) (*model.Journey, error)
	List(ctx context.Context, f JourneyFilter) ([]*model.Journey, error)
	Count(ctx context.Context) (int, error)
}
