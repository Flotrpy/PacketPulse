// Command ingestd runs concurrent traceroutes and emits enriched
// PacketJourney documents.
//
// Batch mode (JSON Lines to stdout):
//
//	sudo ingestd -policy eu-gdpr-strict 8.8.8.8 1.1.1.1 example.com
//
// Service mode (HTTP API, see internal/api):
//
//	sudo ingestd -serve :8080
//
// Data classification: traces are tagged from -rules (or explicit labels
// with -classify); tagged journeys can be sent to a complianced with
// -compliance-url, which raises alerts into its operational stream:
//
//	sudo ingestd -app upi-payments -port 443 -compliance-url http://localhost:8090 razorpay.com
//
// Raw ICMP sockets need root or CAP_NET_RAW:
//
//	sudo setcap cap_net_raw+ep ./ingestd
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/api"
	"github.com/flotrpy/packetpulse/backend/internal/bridge"
	"github.com/flotrpy/packetpulse/backend/internal/classify"
	capi "github.com/flotrpy/packetpulse/backend/internal/compliance/api"
	"github.com/flotrpy/packetpulse/backend/internal/ingest"
	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/pipeline"
	"github.com/flotrpy/packetpulse/backend/internal/store"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

type flags struct {
	serve, targets, protocol, policy, access, dataDir, out, agentID, logLevel string
	maxHops, probes, firstTTL, ipVersion, workers, queue, pps, repoCap        int
	timeout, traceTimeout                                                     time.Duration
	noRDNS, pretty                                                            bool
	srcLat, srcLon, srcAcc                                                    float64
	srcCountry, srcCity                                                       string
	rules, classes, app, sni, complianceURL                                   string
	port                                                                      int
	publicOnly                                                                bool
}

func parseFlags() *flags {
	f := &flags{}
	host, _ := os.Hostname()
	flag.StringVar(&f.serve, "serve", "", "listen address for the HTTP API (e.g. :8080); empty = batch mode")
	flag.StringVar(&f.targets, "targets", "", "comma-separated targets for batch mode (positional args also accepted)")
	flag.StringVar(&f.protocol, "protocol", "icmp", "probe protocol: icmp or udp")
	flag.IntVar(&f.maxHops, "max-hops", 30, "maximum TTL")
	flag.IntVar(&f.firstTTL, "first-ttl", 1, "first TTL to probe")
	flag.IntVar(&f.probes, "probes", 3, "probes per hop")
	flag.DurationVar(&f.timeout, "timeout", 2*time.Second, "per-probe timeout")
	flag.IntVar(&f.ipVersion, "ip-version", 0, "force 4 or 6 when resolving hostnames (0 = prefer IPv4)")
	flag.StringVar(&f.policy, "policy", "", "compliance policy id (see -list-policies)")
	flag.StringVar(&f.access, "access", "", "source access type: wired, wifi, cellular, unknown")
	flag.IntVar(&f.workers, "workers", 8, "concurrent traceroutes")
	flag.IntVar(&f.queue, "queue", 256, "max queued jobs")
	flag.IntVar(&f.pps, "pps", 200, "global probe rate limit (packets/s, 0 = unlimited)")
	flag.DurationVar(&f.traceTimeout, "trace-timeout", 90*time.Second, "hard cap per trace")
	flag.IntVar(&f.repoCap, "repo-capacity", 10000, "journeys kept by the in-memory repository")
	flag.StringVar(&f.dataDir, "data-dir", "auto", "research data directory (repo data/); auto = ./data or ../data; none = seed fixtures only")
	flag.BoolVar(&f.noRDNS, "no-rdns", false, "disable reverse DNS lookups")
	flag.Float64Var(&f.srcLat, "source-lat", 0, "source latitude (enables RTT feasibility checks)")
	flag.Float64Var(&f.srcLon, "source-lon", 0, "source longitude")
	flag.StringVar(&f.srcCountry, "source-country", "", "source ISO2 country (required with -source-lat/-source-lon)")
	flag.StringVar(&f.srcCity, "source-city", "", "source city name")
	flag.Float64Var(&f.srcAcc, "source-accuracy-km", 10, "source location accuracy")
	flag.StringVar(&f.agentID, "agent-id", host, "agent identifier stamped on journeys")
	flag.StringVar(&f.out, "out", "-", "batch output file (JSON Lines); - = stdout")
	flag.BoolVar(&f.pretty, "pretty", false, "pretty-print batch output")
	flag.StringVar(&f.logLevel, "log-level", "info", "debug, info, warn, error")
	flag.StringVar(&f.rules, "rules", "classification-rules.json", "data classification rules file (created from built-in examples if missing; empty = memory only)")
	flag.StringVar(&f.classes, "classify", "", "batch mode: comma-separated data classifications for every target (overrides rules)")
	flag.StringVar(&f.app, "app", "", "batch mode: application label of the traffic, for classification rules")
	flag.StringVar(&f.sni, "sni", "", "batch mode: TLS server name of the traffic, for classification rules")
	flag.IntVar(&f.port, "port", 0, "batch mode: destination service port of the traffic, for classification rules")
	flag.StringVar(&f.complianceURL, "compliance-url", "", "complianced base URL; classified journeys are evaluated there and raise alerts (token: COMPLIANCE_TOKEN)")
	flag.BoolVar(&f.publicOnly, "public-targets-only", false, "refuse targets that resolve to private/loopback/CGNAT/link-local addresses")
	listPolicies := flag.Bool("list-policies", false, "print available policies and exit")
	flag.Parse()
	if *listPolicies {
		f.serve = "__list_policies__"
	}
	return f
}

