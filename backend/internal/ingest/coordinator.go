// Package ingest coordinates trace jobs: it validates requests, queues them,
// runs traceroutes on a bounded worker pool, pushes each raw trace through the
// pipeline, and fans finished journeys out to subscribers.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/classify"
	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/pipeline"
	"github.com/flotrpy/packetpulse/backend/internal/store"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

var (
	ErrQueueFull  = errors.New("ingest queue is full")
	ErrStopped    = errors.New("coordinator is stopped")
	ErrUnknownJob = errors.New("unknown job")
	ErrBadRequest = errors.New("invalid request")
)

// Request is one trace to run.
type Request struct {
	Target     string             `json:"target"` // IP or hostname
	IPVersion  int                `json:"ip_version,omitempty"`
	PolicyID   string             `json:"policy_id,omitempty"`
	AccessType string             `json:"access_type,omitempty"`
	Options    traceroute.Options `json:"options"`

	// Data classification. Explicit labels win; otherwise the classifier's
	// rules match on the target and on the flow context below.
	DataClassifications []string `json:"data_classifications,omitempty"`
	App                 string   `json:"app,omitempty"`  // application the traffic belongs to
	SNI                 string   `json:"sni,omitempty"`  // TLS server name of the flow
	Port                int      `json:"port,omitempty"` // destination service port of the flow
}

