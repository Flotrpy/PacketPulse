// Command packetpulse runs the whole stack in one process on one port, for
// test deployments:
//
//	/v1/compliance/...  compliance engine API (profiles, alerts, evaluate)
//	/v1/...             ingestion API (traces, journeys, stream, classification rules)
//	/healthz            combined health, including whether tracing is possible
//	/                   the globe (frontend/dist)
//
// Live traffic is tagged with data classifications at ingestion; tagged
// journeys are evaluated by the in-process compliance engine and raise
// alerts into /v1/compliance/alerts and /v1/compliance/alerts/stream.
//
// Tokens (env, "actor:token" pairs, tokens >= 16 chars):
//
//	PACKETPULSE_ADMIN_TOKENS  admin for compliance profiles and classification rules; may submit traces
//	PACKETPULSE_TRACE_TOKENS  may submit traces (POST /v1/traces always needs a token here)
//	PACKETPULSE_AGENT_TOKENS  remote agents that push journeys (POST /v1/journeys), e.g.
//	                          `sudo ingestd -forward-url https://<host> ...` with FORWARD_TOKEN
//	                          on a machine that can trace when this host cannot
//	PACKETPULSE_READ_TOKENS   if set, compliance reads need a token too
//
// Tracing needs raw ICMP sockets (root or CAP_NET_RAW). Without them the
// service still runs; /healthz reports "tracing": false.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

	"golang.org/x/net/icmp"

	"github.com/flotrpy/packetpulse/backend/internal/api"
	"github.com/flotrpy/packetpulse/backend/internal/bridge"
	"github.com/flotrpy/packetpulse/backend/internal/classify"
	"github.com/flotrpy/packetpulse/backend/internal/compliance"
	capi "github.com/flotrpy/packetpulse/backend/internal/compliance/api"
	"github.com/flotrpy/packetpulse/backend/internal/ingest"
	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/pipeline"
	"github.com/flotrpy/packetpulse/backend/internal/store"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "packetpulse:", err)
		os.Exit(1)
	}
}