func main() {
	f := parseFlags()
	if err := run(f); err != nil {
		fmt.Fprintln(os.Stderr, "ingestd:", err)
		os.Exit(1)
	}
}

func run(f *flags) error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(f.logLevel)); err != nil {
		return fmt.Errorf("bad -log-level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	// ---- database service layer ----
	ref := store.NewSeededReference()
	if dir := resolveDataDir(f.dataDir); dir != "" {
		t0 := time.Now()
		rep, err := store.LoadDataDir(ref, dir)
		if err != nil {
			log.Warn("reference data not loaded; using seed fixtures", "err", err)
		} else {
			log.Info("reference data loaded", "dir", dir, "took", time.Since(t0).Round(time.Millisecond), "loaded", rep.Loaded)
			for _, w := range rep.Warnings {
				log.Warn("reference data", "warning", w)
			}
		}
	}
	if f.serve == "__list_policies__" {
		for _, p := range ref.Policies() {
			fmt.Printf("%-20s %s\n", p.ID, p.Label)
		}
		return nil
	}
	repo := store.NewMemoryRepository(f.repoCap)

	// ---- pipeline + traceroute engine + coordinator ----
	var dns pipeline.ReverseResolver
	if !f.noRDNS {
		dns = pipeline.SystemResolver()
	}
	cls, err := classify.New(f.rules, classify.DefaultRules())
	if err != nil {
		return err
	}
	pipe := pipeline.Default(ref, repo, dns, cls)
	engine := traceroute.NewEngine(f.pps)
	defer engine.Close()

	var srcLoc *model.Location
	if f.srcCountry != "" {
		srcLoc = &model.Location{Lat: f.srcLat, Lon: f.srcLon, City: f.srcCity, CountryISO2: strings.ToUpper(f.srcCountry),
			AccuracyKM: f.srcAcc, Method: "configured", Confidence: 1}
	}
	coord := ingest.New(ingest.Config{
		Workers: f.workers, QueueSize: f.queue, TraceTimeout: f.traceTimeout, AgentID: f.agentID, SourceLoc: srcLoc,
		PublicTargetsOnly: f.publicOnly,
	}, engine, pipe, ref, nil, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	coord.Start(ctx)

	var b *bridge.Bridge
	if f.complianceURL != "" {
		b = &bridge.Bridge{Eval: bridge.HTTP{BaseURL: f.complianceURL, Token: os.Getenv("COMPLIANCE_TOKEN")}, Log: log}
	}

	if f.serve != "" {
		if b != nil {
			ch, _ := coord.Subscribe()
			go b.Run(ctx, ch)
		}
		return serve(ctx, f, coord, repo, ref, cls, log)
	}
	return batch(ctx, f, coord, repo, b, log)
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func resolveDataDir(v string) string {
	switch v {
	case "none", "":
		return ""
	case "auto":
		for _, d := range []string{"data", "../data", "../../data"} {
			if st, err := os.Stat(filepath.Join(d, "raw")); err == nil && st.IsDir() {
				return d
			}
		}
		return ""
	}
	return v
}

func batch(ctx context.Context, f *flags, coord *ingest.Coordinator, repo store.JourneyRepository, b *bridge.Bridge, log *slog.Logger) error {
	var targets []string
	for _, t := range append(strings.Split(f.targets, ","), flag.Args()...) {
		if t = strings.TrimSpace(t); t != "" {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return errors.New("no targets: pass IPs/hostnames as arguments, use -targets, or run with -serve")
	}

	var w io.Writer = os.Stdout
	if f.out != "-" {
		fh, err := os.Create(f.out)
		if err != nil {
			return err
		}
		defer fh.Close()
		w = fh
	}
	enc := json.NewEncoder(w)
	if f.pretty {
		enc.SetIndent("", "  ")
	}

	opts := traceroute.Options{Protocol: traceroute.Protocol(f.protocol), FirstTTL: f.firstTTL, MaxHops: f.maxHops, ProbesPerHop: f.probes, Timeout: f.timeout}
	var jobs []ingest.Job
	for _, t := range targets {
		j, err := coord.Submit(ingest.Request{Target: t, IPVersion: f.ipVersion, PolicyID: f.policy, AccessType: f.access, Options: opts,
			DataClassifications: splitList(f.classes), App: f.app, SNI: f.sni, Port: f.port})
		if err != nil {
			return fmt.Errorf("submit %s: %w", t, err)
		}
		jobs = append(jobs, j)
	}

	failed := 0
	for _, j := range jobs {
		done, _ := coord.Wait(ctx, j.ID)
		if done.Status != ingest.StatusDone {
			failed++
			log.Error("trace failed", "target", j.Request.Target, "err", done.Error)
			continue
		}
		if done.Warning != "" {
			log.Warn("trace incomplete", "target", j.Request.Target, "warning", done.Warning)
		}
		journey, err := repo.Get(ctx, done.JourneyID)
		if err != nil {
			return err
		}
		if err := enc.Encode(journey); err != nil {
			return err
		}
		if b != nil {
			b.Handle(ctx, journey)
		}
	}

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = coord.Shutdown(sctx)
	if failed == len(jobs) {
		return fmt.Errorf("all %d traces failed", failed)
	}
	return nil
}

func serve(ctx context.Context, f *flags, coord *ingest.Coordinator, repo store.JourneyRepository, ref *store.MemoryReference, cls *classify.Classifier, log *slog.Logger) error {
	admins, err := capi.ParseTokens(os.Getenv("INGEST_ADMIN_TOKENS"))
	if err != nil {
		return fmt.Errorf("INGEST_ADMIN_TOKENS: %w", err)
	}
	if len(admins) == 0 {
		log.Warn("INGEST_ADMIN_TOKENS not set: classification rules are read-only over the API")
	}
	srv := &http.Server{
		Addr:              f.serve,
		Handler:           (&api.Server{Coord: coord, Repo: repo, Ref: ref, Stats: ref.Stats, Classifier: cls, AdminTokens: admins}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http api listening", "addr", f.serve)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return coord.Shutdown(sctx)
}