type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Job is the externally visible state of a request.
type Job struct {
	ID          string    `json:"job_id"`
	Request     Request   `json:"request"`
	Status      Status    `json:"status"`
	JourneyID   string    `json:"journey_id,omitempty"`
	Error       string    `json:"error,omitempty"`
	Warning     string    `json:"warning,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	Coalesced   bool      `json:"coalesced,omitempty"`

	done chan struct{}
}

// HostResolver resolves target hostnames. *net.Resolver satisfies it.
type HostResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type Config struct {
	Workers      int           // concurrent traceroutes
	QueueSize    int           // pending jobs before Submit returns ErrQueueFull
	TraceTimeout time.Duration // hard cap per trace
	JobHistory   int           // finished jobs kept for status queries
	AgentID      string
	SourceLoc    *model.Location
	// PublicTargetsOnly refuses targets that resolve to private, loopback,
	// CGNAT, link-local or other non-global addresses (for internet-facing
	// deployments, so callers cannot map the host's internal network).
	PublicTargetsOnly bool
}

type Coordinator struct {
	cfg      Config
	tracer   traceroute.Tracer
	pipe     *pipeline.Pipeline
	ref      store.ReferenceStore
	resolver HostResolver
	log      *slog.Logger

	queue chan *Job
	wg    sync.WaitGroup

	mu       sync.Mutex
	jobs     map[string]*Job
	history  []string
	inflight map[string]*Job // coalescing key -> job
	stopped  bool

	subMu sync.Mutex
	subs  map[int]chan *model.Journey
	subID int

	stats struct{ submitted, coalesced, completed, failed, running atomic.Int64 }
}

func New(cfg Config, tracer traceroute.Tracer, pipe *pipeline.Pipeline, ref store.ReferenceStore, resolver HostResolver, log *slog.Logger) *Coordinator {
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.TraceTimeout <= 0 {
		cfg.TraceTimeout = 90 * time.Second
	}
	if cfg.JobHistory <= 0 {
		cfg.JobHistory = 5000
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if log == nil {
		log = slog.Default()
	}
	return &Coordinator{
		cfg: cfg, tracer: tracer, pipe: pipe, ref: ref, resolver: resolver, log: log,
		queue: make(chan *Job, cfg.QueueSize), jobs: map[string]*Job{}, inflight: map[string]*Job{},
		subs: map[int]chan *model.Journey{},
	}
}

// Start launches the worker pool. Workers stop when ctx is cancelled or
// Shutdown is called.
func (c *Coordinator) Start(ctx context.Context) {
	for i := 0; i < c.cfg.Workers; i++ {
		c.wg.Add(1)
		go c.worker(ctx, i)
	}
}

// Shutdown stops accepting work, lets queued jobs drain, and waits for
// workers (or ctx expiry).
func (c *Coordinator) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if !c.stopped {
		c.stopped = true
		close(c.queue)
	}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		c.subMu.Lock()
		for id, ch := range c.subs {
			close(ch)
			delete(c.subs, id)
		}
		c.subMu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Validate checks a request without queuing it.
func (c *Coordinator) Validate(req Request) error {
	if req.Target == "" {
		return fmt.Errorf("%w: target is required", ErrBadRequest)
	}
	if req.IPVersion != 0 && req.IPVersion != 4 && req.IPVersion != 6 {
		return fmt.Errorf("%w: ip_version must be 4 or 6", ErrBadRequest)
	}
	switch req.AccessType {
	case "", "wired", "wifi", "cellular", "unknown":
	default:
		return fmt.Errorf("%w: access_type must be wired, wifi, cellular or unknown", ErrBadRequest)
	}
	if len(req.DataClassifications) > 16 {
		return fmt.Errorf("%w: at most 16 data_classifications", ErrBadRequest)
	}
	for _, l := range classify.Normalize(req.DataClassifications) {
		if !classify.ValidLabel(l) {
			return fmt.Errorf("%w: data classification %q must be lower_snake_case (2-64 chars)", ErrBadRequest, l)
		}
	}
	if req.Port < 0 || req.Port > 65535 {
		return fmt.Errorf("%w: port must be 0-65535", ErrBadRequest)
	}
	if len(req.App) > 128 || len(req.SNI) > 253 {
		return fmt.Errorf("%w: app or sni too long", ErrBadRequest)
	}
	if req.PolicyID != "" {
		if _, ok := c.ref.Policy(req.PolicyID); !ok {
			return fmt.Errorf("%w: unknown policy %q", ErrBadRequest, req.PolicyID)
		}
	}
	if err := req.Options.WithDefaults().Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	return nil
}

func coalesceKey(r Request) string {
	o := r.Options.WithDefaults()
	// Requests that would be classified differently must not share a job.
	return fmt.Sprintf("%s|%d|%s|%s|%s|%d|%d|%d|%s|%s|%s|%d", r.Target, r.IPVersion, o.Protocol, r.PolicyID, r.AccessType,
		o.FirstTTL, o.MaxHops, o.ProbesPerHop, strings.Join(classify.Normalize(r.DataClassifications), ","), r.App, r.SNI, r.Port)
}

// Submit queues a request. An identical request that is already queued or
// running is coalesced: the existing job is returned instead of tracing twice.
func (c *Coordinator) Submit(req Request) (Job, error) {
	if err := c.Validate(req); err != nil {
		return Job{}, err
	}
	key := coalesceKey(req)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return Job{}, ErrStopped
	}
	c.stats.submitted.Add(1)
	if j, ok := c.inflight[key]; ok {
		c.stats.coalesced.Add(1)
		cp := *j
		cp.Coalesced = true
		return cp, nil
	}
	j := &Job{ID: newID(), Request: req, Status: StatusQueued, SubmittedAt: time.Now().UTC(), done: make(chan struct{})}
	select {
	case c.queue <- j:
	default:
		return Job{}, ErrQueueFull
	}
	c.jobs[j.ID] = j
	c.inflight[key] = j
	return *j, nil
}

// Job returns a snapshot of a job.
func (c *Coordinator) Job(id string) (Job, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j, ok := c.jobs[id]
	if !ok {
		return Job{}, ErrUnknownJob
	}
	return *j, nil
}

// Wait blocks until the job finishes or ctx ends.
func (c *Coordinator) Wait(ctx context.Context, id string) (Job, error) {
	c.mu.Lock()
	j, ok := c.jobs[id]
	c.mu.Unlock()
	if !ok {
		return Job{}, ErrUnknownJob
	}
	select {
	case <-j.done:
		return c.Job(id)
	case <-ctx.Done():
		return c.Job(id)
	}
}

// Subscribe returns a channel of finished journeys. Slow subscribers miss
// journeys rather than blocking ingestion (buffer of 64).
func (c *Coordinator) Subscribe() (<-chan *model.Journey, func()) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.subID++
	id := c.subID
	ch := make(chan *model.Journey, 64)
	c.subs[id] = ch
	return ch, func() {
		c.subMu.Lock()
		defer c.subMu.Unlock()
		if ch, ok := c.subs[id]; ok {
			close(ch)
			delete(c.subs, id)
		}
	}
}

type Stats struct {
	Submitted int64 `json:"submitted"`
	Coalesced int64 `json:"coalesced"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
	Running   int64 `json:"running"`
	Queued    int   `json:"queued"`
	Workers   int   `json:"workers"`
}

func (c *Coordinator) Stats() Stats {
	return Stats{
		Submitted: c.stats.submitted.Load(), Coalesced: c.stats.coalesced.Load(), Completed: c.stats.completed.Load(),
		Failed: c.stats.failed.Load(), Running: c.stats.running.Load(), Queued: len(c.queue), Workers: c.cfg.Workers,
	}
}

// ---------- worker ----------

