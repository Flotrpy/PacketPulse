// Command infraenrich decorates a traceroute with infrastructure telemetry
// and prints one InfraMap JSON document (schema/infra_map.schema.json).
//
// Replay a recorded raw trace:
//
//	infraenrich -trace trace.json -mmdb-city GeoLite2-City.mmdb -mmdb-asn GeoLite2-ASN.mmdb \
//	    -device 19.0596,72.8295,1 -access cellular -mcc 405 -mnc 874
//
// Trace live (raw sockets: root or CAP_NET_RAW), using IPinfo:
//
//	IPINFO_TOKEN=... infraenrich -target 1.1.1.1 -ipinfo -rdns
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/enrich"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/geoip"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/mobile"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
	"github.com/flotrpy/packetpulse/backend/schema"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "infraenrich:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		traceFile, target, protocol, out, record, hostnamesFile                  string
		dataDir, cables, mmCity, mmASN, mmISP, staticGeo, ipinfoToken, ipinfoURL string
		device, deviceCountry, access, mcc, mnc, cell, ocidKey, ocidCSV          string
		maxHops, probes                                                          int
		useIPinfo, useIPinfoLite, rdns, routerHostnames, pretty, validate        bool
	)
	flag.StringVar(&traceFile, "trace", "", "raw trace JSON (traceroute.RawTrace) to enrich")
	flag.StringVar(&target, "target", "", "trace this IP/hostname live instead of -trace")
	flag.StringVar(&protocol, "protocol", "icmp", "live trace protocol: icmp or udp")
	flag.IntVar(&maxHops, "max-hops", 30, "live trace max TTL")
	flag.IntVar(&probes, "probes", 3, "live trace probes per hop")
	flag.StringVar(&dataDir, "data-dir", "auto", "repo data/ directory (auto = ./data, ../data, ../../data)")
	flag.StringVar(&cables, "cables", "", "cable GeoJSON (default: data/curated/infrastructure/submarine_cables.mock.geojson; the raw TeleGeography cable-geo.json.gz also works)")
	flag.StringVar(&mmCity, "mmdb-city", "", "GeoIP2/GeoLite2/DB-IP City .mmdb")
	flag.StringVar(&mmASN, "mmdb-asn", "", "GeoLite2/DB-IP ASN .mmdb")
	flag.StringVar(&mmISP, "mmdb-isp", "", "GeoIP2-ISP .mmdb (adds mobile MCC/MNC)")
	flag.StringVar(&staticGeo, "geoip-static", "", "static/recorded GeoIP JSON, consulted first")
	flag.BoolVar(&useIPinfo, "ipinfo", false, "query the IPinfo standard API (token from -ipinfo-token or IPINFO_TOKEN)")
	flag.BoolVar(&useIPinfoLite, "ipinfo-lite", false, "query the IPinfo Lite API (country + ASN)")
	flag.StringVar(&ipinfoToken, "ipinfo-token", os.Getenv("IPINFO_TOKEN"), "IPinfo token")
	flag.StringVar(&ipinfoURL, "ipinfo-url", "", "override IPinfo base URL")
	flag.StringVar(&device, "device", "", "device fix: lat,lon[,accuracy_km]")
	flag.StringVar(&deviceCountry, "device-country", "", "device ISO2 country")
	flag.StringVar(&access, "access", "", "device access type: wired, wifi, cellular, unknown")
	flag.StringVar(&mcc, "mcc", "", "device SIM/serving MCC")
	flag.StringVar(&mnc, "mnc", "", "device SIM/serving MNC")
	flag.StringVar(&cell, "cell", "", "serving cell RADIO:MCC:MNC:LAC:CELLID (needs -opencellid-key or -opencellid-csv)")
	flag.StringVar(&ocidKey, "opencellid-key", os.Getenv("OPENCELLID_TOKEN"), "OpenCellID API key")
	flag.StringVar(&ocidCSV, "opencellid-csv", "", "OpenCellID dump (.csv/.csv.gz)")
	flag.StringVar(&hostnamesFile, "hostnames", "", "JSON object {ip: ptr-name} for hops")
	flag.BoolVar(&rdns, "rdns", false, "resolve PTR names live")
	flag.BoolVar(&routerHostnames, "router-hostnames", true, "locate routers from city codes in their hostnames")
	flag.StringVar(&record, "record-geoip", "", "write every GeoIP answer to this file (replay with -geoip-static)")
	flag.StringVar(&out, "out", "-", "output file (- = stdout)")
	flag.BoolVar(&pretty, "pretty", true, "indent JSON")
	flag.BoolVar(&validate, "validate", true, "validate output against schema/infra_map.schema.json")
	flag.Parse()

	dir, err := findDataDir(dataDir)
	if err != nil {
		return err
	}
	e, closeFn, err := enrich.Build(enrich.Config{
		DataDir: dir, Cables: cables, StaticGeoIP: staticGeo, MaxMindCity: mmCity, MaxMindASN: mmASN, MaxMindISP: mmISP,
		IPinfo: useIPinfo, IPinfoLite: useIPinfoLite, IPinfoToken: ipinfoToken, IPinfoBaseURL: ipinfoURL,
		OpenCellIDKey: ocidKey, OpenCellIDCSV: ocidCSV, RouterHostnames: routerHostnames,
	})
	if err != nil {
		return err
	}
	defer closeFn()
	var rec *geoip.Recorder
	if record != "" {
		rec = &geoip.Recorder{Next: e.GeoIP}
		e.GeoIP = rec
	}
	if rdns {
		e.DNS = net.DefaultResolver
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rt, err := loadTrace(ctx, traceFile, target, traceroute.Options{Protocol: traceroute.Protocol(protocol), MaxHops: maxHops, ProbesPerHop: probes})
	if err != nil {
		return err
	}

	in := enrich.Input{Trace: rt}
	if hostnamesFile != "" {
		b, err := os.ReadFile(hostnamesFile)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &in.Hostnames); err != nil {
			return fmt.Errorf("hostnames: %w", err)
		}
	}
	if in.Device, err = parseDevice(device, deviceCountry, access, mcc, mnc, cell); err != nil {
		return err
	}

	m, err := e.Enrich(ctx, in)
	if err != nil {
		return err
	}
	if validate {
		if err := schema.ValidateInfraMap(m); err != nil {
			return fmt.Errorf("output violates schema: %w", err)
		}
	}
	if rec != nil {
		if err := rec.Save(record, e.GeoIP.Name()); err != nil {
			return err
		}
	}
	var w io.Writer = os.Stdout
	if out != "-" {
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(m)
}

