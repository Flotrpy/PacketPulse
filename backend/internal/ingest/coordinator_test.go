package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/pipeline"
	"github.com/flotrpy/packetpulse/backend/internal/store"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

// scriptedTracer returns a two-hop trace after an optional delay or gate,
// and records peak concurrency.
type scriptedTracer struct {
	delay   time.Duration
	gate    chan struct{} // if set, Trace blocks until closed
	failFor string        // target that returns an error with no probes
	partial string        // target that returns probes plus a deadline error
	active  atomic.Int32
	peak    atomic.Int32
	calls   atomic.Int32
}

func (s *scriptedTracer) Trace(ctx context.Context, target netip.Addr, opts traceroute.Options) (*traceroute.RawTrace, error) {
	s.calls.Add(1)
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	time.Sleep(s.delay)
	if target.String() == s.failFor {
		return nil, errors.New("network unreachable")
	}
	opts = opts.WithDefaults()
	now := time.Now()
	rt := &traceroute.RawTrace{Target: target, Source: netip.MustParseAddr("10.0.0.5"), Options: opts, StartedAt: now, FinishedAt: now.Add(time.Millisecond)}
	rt.Probes = []traceroute.ProbeResult{
		{TTL: 1, Responded: true, From: netip.MustParseAddr("10.0.0.1"), Kind: traceroute.ReplyTimeExceeded, RTT: time.Millisecond},
		{TTL: 2, Responded: true, From: target, Kind: traceroute.ReplyEcho, RTT: 5 * time.Millisecond},
	}
	rt.Reached, rt.DestinationTTL = true, 2
	if target.String() == s.partial {
		return rt, context.DeadlineExceeded
	}
	return rt, nil
}

func (s *scriptedTracer) Close() error { return nil }

func newTestCoordinator(t *testing.T, tr traceroute.Tracer, cfg Config) (*Coordinator, *store.MemoryRepository) {
	t.Helper()
	ref := store.NewSeededReference()
	repo := store.NewMemoryRepository(100)
	c := New(cfg, tr, pipeline.Default(ref, repo, nil, nil), ref, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx)
	return c, repo
}

func TestConcurrentJobsRespectWorkerLimit(t *testing.T) {
	tr := &scriptedTracer{delay: 30 * time.Millisecond}
	c, repo := newTestCoordinator(t, tr, Config{Workers: 4, QueueSize: 64})
	sub, unsub := c.Subscribe()
	defer unsub()

	var ids []string
	for i := 1; i <= 16; i++ {
		j, err := c.Submit(Request{Target: netip.AddrFrom4([4]byte{8, 8, 4, byte(i)}).String(), PolicyID: "us-doj-bulk"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range ids {
		j, _ := c.Wait(ctx, id)
		if j.Status != StatusDone || j.JourneyID == "" {
			t.Fatalf("job %s: %+v", id, j)
		}
	}
	if p := tr.peak.Load(); p > 4 || p < 2 {
		t.Fatalf("peak concurrency %d, want 2..4", p)
	}
	if n, _ := repo.Count(ctx); n != 16 {
		t.Fatalf("stored %d journeys", n)
	}
	got := 0
	for got < 16 {
		select {
		case <-sub:
			got++
		case <-ctx.Done():
			t.Fatalf("subscriber received %d/16", got)
		}
	}
	if st := c.Stats(); st.Completed != 16 || st.Failed != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestCoalescingAndQueueFull(t *testing.T) {
	tr := &scriptedTracer{gate: make(chan struct{})}
	c, _ := newTestCoordinator(t, tr, Config{Workers: 1, QueueSize: 1})

	first, err := c.Submit(Request{Target: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the worker has picked it up so the queue slot is free.
	deadline := time.Now().Add(2 * time.Second)
	for tr.active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	dup, err := c.Submit(Request{Target: "1.1.1.1"})
	if err != nil || !dup.Coalesced || dup.ID != first.ID {
		t.Fatalf("duplicate not coalesced: %+v err=%v", dup, err)
	}
	if _, err := c.Submit(Request{Target: "9.9.9.9"}); err != nil {
		t.Fatalf("queue slot should be free: %v", err)
	}
	if _, err := c.Submit(Request{Target: "8.8.8.8"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}
	close(tr.gate)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if j, _ := c.Wait(ctx, first.ID); j.Status != StatusDone {
		t.Fatalf("first job %+v", j)
	}
	if tr.calls.Load() > 2 {
		t.Fatalf("coalesced request was traced twice (%d calls)", tr.calls.Load())
	}
}

func TestFailuresAndPartialTraces(t *testing.T) {
	tr := &scriptedTracer{failFor: "203.0.113.1", partial: "198.51.100.7"}
	c, _ := newTestCoordinator(t, tr, Config{Workers: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	bad, _ := c.Submit(Request{Target: "203.0.113.1"})
	part, _ := c.Submit(Request{Target: "198.51.100.7"})
	if j, _ := c.Wait(ctx, bad.ID); j.Status != StatusFailed || j.Error == "" {
		t.Fatalf("failed job %+v", j)
	}
	if j, _ := c.Wait(ctx, part.ID); j.Status != StatusDone || j.Warning == "" {
		t.Fatalf("partial job %+v", j)
	}
}

func TestPublicTargetsOnly(t *testing.T) {
	c, _ := newTestCoordinator(t, &scriptedTracer{}, Config{Workers: 2, PublicTargetsOnly: true})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for target, ok := range map[string]bool{"8.8.8.8": true, "10.1.2.3": false, "127.0.0.1": false, "100.64.1.1": false, "169.254.169.254": false, "::1": false, "fd00::1": false} {
		j, err := c.Submit(Request{Target: target})
		if err != nil {
			t.Fatal(err)
		}
		done, _ := c.Wait(ctx, j.ID)
		if (done.Status == StatusDone) != ok {
			t.Errorf("%s: status %s (%s)", target, done.Status, done.Error)
		}
	}
}

func TestValidation(t *testing.T) {
	c, _ := newTestCoordinator(t, &scriptedTracer{}, Config{})
	bad := []Request{
		{},
		{Target: "8.8.8.8", PolicyID: "no-such-policy"},
		{Target: "8.8.8.8", AccessType: "carrier-pigeon"},
		{Target: "8.8.8.8", IPVersion: 5},
		{Target: "8.8.8.8", Options: traceroute.Options{MaxHops: 99}},
		{Target: "8.8.8.8", DataClassifications: []string{"x"}},
		{Target: "8.8.8.8", Port: -1},
	}
	for _, r := range bad {
		if _, err := c.Submit(r); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%+v: want ErrBadRequest, got %v", r, err)
		}
	}
}

func TestShutdownDrainsAndRejects(t *testing.T) {
	tr := &scriptedTracer{delay: 20 * time.Millisecond}
	c, repo := newTestCoordinator(t, tr, Config{Workers: 2})
	for i := 0; i < 6; i++ {
		if _, err := c.Submit(Request{Target: netip.AddrFrom4([4]byte{1, 0, 0, byte(i + 1)}).String()}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := repo.Count(ctx); n != 6 {
		t.Fatalf("queued jobs not drained: %d stored", n)
	}
	if _, err := c.Submit(Request{Target: "1.1.1.1"}); !errors.Is(err, ErrStopped) {
		t.Fatalf("want ErrStopped, got %v", err)
	}
}
