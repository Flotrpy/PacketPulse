package enrich

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/geoip"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/mobile"
	"github.com/flotrpy/packetpulse/backend/internal/enrich/subsea"
	"github.com/flotrpy/packetpulse/backend/internal/store"
)

// Config locates every data source. Empty fields are skipped.
type Config struct {
	DataDir string // repo data/ (Natural Earth, PeeringDB, MCC-MNC)
	Cables  string // cable GeoJSON; default <DataDir>/curated/infrastructure/submarine_cables.mock.geojson

	// GeoIP providers, tried in this order: Static overrides, MaxMind, IPinfo.
	StaticGeoIP    string
	MaxMindCity    string
	MaxMindASN     string
	MaxMindISP     string
	IPinfo         bool
	IPinfoLite     bool
	IPinfoToken    string
	IPinfoBaseURL  string
	GeoIPCacheSize int

	OpenCellIDKey     string
	OpenCellIDBaseURL string
	OpenCellIDCSV     string // dump file (.csv or .csv.gz)

	// RouterHostnames enables the router-hostname gazetteer (store seed).
	RouterHostnames bool
}

// Closer releases resources opened by Build (MMDB readers).
type Closer func() error

// Build assembles an Enricher from Config.
func Build(cfg Config) (*Enricher, Closer, error) {
	e := &Enricher{Datasets: map[string]string{}}
	var closers []func() error
	closeAll := func() error {
		var errs []error
		for _, c := range closers {
			errs = append(errs, c())
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (*Enricher, Closer, error) {
		closeAll()
		return nil, nil, err
	}
	raw := filepath.Join(cfg.DataDir, "raw")

	// --- GeoIP chain ---
	var chain geoip.Chain
	if cfg.StaticGeoIP != "" {
		s, err := geoip.LoadStatic(cfg.StaticGeoIP)
		if err != nil {
			return fail(err)
		}
		chain.Providers = append(chain.Providers, s)
		e.Datasets["geoip_static"] = s.Name()
	}
	if cfg.MaxMindCity != "" || cfg.MaxMindASN != "" || cfg.MaxMindISP != "" {
		mm, err := geoip.OpenMaxMind(geoip.MaxMindPaths{City: cfg.MaxMindCity, ASN: cfg.MaxMindASN, ISP: cfg.MaxMindISP})
		if err != nil {
			return fail(err)
		}
		closers = append(closers, mm.Close)
		chain.Providers = append(chain.Providers, mm)
		for k, v := range mm.DatabaseTypes() {
			e.Datasets["mmdb_"+k] = v
		}
	}
	if cfg.IPinfo || cfg.IPinfoLite {
		c := geoip.NewIPinfo(cfg.IPinfoToken)
		if cfg.IPinfoLite {
			c = geoip.NewIPinfoLite(cfg.IPinfoToken)
		}
		if cfg.IPinfoBaseURL != "" {
			c.BaseURL = cfg.IPinfoBaseURL
		}
		chain.Providers = append(chain.Providers, c)
		e.Datasets["ipinfo"] = c.Name()
	}
	if len(chain.Providers) == 0 {
		return fail(errors.New("no GeoIP provider configured (MaxMind/DB-IP mmdb, IPinfo, or a static file)"))
	}
	size := cfg.GeoIPCacheSize
	if size == 0 {
		size = 100000
	}
	e.GeoIP = geoip.NewCache(&chain, size, 24*time.Hour)

	// --- land polygons ---
	landPath := filepath.Join(raw, "naturalearth", "ne_50m_admin_0_countries.geojson.gz")
	land, err := gis.LoadLandIndex(landPath)
	if err != nil {
		return fail(fmt.Errorf("land index: %w", err))
	}
	e.Land = land
	e.Datasets["land"] = "Natural Earth 1:50m admin-0 (" + strconv.Itoa(land.PolygonCount()) + " polygons)"

	// --- cables ---
	cables := cfg.Cables
	if cables == "" {
		cables = filepath.Join(cfg.DataDir, "curated", "infrastructure", "submarine_cables.mock.geojson")
	}
	ds, err := subsea.Load(cables)
	if err != nil {
		return fail(fmt.Errorf("cables: %w", err))
	}
	e.Cables = subsea.NewMatcher(ds)
	e.Datasets["cables"] = fmt.Sprintf("%s (%d cables)", filepath.Base(cables), len(ds.Cables))

	// --- MNO registry + gateway hubs (PeeringDB) ---
	reg := mobile.NewRegistry()
	for _, op := range mobile.Seed() {
		reg.Add(op)
	}
	pdb := func(t string) string { return filepath.Join(raw, "peeringdb", t+".json.gz") }
	if n, err := reg.LoadPeeringDBMatches(filepath.Join(raw, "mcc-mnc", "mcc-mnc-list.json.gz"), pdb("net"), pdb("org")); err == nil {
		e.Datasets["mno_registry"] = fmt.Sprintf("%d seed + %d PeeringDB x MCC-MNC matches", len(mobile.Seed()), n)
	} else {
		e.Datasets["mno_registry"] = fmt.Sprintf("%d seed (PeeringDB match unavailable: %v)", len(mobile.Seed()), err)
	}
	e.MNO = reg
	if hubs, err := mobile.LoadHubDirectory(pdb("fac"), pdb("netfac"), pdb("netixlan"), pdb("ixfac")); err == nil {
		e.Hubs = hubs
		e.Datasets["gateway_hubs"] = "PeeringDB fac/netfac/netixlan/ixfac"
	}

	// --- cell towers ---
	switch {
	case cfg.OpenCellIDCSV != "":
		loc, err := loadCellCSV(cfg.OpenCellIDCSV)
		if err != nil {
			return fail(err)
		}
		e.Cells = loc
		e.Datasets["cells"] = fmt.Sprintf("OpenCellID dump (%d cells)", loc.Len())
	case cfg.OpenCellIDKey != "":
		e.Cells = &mobile.OpenCellIDClient{Key: cfg.OpenCellIDKey, BaseURL: cfg.OpenCellIDBaseURL}
		e.Datasets["cells"] = "OpenCellID API"
	}

	if cfg.RouterHostnames {
		e.Hostnames = StoreHostnames{Ref: store.NewSeededReference()}
		e.Datasets["router_hostnames"] = "PacketPulse seed gazetteer (IATA/CLLI/city tokens)"
	}
	return e, closeAll, nil
}

func loadCellCSV(path string) (*mobile.CSVLocator, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	return mobile.LoadCSV(r, nil)
}

// StoreHostnames adapts the store's router-hostname gazetteer.
type StoreHostnames struct {
	Ref interface {
		MatchHostname(string) (store.City, string, bool)
	}
}

func (s StoreHostnames) LocateHostname(host string) (float64, float64, string, string, bool) {
	c, _, ok := s.Ref.MatchHostname(host)
	if !ok {
		return 0, 0, "", "", false
	}
	return c.Lat, c.Lon, c.Name, c.CountryISO2, true
}
