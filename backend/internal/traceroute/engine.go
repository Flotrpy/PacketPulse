package traceroute

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// Engine is the native Tracer. Create one per process and share it: all
// concurrent traces reuse its raw listeners.
type Engine struct {
	mu      sync.Mutex
	v4, v6  *listener
	ids     *idAllocator
	limiter *rateLimiter
	closed  bool
}

// NewEngine returns an Engine. maxPPS caps probes per second across all
// traces (0 = unlimited); keep it modest to avoid router ICMP rate limits.
func NewEngine(maxPPS int) *Engine {
	return &Engine{ids: newIDAllocator(), limiter: newRateLimiter(maxPPS)}
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	var errs []error
	for _, l := range []*listener{e.v4, e.v6} {
		if l != nil {
			errs = append(errs, l.close())
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) listenerFor(v6 bool) (*listener, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("engine closed")
	}
	slot := &e.v4
	if v6 {
		slot = &e.v6
	}
	if *slot == nil {
		l, err := openListener(v6)
		if err != nil {
			return nil, err
		}
		*slot = l
	}
	return *slot, nil
}

// Trace runs one traceroute. Round 0 sends one probe to every TTL at once
// (paced by SendInterval) to find the destination distance; the remaining
// probes are then sent only up to that distance.
func (e *Engine) Trace(ctx context.Context, target netip.Addr, opts Options) (*RawTrace, error) {
	opts = opts.WithDefaults()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	target = target.Unmap()
	if !target.IsValid() || target.IsUnspecified() || target.IsMulticast() {
		return nil, fmt.Errorf("invalid target %s", target)
	}
	v6 := target.Is6()
	l, err := e.listenerFor(v6)
	if err != nil {
		return nil, err
	}

	s, err := e.newSender(l, target, opts)
	if err != nil {
		return nil, err
	}
	defer s.close()

	rt := &RawTrace{Target: target, Source: sourceAddrFor(target), Options: opts, StartedAt: time.Now()}

	ttls := make([]int, 0, opts.MaxHops)
	for t := opts.FirstTTL; t <= opts.MaxHops; t++ {
		ttls = append(ttls, t)
	}
	results, err := e.runBatch(ctx, l, s, ttls, 0)
	rt.Probes = append(rt.Probes, results...)
	if err == nil && opts.ProbesPerHop > 1 {
		limit := opts.MaxHops
		if d := destTTL(results); d > 0 {
			limit = d
		}
		var batch []int
		var attempts []int
		for a := 1; a < opts.ProbesPerHop; a++ {
			for t := opts.FirstTTL; t <= limit; t++ {
				batch = append(batch, t)
				attempts = append(attempts, a)
			}
		}
		var more []ProbeResult
		more, err = e.runBatchAttempts(ctx, l, s, batch, attempts)
		rt.Probes = append(rt.Probes, more...)
	}

	sort.Slice(rt.Probes, func(i, j int) bool {
		if rt.Probes[i].TTL != rt.Probes[j].TTL {
			return rt.Probes[i].TTL < rt.Probes[j].TTL
		}
		return rt.Probes[i].Attempt < rt.Probes[j].Attempt
	})
	rt.DestinationTTL = destTTL(rt.Probes)
	rt.Reached = rt.DestinationTTL > 0
	rt.FinishedAt = time.Now()
	return rt, err
}

func destTTL(probes []ProbeResult) int {
	best := 0
	for _, p := range probes {
		if p.Responded && p.Kind.FromTarget() && (best == 0 || p.TTL < best) {
			best = p.TTL
		}
	}
	return best
}

func (e *Engine) runBatch(ctx context.Context, l *listener, s *sender, ttls []int, attempt int) ([]ProbeResult, error) {
	attempts := make([]int, len(ttls))
	for i := range attempts {
		attempts[i] = attempt
	}
	return e.runBatchAttempts(ctx, l, s, ttls, attempts)
}

// runBatchAttempts sends all probes (paced) and waits for every reply or
// timeout concurrently.
func (e *Engine) runBatchAttempts(ctx context.Context, l *listener, s *sender, ttls, attempts []int) ([]ProbeResult, error) {
	results := make([]ProbeResult, len(ttls))
	var wg sync.WaitGroup
	var sendErr error

	for i, ttl := range ttls {
		results[i] = ProbeResult{TTL: ttl, Attempt: attempts[i]}
		if err := e.limiter.wait(ctx); err != nil {
			sendErr = err
			break
		}
		seq := s.nextSeq()
		key := s.key(seq)
		ch, err := l.register(key)
		if err != nil {
			sendErr = err
			break
		}
		sentAt := time.Now()
		if err := s.send(ttl, seq); err != nil {
			l.unregister(key)
			sendErr = fmt.Errorf("send ttl=%d: %w", ttl, err)
			break
		}
		results[i].SentAt = sentAt

		wg.Add(1)
		go func(i int, key probeKey, ch <-chan inbound, sentAt time.Time) {
			defer wg.Done()
			defer l.unregister(key)
			timer := time.NewTimer(s.opts.Timeout)
			defer timer.Stop()
			select {
			case in := <-ch:
				results[i].Responded = true
				results[i].RTT = in.at.Sub(sentAt)
				results[i].From = in.From
				results[i].Kind = in.Kind
				results[i].Code = in.Code
				results[i].MPLSLabels = in.MPLSLabels
			case <-timer.C:
			case <-ctx.Done():
			}
		}(i, key, ch, sentAt)

		if s.opts.SendInterval > 0 && i < len(ttls)-1 {
			select {
			case <-time.After(s.opts.SendInterval):
			case <-ctx.Done():
				sendErr = ctx.Err()
			}
			if sendErr != nil {
				break
			}
		}
	}
	wg.Wait()
	if sendErr == nil {
		sendErr = ctx.Err()
	}
	return results, sendErr
}

// ---------------- listener (shared raw socket + demultiplexer) ----------------

type inbound struct {
	reply
	at time.Time
}

type listener struct {
	v6      bool
	conn    *icmp.PacketConn
	sendMu  sync.Mutex // serialises SetTTL+WriteTo on the shared raw socket
	pmu     sync.Mutex
	pending map[probeKey]chan inbound
	done    chan struct{}
}

func openListener(v6 bool) (*listener, error) {
	network, addr := "ip4:icmp", "0.0.0.0"
	if v6 {
		network, addr = "ip6:ipv6-icmp", "::"
	}
	c, err := icmp.ListenPacket(network, addr)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, ErrPermission
		}
		return nil, fmt.Errorf("open %s listener: %w", network, err)
	}
	l := &listener{v6: v6, conn: c, pending: map[probeKey]chan inbound{}, done: make(chan struct{})}
	go l.readLoop()
	return l, nil
}