func run() error {
	defListen := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		defListen = ":" + p
	}
	host, _ := os.Hostname()
	listen := flag.String("listen", defListen, "HTTP listen address (default :$PORT or :8080)")
	dataDir := flag.String("data-dir", "auto", "repo data/ directory")
	webDir := flag.String("web", "auto", "built globe (frontend/dist); none = API only")
	stateDir := flag.String("state-dir", "", "where compliance registry and classification rules are saved; empty = memory only")
	workers := flag.Int("workers", 4, "concurrent traceroutes")
	pps := flag.Int("pps", 50, "global probe rate limit (packets/s)")
	agentID := flag.String("agent-id", host, "agent identifier stamped on journeys")
	noRDNS := flag.Bool("no-rdns", false, "disable reverse DNS")
	publicOnly := flag.Bool("public-targets-only", true, "refuse targets that resolve to non-public addresses")
	seed := flag.String("seed", "", "glob of PacketJourney files to load at startup (e.g. schema/examples/*.journey.json); they are classified, stored and evaluated like live traces")
	srcCountry := flag.String("source-country", "", "ISO2 country of this host (enables RTT feasibility checks)")
	srcLat := flag.Float64("source-lat", 0, "latitude of this host")
	srcLon := flag.Float64("source-lon", 0, "longitude of this host")
	srcCity := flag.String("source-city", "", "city of this host")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dir, err := findDataDir(*dataDir)
	if err != nil {
		return err
	}
	statePath := func(name string) string {
		if *stateDir == "" {
			return ""
		}
		return filepath.Join(*stateDir, name)
	}
	if *stateDir != "" {
		if err := os.MkdirAll(*stateDir, 0o700); err != nil {
			return err
		}
	}

	admins, err := tokens("PACKETPULSE_ADMIN_TOKENS")
	if err != nil {
		return err
	}
	tracers, err := tokens("PACKETPULSE_TRACE_TOKENS")
	if err != nil {
		return err
	}
	agents, err := tokens("PACKETPULSE_AGENT_TOKENS")
	if err != nil {
		return err
	}
	readers, err := tokens("PACKETPULSE_READ_TOKENS")
	if err != nil {
		return err
	}
	if len(admins) == 0 {
		b := make([]byte, 24)
		rand.Read(b)
		tok := hex.EncodeToString(b)
		admins = map[string]string{tok: "bootstrap-admin"}
		log.Warn("PACKETPULSE_ADMIN_TOKENS not set; generated a one-off admin token for this process", "token", tok)
	}

	// ---- compliance engine ----
	ne := filepath.Join(dir, "raw", "naturalearth")
	loc, err := compliance.LoadLocator(filepath.Join(ne, "ne_50m_admin_0_countries.geojson.gz"), filepath.Join(ne, "ne_50m_admin_1_states_provinces.geojson.gz"))
	if err != nil {
		return err
	}
	reg, err := compliance.NewRegistry(statePath("compliance-registry.json"))
	if err != nil {
		return err
	}
	alerts := compliance.NewAlertStore(10000)
	laws, err := compliance.LoadLawDirectory(filepath.Join(dir, "curated", "regulations", "jurisdictions.json"))
	if err != nil {
		log.Warn("law directory not loaded", "err", err)
	}
	engine := &compliance.Engine{Registry: reg, Locator: loc, Sink: alerts}

	// ---- ingestion ----
	ref := store.NewSeededReference()
	if rep, err := store.LoadDataDir(ref, dir); err != nil {
		log.Warn("reference data not loaded; using seed fixtures", "err", err)
	} else {
		log.Info("reference data loaded", "loaded", rep.Loaded, "warnings", len(rep.Warnings))
	}
	repo := store.NewMemoryRepository(5000)
	cls, err := classify.New(statePath("classification-rules.json"), classify.DefaultRules())
	if err != nil {
		return err
	}
	var dns pipeline.ReverseResolver
	if !*noRDNS {
		dns = pipeline.SystemResolver()
	}
	var srcLoc *model.Location
	if *srcCountry != "" {
		srcLoc = &model.Location{Lat: *srcLat, Lon: *srcLon, City: *srcCity, CountryISO2: strings.ToUpper(*srcCountry), AccuracyKM: 25, Method: "configured", Confidence: 1}
	}
	pipe := pipeline.Default(ref, repo, dns, cls)
	tracer := traceroute.NewEngine(*pps)
	defer tracer.Close()
	coord := ingest.New(ingest.Config{Workers: *workers, QueueSize: 128, TraceTimeout: 90 * time.Second, AgentID: *agentID,
		SourceLoc: srcLoc, PublicTargetsOnly: *publicOnly}, tracer, pipe, ref, nil, log)
	tracing := canTrace()
	if !tracing {
		log.Warn("raw ICMP sockets unavailable (needs root or CAP_NET_RAW): trace requests will fail; remote agents can push journeys to POST /v1/journeys")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	coord.Start(ctx)

	// Tagged journeys -> compliance engine -> alert stream.
	br := &bridge.Bridge{Eval: engine, Log: log}
	journeys, _ := coord.Subscribe()
	go br.Run(ctx, journeys)
	if *seed != "" {
		n, err := seedJourneys(ctx, *seed, repo, br)
		if err != nil {
			return err
		}
		log.Info("seeded journeys", "count", n)
	}

	// ---- HTTP ----
	ingestAPI := (&api.Server{Coord: coord, Repo: repo, Ref: ref, Stats: ref.Stats, Classifier: cls, AdminTokens: admins, AgentTokens: agents}).Handler()
	complianceAPI := (&capi.Server{Engine: engine, Alerts: alerts, AdminTokens: admins, ReadTokens: readers, Laws: laws}).Handler()
	mux := http.NewServeMux()
	mux.Handle("/v1/compliance/", complianceAPI)
	mux.Handle("/v1/", requireTraceToken(ingestAPI, admins, tracers))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		n, _ := repo.Count(r.Context())
		snap := reg.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "tracing": tracing, "ingest": coord.Stats(), "journeys_stored": n,
			"classification_rules": cls.Rules().Version, "compliance_bridge": br.Stats(),
			"compliance": map[string]any{"registry_snapshot": snap.Seq, "profiles": len(snap.Profiles), "regions": len(snap.Regions)},
		})
	})
	if web := findWebDir(*webDir); web != "" {
		mux.Handle("/", staticHandler(web))
		log.Info("serving globe", "dir", web)
	}

	srv := &http.Server{Addr: *listen, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("packetpulse listening", "addr", *listen, "tracing", tracing, "profiles", len(reg.Snapshot().Profiles), "rules", len(cls.Rules().Rules))
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return coord.Shutdown(sctx)
}

func tokens(env string) (map[string]string, error) {
	m, err := capi.ParseTokens(os.Getenv(env))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", env, err)
	}
	return m, nil
}

// requireTraceToken guards POST /v1/traces: an internet-facing host must not
// launch traceroutes for anonymous callers.
func requireTraceToken(next http.Handler, sets ...map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			ok := false
			for _, s := range sets {
				if _, hit := capi.MatchToken(s, tok); hit {
					ok = true
				}
			}
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="traces"`)
				http.Error(w, `{"error":"trace token required"}`, http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

// staticHandler serves the built globe; hashed assets are cached forever.
func staticHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fs.ServeHTTP(w, r)
	})
}

// canTrace reports whether a raw ICMP socket can be opened.
func canTrace() bool {
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// seedJourneys loads recorded journeys (keeping the classification they were
// ingested with), stores them and sends them through the compliance bridge
// like a finished live trace.
func seedJourneys(ctx context.Context, glob string, repo store.JourneyRepository, br *bridge.Bridge) (int, error) {
	files, err := filepath.Glob(glob)
	if err != nil {
		return 0, err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		var j model.Journey
		if err := json.Unmarshal(b, &j); err != nil {
			return 0, fmt.Errorf("%s: %w", f, err)
		}
		if err := repo.Save(ctx, &j); err != nil {
			return 0, err
		}
		br.Handle(ctx, &j)
	}
	return len(files), nil
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

func findWebDir(v string) string {
	switch v {
	case "none":
		return ""
	case "auto":
		for _, d := range []string{"web", "frontend/dist", "../frontend/dist"} {
			if _, err := os.Stat(filepath.Join(d, "index.html")); err == nil {
				return d
			}
		}
		return ""
	}
	return v
}