func (c *Coordinator) worker(ctx context.Context, n int) {
	defer c.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-c.queue:
			if !ok {
				return
			}
			c.run(ctx, j)
		}
	}
}

func (c *Coordinator) run(ctx context.Context, j *Job) {
	c.update(j, func(j *Job) { j.Status, j.StartedAt = StatusRunning, time.Now().UTC() })
	c.stats.running.Add(1)
	defer c.stats.running.Add(-1)

	journey, warn, err := c.execute(ctx, j.Request)
	c.update(j, func(j *Job) {
		j.FinishedAt = time.Now().UTC()
		j.Warning = warn
		if err != nil {
			j.Status, j.Error = StatusFailed, err.Error()
		} else {
			j.Status, j.JourneyID = StatusDone, journey.JourneyID
		}
	})

	c.mu.Lock()
	delete(c.inflight, coalesceKey(j.Request))
	c.history = append(c.history, j.ID)
	for len(c.history) > c.cfg.JobHistory {
		delete(c.jobs, c.history[0])
		c.history = c.history[1:]
	}
	c.mu.Unlock()
	close(j.done)

	if err != nil {
		c.stats.failed.Add(1)
		c.log.Warn("trace failed", "job", j.ID, "target", j.Request.Target, "err", err)
		return
	}
	c.stats.completed.Add(1)
	c.log.Info("trace done", "job", j.ID, "target", j.Request.Target, "journey", journey.JourneyID,
		"hops", len(journey.Hops), "reached", journey.Metadata.ReachedDestination, "verdict", journey.Compliance.Verdict)
	c.publish(journey)
}

// execute resolves the target, traces it and runs the pipeline. A trace cut
// short (timeout/cancel) with some probes is still enriched and returned with
// a warning.
func (c *Coordinator) execute(ctx context.Context, req Request) (*model.Journey, string, error) {
	tctx, cancel := context.WithTimeout(ctx, c.cfg.TraceTimeout)
	defer cancel()

	addr, err := c.resolveTarget(tctx, req.Target, req.IPVersion)
	if err != nil {
		return nil, "", err
	}
	if c.cfg.PublicTargetsOnly && !isPublic(addr) {
		return nil, "", fmt.Errorf("target %s resolves to non-public address %s", req.Target, addr)
	}
	raw, err := c.tracer.Trace(tctx, addr, req.Options)
	warn := ""
	if err != nil {
		if raw == nil || len(raw.Probes) == 0 {
			return nil, "", fmt.Errorf("traceroute: %w", err)
		}
		warn = "partial trace: " + err.Error()
	}
	journey, err := c.pipe.Run(ctx, raw, pipeline.Request{
		Target: req.Target, PolicyID: req.PolicyID, AgentID: c.cfg.AgentID,
		AccessType: req.AccessType, SourceLocation: c.cfg.SourceLoc,
		Classifications: req.DataClassifications, App: req.App, SNI: req.SNI, Port: req.Port,
	})
	if err != nil {
		return nil, warn, fmt.Errorf("pipeline: %w", err)
	}
	return journey, warn, nil
}

func (c *Coordinator) resolveTarget(ctx context.Context, target string, ipv int) (netip.Addr, error) {
	if a, err := netip.ParseAddr(target); err == nil {
		a = a.Unmap()
		if ipv == 4 && !a.Is4() || ipv == 6 && !a.Is6() {
			return netip.Addr{}, fmt.Errorf("target %s is not IPv%d", target, ipv)
		}
		return a, nil
	}
	network := "ip"
	if ipv == 4 {
		network = "ip4"
	} else if ipv == 6 {
		network = "ip6"
	}
	addrs, err := c.resolver.LookupNetIP(ctx, network, target)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolve %s: %w", target, err)
	}
	// Prefer IPv4 when unspecified: wider ICMP visibility on most paths.
	for _, a := range addrs {
		if a.Unmap().Is4() || ipv == 6 {
			return a.Unmap(), nil
		}
	}
	if len(addrs) > 0 {
		return addrs[0].Unmap(), nil
	}
	return netip.Addr{}, fmt.Errorf("resolve %s: no addresses", target)
}

func (c *Coordinator) update(j *Job, f func(*Job)) {
	c.mu.Lock()
	f(j)
	c.mu.Unlock()
}

func (c *Coordinator) publish(j *model.Journey) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- j:
		default: // drop for slow subscriber
		}
	}
}

var idCounter atomic.Uint64

func newID() string {
	return fmt.Sprintf("job-%d-%06d", time.Now().UnixMilli(), idCounter.Add(1)%1_000_000)
}

// Non-global ranges that netip's IsGlobalUnicast/IsPrivate still accept.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

func isPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
