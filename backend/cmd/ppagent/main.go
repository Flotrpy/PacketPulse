// Command ppagent runs traceroutes from your own computer for PacketPulse.
//
// It finds your public IP and approximate location, long-polls the server
// for trace jobs (the "Trace from my computer" box on the globe), traces
// each target from your network, and sends the raw result back. The server
// enriches, classifies and checks it, and the globe draws the path from
// where you are.
//
//	sudo PACKETPULSE_AGENT_TOKEN=<token> ./ppagent -server https://packetpulse-test.onrender.com
//
// One-off traces without the globe:
//
//	sudo PACKETPULSE_AGENT_TOKEN=<token> ./ppagent -server https://... example.com 1.1.1.1
//
// Traceroute needs raw ICMP sockets: run with sudo (macOS/Linux) or from an
// Administrator terminal (Windows).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/agenthub"
	"github.com/flotrpy/packetpulse/backend/internal/ingest"
	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

const version = "1.0.0"

type agent struct {
	server string
	token  string
	info   agenthub.Agent
	tracer *traceroute.Engine
	opts   traceroute.Options
	http   *http.Client
	log    *slog.Logger
}

func main() {
	host, _ := os.Hostname()
	server := flag.String("server", "https://packetpulse-test.onrender.com", "PacketPulse server URL")
	id := flag.String("id", host, "agent name shown on the globe")
	protocol := flag.String("protocol", "icmp", "default probe protocol: icmp or udp")
	maxHops := flag.Int("max-hops", 30, "default maximum TTL")
	pps := flag.Int("pps", 100, "probe rate limit (packets/s)")
	noGeo := flag.Bool("no-geolocate", false, "don't look up this computer's public IP location (ipinfo.io)")
	lat := flag.Float64("lat", 0, "your latitude (overrides IP geolocation)")
	lon := flag.Float64("lon", 0, "your longitude")
	country := flag.String("country", "", "your ISO2 country (required with -lat/-lon)")
	city := flag.String("city", "", "your city")
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Some Windows shells pass the program's own path again as the first
	// argument; it would stop flag parsing and be traced as a "target".
	args, dropped := withoutSelf(os.Args[1:])
	for _, d := range dropped {
		log.Debug("ignoring argument that is this program's own path", "arg", d)
	}
	flag.CommandLine.Parse(args)

	token := os.Getenv("PACKETPULSE_AGENT_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "ppagent: set PACKETPULSE_AGENT_TOKEN to the server's agent token")
		os.Exit(2)
	}
	a := &agent{
		server: strings.TrimRight(*server, "/"), token: token,
		tracer: traceroute.NewEngine(*pps),
		opts:   traceroute.Options{Protocol: traceroute.Protocol(*protocol), MaxHops: *maxHops},
		http:   &http.Client{Timeout: 40 * time.Second},
		log:    log,
		info:   agenthub.Agent{ID: *id, OS: runtime.GOOS + "/" + runtime.GOARCH, Version: version},
	}
	defer a.tracer.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a.info.Tracing = canTrace()
	if !a.info.Tracing {
		log.Error("cannot open raw ICMP sockets: run with sudo (macOS/Linux) or as Administrator (Windows)")
	}
	switch {
	case *country != "":
		a.info.Location = &model.Location{Lat: *lat, Lon: *lon, City: *city, CountryISO2: strings.ToUpper(*country), AccuracyKM: 5, Method: "configured", Confidence: 1}
	case !*noGeo:
		if err := a.geolocate(ctx); err != nil {
			log.Warn("could not geolocate this computer; paths will start at the first located hop", "err", err)
		}
	}
	if l := a.info.Location; l != nil {
		log.Info("this computer", "public_ip", a.info.PublicIP, "network", a.info.ASOrg, "city", l.City, "country", l.CountryISO2, "lat", l.Lat, "lon", l.Lon)
	}

	if targets := flag.Args(); len(targets) > 0 {
		for _, t := range targets {
			if !looksLikeTarget(t) {
				log.Error("not a hostname or IP address; skipping (flags must come before targets)", "arg", t)
				continue
			}
			if err := a.oneOff(ctx, t); err != nil {
				log.Error("trace failed", "target", t, "err", err)
			}
		}
		return
	}
	log.Info("waiting for trace jobs from the globe", "server", a.server, "agent", a.info.ID)
	a.loop(ctx)
}

// loop long-polls for jobs until interrupted.
func (a *agent) loop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		job, err := a.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.Warn("poll failed; retrying", "err", err, "in", backoff)
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if job != nil {
			a.run(ctx, job)
		}
	}
}

func (a *agent) poll(ctx context.Context) (*agenthub.Job, error) {
	body, _ := json.Marshal(a.info)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.server+"/v1/agent/poll?wait=25", bytes.NewReader(body))
	a.auth(req)
	res, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var j agenthub.Job
		return &j, json.NewDecoder(res.Body).Decode(&j)
	case http.StatusUnauthorized:
		return nil, errors.New("server rejected the agent token (PACKETPULSE_AGENT_TOKEN)")
	default:
		b, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return nil, fmt.Errorf("%s: %s", res.Status, b)
	}
}