func (l *listener) readLoop() {
	buf := make([]byte, 9216)
	for {
		n, peer, err := l.conn.ReadFrom(buf)
		at := time.Now()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		ipa, ok := peer.(*net.IPAddr)
		if !ok {
			continue
		}
		from, ok := netip.AddrFromSlice(ipa.IP)
		if !ok {
			continue
		}
		r, err := parseReply(l.v6, buf[:n], from)
		if err != nil {
			continue
		}
		l.pmu.Lock()
		ch, ok := l.pending[r.Key]
		if ok {
			delete(l.pending, r.Key) // first answer wins
		}
		l.pmu.Unlock()
		if ok {
			ch <- inbound{reply: r, at: at} // buffered, never blocks
		}
	}
}

func (l *listener) register(k probeKey) (<-chan inbound, error) {
	l.pmu.Lock()
	defer l.pmu.Unlock()
	if _, dup := l.pending[k]; dup {
		return nil, fmt.Errorf("probe key collision %+v", k)
	}
	ch := make(chan inbound, 1)
	l.pending[k] = ch
	return ch, nil
}

func (l *listener) unregister(k probeKey) {
	l.pmu.Lock()
	delete(l.pending, k)
	l.pmu.Unlock()
}

func (l *listener) close() error {
	close(l.done)
	return l.conn.Close()
}

// ---------------- sender (per trace) ----------------

type sender struct {
	opts   Options
	target netip.Addr
	l      *listener
	seq    uint16
	seqMu  sync.Mutex

	// ICMP
	id      uint16
	release func()

	// UDP
	udp     *net.UDPConn
	srcPort uint16
	udpMu   sync.Mutex
	p4      *ipv4.PacketConn
	p6      *ipv6.PacketConn
}

