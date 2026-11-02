package traceroute

import (
	"fmt"
	"net/netip"
	"sort"
	"time"
)

// HopSummary aggregates the probes sent with one TTL.
type HopSummary struct {
	TTL        int
	Addr       netip.Addr      // most frequent responder; zero if nothing answered
	Alternates []netip.Addr    // other responders (ECMP / load balancing)
	RTTs       []time.Duration // replies from Addr, in attempt order
	Sent       int
	Received   int // replies from any responder
	Kind       ReplyKind
	MPLSLabels []int
}

// Hops groups probes by TTL from FirstTTL up to the destination (if reached)
// or the last TTL that got any reply. Trailing silent TTLs are dropped.
func (rt *RawTrace) Hops() []HopSummary {
	byTTL := map[int][]ProbeResult{}
	last := 0
	for _, p := range rt.Probes {
		byTTL[p.TTL] = append(byTTL[p.TTL], p)
		if p.Responded && p.TTL > last {
			last = p.TTL
		}
	}
	if rt.Reached {
		last = rt.DestinationTTL
	}
	var out []HopSummary
	for ttl := rt.Options.FirstTTL; ttl <= last; ttl++ {
		out = append(out, summarize(ttl, byTTL[ttl]))
	}
	return out
}

func summarize(ttl int, probes []ProbeResult) HopSummary {
	h := HopSummary{TTL: ttl, Sent: len(probes)}
	counts := map[netip.Addr]int{}
	var order []netip.Addr
	for _, p := range probes {
		if !p.Responded {
			continue
		}
		h.Received++
		if counts[p.From] == 0 {
			order = append(order, p.From)
		}
		counts[p.From]++
	}
	if len(order) == 0 {
		return h
	}
	h.Addr = order[0]
	for _, a := range order[1:] {
		if counts[a] > counts[h.Addr] {
			h.Addr = a
		}
	}
	for _, a := range order {
		if a != h.Addr {
			h.Alternates = append(h.Alternates, a)
		}
	}
	sorted := append([]ProbeResult(nil), probes...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].Attempt < sorted[b].Attempt })
	seen := map[int]bool{}
	for _, p := range sorted {
		if !p.Responded || p.From != h.Addr {
			continue
		}
		h.RTTs = append(h.RTTs, p.RTT)
		if h.Kind == ReplyNone {
			h.Kind = p.Kind
		}
		for _, l := range p.MPLSLabels {
			if !seen[l] {
				seen[l] = true
				h.MPLSLabels = append(h.MPLSLabels, l)
			}
		}
	}
	return h
}

// MinRTT returns the smallest RTT, or 0 when the hop never answered.
func (h HopSummary) MinRTT() time.Duration {
	var m time.Duration
	for i, r := range h.RTTs {
		if i == 0 || r < m {
			m = r
		}
	}
	return m
}

// MarshalText renders ReplyKind as its name in JSON.
func (k ReplyKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

func (k *ReplyKind) UnmarshalText(b []byte) error {
	for _, c := range []ReplyKind{ReplyNone, ReplyTimeExceeded, ReplyUnreachable, ReplyEcho, ReplyDestination} {
		if c.String() == string(b) {
			*k = c
			return nil
		}
	}
	return fmt.Errorf("unknown reply kind %q", b)
}