func (a *agent) run(ctx context.Context, j *agenthub.Job) {
	a.log.Info("tracing", "job", j.ID, "target", j.Target)
	opts := a.opts
	if j.Protocol != "" {
		opts.Protocol = traceroute.Protocol(j.Protocol)
	}
	if j.MaxHops > 0 {
		opts.MaxHops = j.MaxHops
	}
	raw, err := a.trace(ctx, j.Target, opts)
	res := agenthub.Result{JobID: j.ID, AgentID: a.info.ID, Raw: raw}
	if err != nil {
		res.Error = err.Error()
	}
	out, err := a.post(ctx, "/v1/agent/results", res)
	if err != nil {
		a.log.Error("could not send result", "job", j.ID, "err", err)
		return
	}
	a.log.Info("result sent", "job", j.ID, "target", j.Target, "server", strings.TrimSpace(string(out)))
}

// oneOff traces a target given on the command line: it queues a job for
// itself so the result is stored and shown like one from the globe.
func (a *agent) oneOff(ctx context.Context, target string) error {
	// Register (heartbeat without waiting) so the server accepts the job.
	body, _ := json.Marshal(a.info)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.server+"/v1/agent/poll?wait=0", bytes.NewReader(body))
	a.auth(req)
	if res, err := a.http.Do(req); err != nil {
		return err
	} else {
		res.Body.Close()
	}
	out, err := a.post(ctx, "/v1/agent/jobs", agenthub.JobRequest{Target: target, AgentID: a.info.ID})
	if err != nil {
		return fmt.Errorf("queue job (the agent token must also be a trace token): %w", err)
	}
	var j agenthub.Job
	if err := json.Unmarshal(out, &j); err != nil {
		return err
	}
	for ctx.Err() == nil {
		job, err := a.poll(ctx)
		if err != nil {
			return err
		}
		if job != nil {
			a.run(ctx, job)
			if job.ID == j.ID {
				return nil
			}
		}
	}
	return ctx.Err()
}

func (a *agent) trace(ctx context.Context, target string, opts traceroute.Options) (*traceroute.RawTrace, error) {
	if !a.info.Tracing {
		return nil, errors.New("agent cannot open raw sockets (run it with sudo / as Administrator)")
	}
	addr, err := netip.ParseAddr(target)
	if err != nil {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		addrs, rerr := net.DefaultResolver.LookupNetIP(rctx, "ip4", target)
		cancel()
		if rerr != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("resolve %s: %v", target, rerr)
		}
		addr = addrs[0]
	}
	addr = addr.Unmap()
	if !ingest.IsPublic(addr) {
		return nil, fmt.Errorf("%s resolves to %s, which is not a public address", target, addr)
	}
	tctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	raw, err := a.tracer.Trace(tctx, addr, opts)
	if err != nil && (raw == nil || len(raw.Probes) == 0) {
		return nil, err
	}
	return raw, nil
}

// geolocate finds this computer's public IP and its approximate location.
func (a *agent) geolocate(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://ipinfo.io/json", nil)
	req.Header.Set("Accept", "application/json")
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var v struct{ IP, City, Region, Country, Loc, Org string }
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return err
	}
	lat, lon, ok := strings.Cut(v.Loc, ",")
	la, err1 := strconv.ParseFloat(lat, 64)
	lo, err2 := strconv.ParseFloat(lon, 64)
	if !ok || err1 != nil || err2 != nil || v.Country == "" {
		return fmt.Errorf("unexpected ipinfo answer %+v", v)
	}
	a.info.PublicIP, a.info.ASOrg = v.IP, v.Org
	a.info.Location = &model.Location{Lat: la, Lon: lo, City: v.City, CountryISO2: strings.ToUpper(v.Country),
		AccuracyKM: 25, Method: "prefix_db", Confidence: 0.7}
	return nil
}

func (a *agent) post(ctx context.Context, path string, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.server+path, bytes.NewReader(body))
	a.auth(req)
	res, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return out, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (a *agent) auth(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+a.token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "ppagent/"+version)
}

// withoutSelf removes arguments that name this executable.
func withoutSelf(args []string) (kept, dropped []string) {
	self, _ := os.Executable()
	selfInfo, err := os.Stat(self)
	for _, a := range args {
		if a == os.Args[0] || a == self {
			dropped = append(dropped, a)
			continue
		}
		if err == nil && strings.ContainsAny(a, `/\`) {
			if fi, err := os.Stat(a); err == nil && os.SameFile(fi, selfInfo) {
				dropped = append(dropped, a)
				continue
			}
		}
		kept = append(kept, a)
	}
	return kept, dropped
}

// looksLikeTarget accepts IP addresses and DNS names, not paths or URLs.
func looksLikeTarget(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	if s == "" || len(s) > 253 || strings.ContainsAny(s, `/\:?#@ `) {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if label == "" || len(label) > 63 {
			return false
		}
	}
	return true
}

func canTrace() bool {
	c, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
