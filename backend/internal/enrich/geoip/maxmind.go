package geoip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/oschwald/geoip2-golang"
)

// MaxMind reads local MMDB databases. Any of the three may be omitted:
//
//	City: GeoIP2-City, GeoLite2-City, or DB-IP City Lite   -> coordinates, city, country
//	ASN:  GeoLite2-ASN or DB-IP ASN Lite                    -> ASN, AS organisation
//	ISP:  GeoIP2-ISP (commercial)                           -> ASN, ISP, mobile MCC/MNC
//
// Readers memory-map the files and are safe for concurrent use.
type MaxMind struct {
	city, asn, isp *geoip2.Reader
	// DefaultAccuracyKM is used when the database has no accuracy radius
	// (DB-IP Lite) and a city was resolved.
	DefaultAccuracyKM float64
}

// MaxMindPaths names the database files to open (empty = skip).
type MaxMindPaths struct {
	City, ASN, ISP string
}

func OpenMaxMind(p MaxMindPaths) (*MaxMind, error) {
	m := &MaxMind{DefaultAccuracyKM: 50}
	open := func(path, want string) (*geoip2.Reader, error) {
		if path == "" {
			return nil, nil
		}
		r, err := geoip2.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s database %s: %w", want, path, err)
		}
		return r, nil
	}
	var err error
	if m.city, err = open(p.City, "city"); err != nil {
		return nil, err
	}
	if m.asn, err = open(p.ASN, "asn"); err != nil {
		m.Close()
		return nil, err
	}
	if m.isp, err = open(p.ISP, "isp"); err != nil {
		m.Close()
		return nil, err
	}
	if m.city == nil && m.asn == nil && m.isp == nil {
		return nil, errors.New("maxmind: no database paths given")
	}
	return m, nil
}

func (m *MaxMind) Name() string { return "maxmind" }

func (m *MaxMind) Close() error {
	var errs []error
	for _, r := range []*geoip2.Reader{m.city, m.asn, m.isp} {
		if r != nil {
			errs = append(errs, r.Close())
		}
	}
	return errors.Join(errs...)
}

// DatabaseTypes reports the metadata type of each open database (e.g.
// "GeoLite2-City", "DBIP-City-Lite"), useful for audit trails.
func (m *MaxMind) DatabaseTypes() map[string]string {
	out := map[string]string{}
	for k, r := range map[string]*geoip2.Reader{"city": m.city, "asn": m.asn, "isp": m.isp} {
		if r != nil {
			out[k] = r.Metadata().DatabaseType
		}
	}
	return out
}

func (m *MaxMind) Lookup(ctx context.Context, ip netip.Addr) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	ip = ip.Unmap()
	if IsBogon(ip) {
		return Record{}, ErrBogon
	}
	nip := net.IP(ip.AsSlice())
	rec := Record{IP: ip.String()}
	found := false

	if m.city != nil {
		c, err := m.city.City(nip)
		if err != nil {
			return Record{}, fmt.Errorf("maxmind city: %w", err)
		}
		if c.Location.Latitude != 0 || c.Location.Longitude != 0 {
			rec.Lat, rec.Lon, rec.HasLocation = c.Location.Latitude, c.Location.Longitude, true
			rec.AccuracyKM = float64(c.Location.AccuracyRadius)
			rec.City = c.City.Names["en"]
			if rec.AccuracyKM == 0 {
				rec.AccuracyKM = m.DefaultAccuracyKM
				if rec.City == "" {
					rec.AccuracyKM = 500 // country-level only
				}
			}
			if len(c.Subdivisions) > 0 {
				rec.Region = c.Subdivisions[0].Names["en"]
			}
			rec.source("location", "maxmind")
			found = true
		}
		if c.Country.IsoCode != "" {
			rec.CountryISO2 = c.Country.IsoCode
			found = true
		}
	}
	if m.isp != nil {
		i, err := m.isp.ISP(nip)
		if err != nil {
			return Record{}, fmt.Errorf("maxmind isp: %w", err)
		}
		if i.AutonomousSystemNumber != 0 {
			rec.ASN, rec.ASOrg = int(i.AutonomousSystemNumber), i.AutonomousSystemOrganization
			rec.source("asn", "maxmind")
			found = true
		}
		if i.ISP != "" {
			rec.ISP = i.ISP
			rec.source("isp", "maxmind")
		}
		if i.MobileCountryCode != "" {
			rec.MobileMCC, rec.MobileMNC = i.MobileCountryCode, i.MobileNetworkCode
			rec.source("mobile", "maxmind")
		}
	}
	if m.asn != nil && rec.ASN == 0 {
		a, err := m.asn.ASN(nip)
		if err != nil {
			return Record{}, fmt.Errorf("maxmind asn: %w", err)
		}
		if a.AutonomousSystemNumber != 0 {
			rec.ASN, rec.ASOrg = int(a.AutonomousSystemNumber), a.AutonomousSystemOrganization
			rec.source("asn", "maxmind")
			found = true
		}
	}
	if !found {
		return Record{}, ErrNotFound
	}
	return rec, nil
}