func (e *Engine) newSender(l *listener, target netip.Addr, opts Options) (*sender, error) {
	s := &sender{opts: opts, target: target, l: l}
	switch opts.Protocol {
	case ICMP:
		id, release, err := e.ids.acquire()
		if err != nil {
			return nil, err
		}
		s.id, s.release = id, release
	case UDP:
		network := "udp4"
		if target.Is6() {
			network = "udp6"
		}
		c, err := net.ListenUDP(network, nil)
		if err != nil {
			return nil, fmt.Errorf("open udp socket: %w", err)
		}
		s.udp = c
		s.srcPort = uint16(c.LocalAddr().(*net.UDPAddr).Port)
		if target.Is6() {
			s.p6 = ipv6.NewPacketConn(c)
		} else {
			s.p4 = ipv4.NewPacketConn(c)
		}
	}
	return s, nil
}

func (s *sender) nextSeq() uint16 {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	s.seq++
	return s.seq
}

func (s *sender) key(seq uint16) probeKey {
	if s.opts.Protocol == UDP {
		return probeKey{Proto: UDP, A: s.srcPort, B: uint16(s.opts.DstPortBase) + seq - 1, Dst: s.target}
	}
	return probeKey{Proto: ICMP, A: s.id, B: seq, Dst: s.target}
}

func (s *sender) send(ttl int, seq uint16) error {
	if s.opts.Protocol == UDP {
		port := s.opts.DstPortBase + int(seq) - 1
		payload := make([]byte, s.opts.PayloadSize)
		s.udpMu.Lock()
		defer s.udpMu.Unlock()
		if s.p6 != nil {
			if err := s.p6.SetHopLimit(ttl); err != nil {
				return err
			}
		} else if err := s.p4.SetTTL(ttl); err != nil {
			return err
		}
		_, err := s.udp.WriteToUDPAddrPort(payload, netip.AddrPortFrom(s.target, uint16(port)))
		return err
	}

	b, err := buildEcho(s.target.Is6(), s.id, seq, s.opts.PayloadSize)
	if err != nil {
		return err
	}
	dst := &net.IPAddr{IP: s.target.AsSlice()}
	s.l.sendMu.Lock()
	defer s.l.sendMu.Unlock()
	if s.l.v6 {
		if err := s.l.conn.IPv6PacketConn().SetHopLimit(ttl); err != nil {
			return err
		}
	} else if err := s.l.conn.IPv4PacketConn().SetTTL(ttl); err != nil {
		return err
	}
	_, err = s.l.conn.WriteTo(b, dst)
	return err
}

func (s *sender) close() {
	if s.release != nil {
		s.release()
	}
	if s.udp != nil {
		s.udp.Close()
	}
}

// sourceAddrFor asks the kernel which local address routes to target. A UDP
// "connect" sends no packets.
func sourceAddrFor(target netip.Addr) netip.Addr {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(target, 33434)))
	if err != nil {
		return netip.Addr{}
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
}

// ---------------- helpers ----------------

// idAllocator hands out unique ICMP echo identifiers to concurrent traces.
type idAllocator struct {
	mu   sync.Mutex
	next uint16
	used map[uint16]bool
}

func newIDAllocator() *idAllocator {
	return &idAllocator{next: uint16(rand.UintN(65535) + 1), used: map[uint16]bool{}}
}

func (a *idAllocator) acquire() (uint16, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < 65535; i++ {
		id := a.next
		a.next++
		if a.next == 0 {
			a.next = 1
		}
		if id != 0 && !a.used[id] {
			a.used[id] = true
			return id, func() { a.mu.Lock(); delete(a.used, id); a.mu.Unlock() }, nil
		}
	}
	return 0, nil, errors.New("no free ICMP identifiers")
}

// rateLimiter spaces probe sends to at most pps per second across the engine.
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newRateLimiter(pps int) *rateLimiter {
	if pps <= 0 {
		return &rateLimiter{}
	}
	return &rateLimiter{interval: time.Second / time.Duration(pps)}
}

func (r *rateLimiter) wait(ctx context.Context) error {
	if r.interval == 0 {
		return ctx.Err()
	}
	r.mu.Lock()
	now := time.Now()
	if r.next.Before(now) {
		r.next = now
	}
	at := r.next
	r.next = r.next.Add(r.interval)
	r.mu.Unlock()
	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}
