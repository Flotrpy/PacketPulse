package traceroute

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// TestEngineLoopback sends real probes to 127.0.0.1 (no external network
// needed). It is skipped without raw-socket privileges.
func TestEngineLoopback(t *testing.T) {
	e := NewEngine(0)
	defer e.Close()
	lo := netip.MustParseAddr("127.0.0.1")

	for _, proto := range []Protocol{ICMP, UDP} {
		rt, err := e.Trace(context.Background(), lo, Options{Protocol: proto, MaxHops: 4, ProbesPerHop: 2, Timeout: 500 * time.Millisecond})
		if errors.Is(err, ErrPermission) {
			t.Skip("raw sockets unavailable:", err)
		}
		if err != nil {
			t.Fatalf("%s: %v", proto, err)
		}
		if !rt.Reached || rt.DestinationTTL != 1 {
			t.Fatalf("%s: reached=%v destTTL=%d", proto, rt.Reached, rt.DestinationTTL)
		}
		// Round 0 probes TTL 1..4; extra attempts only up to the destination.
		if len(rt.Probes) != 4+1 {
			t.Fatalf("%s: %d probes, want 5", proto, len(rt.Probes))
		}
		p := rt.Probes[0]
		if !p.Responded || p.From != lo || !p.Kind.FromTarget() || p.RTT <= 0 {
			t.Fatalf("%s: first probe %+v", proto, p)
		}
	}
}

// TestEngineConcurrentTraces runs many traces at once through one engine and
// checks every probe is matched to its own trace.
func TestEngineConcurrentTraces(t *testing.T) {
	e := NewEngine(0)
	defer e.Close()
	lo := netip.MustParseAddr("127.0.0.1")
	if _, err := e.Trace(context.Background(), lo, Options{MaxHops: 1, ProbesPerHop: 1, Timeout: 200 * time.Millisecond}); errors.Is(err, ErrPermission) {
		t.Skip("raw sockets unavailable")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			proto := ICMP
			if i%2 == 1 {
				proto = UDP
			}
			rt, err := e.Trace(context.Background(), lo, Options{Protocol: proto, MaxHops: 3, ProbesPerHop: 3, Timeout: time.Second})
			if err != nil {
				errs <- err
				return
			}
			for _, p := range rt.Probes {
				if !p.Responded {
					errs <- errors.New("unanswered loopback probe")
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestEngineCancel(t *testing.T) {
	e := NewEngine(0)
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	// TEST-NET-3 is unroutable; every probe would wait for the full timeout.
	start := time.Now()
	_, err := e.Trace(ctx, netip.MustParseAddr("203.0.113.9"), Options{MaxHops: 5, ProbesPerHop: 1, Timeout: 5 * time.Second})
	if errors.Is(err, ErrPermission) {
		t.Skip("raw sockets unavailable")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancel not honoured: took %s", time.Since(start))
	}
}
