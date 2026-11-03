// Command complianced runs the compliance engine as an independent service.
//
//	COMPLIANCE_ADMIN_TOKENS="alice:<token>" complianced -listen :8090 -store /var/lib/packetpulse/compliance.json
//
// Profiles, regions and proxies live in -store (created from the built-in
// defaults on first start). Admin changes made through the API are written
// there atomically and take effect immediately; no restart is needed.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/compliance"
	"github.com/flotrpy/packetpulse/backend/internal/compliance/api"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "complianced:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", ":8090", "HTTP listen address")
	dataDir := flag.String("data-dir", "auto", "repo data/ directory (Natural Earth polygons)")
	store := flag.String("store", "compliance-registry.json", "registry file (profiles, regions, proxies, audit log); empty = memory only")
	alertCap := flag.Int("alert-capacity", 10000, "alerts kept in memory")
	lawsPath := flag.String("laws", "", "law directory JSON (default <data-dir>/curated/regulations/jurisdictions.json; none = disabled)")
	corsOrigins := flag.String("cors-origins", "", "comma-separated browser origins allowed to call the API directly (* = any)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dir, err := findDataDir(*dataDir)
	if err != nil {
		return err
	}
	ne := filepath.Join(dir, "raw", "naturalearth")
	loc, err := compliance.LoadLocator(filepath.Join(ne, "ne_50m_admin_0_countries.geojson.gz"), filepath.Join(ne, "ne_50m_admin_1_states_provinces.geojson.gz"))
	if err != nil {
		return err
	}
	reg, err := compliance.NewRegistry(*store)
	if err != nil {
		return err
	}
	alerts := compliance.NewAlertStore(*alertCap)
	var laws *compliance.LawDirectory
	if *lawsPath != "none" {
		p := *lawsPath
		if p == "" {
			p = filepath.Join(dir, "curated", "regulations", "jurisdictions.json")
		}
		if laws, err = compliance.LoadLawDirectory(p); err != nil {
			log.Warn("law directory not loaded", "path", p, "err", err)
		}
	}
	var origins []string
	for _, o := range strings.Split(*corsOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	admins, err := api.ParseTokens(os.Getenv("COMPLIANCE_ADMIN_TOKENS"))
	if err != nil {
		return fmt.Errorf("COMPLIANCE_ADMIN_TOKENS: %w", err)
	}
	readers, err := api.ParseTokens(os.Getenv("COMPLIANCE_READ_TOKENS"))
	if err != nil {
		return fmt.Errorf("COMPLIANCE_READ_TOKENS: %w", err)
	}
	if len(admins) == 0 {
		b := make([]byte, 24)
		rand.Read(b)
		tok := hex.EncodeToString(b)
		admins = map[string]string{tok: "bootstrap-admin"}
		log.Warn("no COMPLIANCE_ADMIN_TOKENS set; generated a one-off admin token for this process", "token", tok)
	}

	srv := &http.Server{
		Addr: *listen,
		Handler: (&api.Server{
			Engine:      &compliance.Engine{Registry: reg, Locator: loc, Sink: alerts},
			Alerts:      alerts,
			AdminTokens: admins,
			ReadTokens:  readers,
			Laws:        laws,
			CORSOrigins: origins,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	snap := reg.Snapshot()
	log.Info("compliance engine ready", "listen", *listen, "profiles", len(snap.Profiles), "regions", len(snap.Regions), "store", *store)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
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