func findDataDir(v string) (string, error) {
	if v != "auto" {
		return v, nil
	}
	for _, d := range []string{"data", "../data", "../../data"} {
		if st, err := os.Stat(filepath.Join(d, "raw", "naturalearth")); err == nil && st.IsDir() {
			return d, nil
		}
	}
	return "", errors.New("data directory not found; pass -data-dir")
}

func loadTrace(ctx context.Context, file, target string, opts traceroute.Options) (*traceroute.RawTrace, error) {
	switch {
	case file != "" && target != "":
		return nil, errors.New("use -trace or -target, not both")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var rt traceroute.RawTrace
		if err := json.Unmarshal(b, &rt); err != nil {
			return nil, fmt.Errorf("trace %s: %w", file, err)
		}
		return &rt, nil
	case target != "":
		addr, err := netip.ParseAddr(target)
		if err != nil {
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", target)
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("resolve %s: %v", target, err)
			}
			addr = ips[0]
		}
		eng := traceroute.NewEngine(200)
		defer eng.Close()
		return eng.Trace(ctx, addr, opts)
	}
	return nil, errors.New("pass -trace FILE or -target HOST")
}

func parseDevice(fix, country, access, mcc, mnc, cell string) (*enrich.Device, error) {
	if fix == "" && country == "" && access == "" && mcc == "" && cell == "" {
		return nil, nil
	}
	d := &enrich.Device{Country: strings.ToUpper(country), AccessType: access, MCC: mcc, MNC: mnc}
	if fix != "" {
		parts := strings.Split(fix, ",")
		if len(parts) < 2 {
			return nil, errors.New("-device wants lat,lon[,accuracy_km]")
		}
		var err1, err2 error
		d.Lat, err1 = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		d.Lon, err2 = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 != nil || err2 != nil {
			return nil, errors.New("-device: bad coordinates")
		}
		if len(parts) > 2 {
			d.AccuracyKM, _ = strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		}
		d.HasFix = true
	}
	if cell != "" {
		p := strings.Split(cell, ":")
		if len(p) != 5 {
			return nil, errors.New("-cell wants RADIO:MCC:MNC:LAC:CELLID")
		}
		m1, e1 := strconv.Atoi(p[1])
		m2, e2 := strconv.Atoi(p[2])
		lac, e3 := strconv.ParseInt(p[3], 10, 64)
		cid, e4 := strconv.ParseInt(p[4], 10, 64)
		if err := errors.Join(e1, e2, e3, e4); err != nil {
			return nil, fmt.Errorf("-cell: %w", err)
		}
		d.Cell = &mobile.CellKey{Radio: strings.ToUpper(p[0]), MCC: m1, MNC: m2, LAC: lac, CellID: cid}
	}
	return d, nil
}
