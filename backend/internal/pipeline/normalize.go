package pipeline

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

// Normalize builds journey metadata and one Hop per TTL from raw probes.
type Normalize struct{}

func (Normalize) Name() string   { return "normalize" }
func (Normalize) Required() bool { return true }

func (Normalize) Process(_ context.Context, s *State) error {
	raw := s.Raw
	if !raw.Target.IsValid() {
		return errors.New("trace has no target")
	}
	j := s.Journey
	j.JourneyID = newUUID()

	ipv := 4
	if raw.Target.Is6() {
		ipv = 6
	}
	access := s.Req.AccessType
	if access == "" {
		access = "unknown"
	}
	j.Metadata = model.Metadata{
		Timestamp:          raw.StartedAt.UTC(),
		CompletedAt:        raw.FinishedAt.UTC(),
		Protocol:           string(raw.Options.Protocol),
		IPVersion:          ipv,
		AgentID:            s.Req.AgentID,
		Source:             model.Endpoint{IP: raw.Source.String(), Access: access, Location: s.Req.SourceLocation},
		Destination:        model.Endpoint{IP: raw.Target.String()},
		ReachedDestination: raw.Reached,
		DurationMS:         ms(raw.FinishedAt.Sub(raw.StartedAt)),
		Probe: model.ProbeConfig{
			FirstTTL: raw.Options.FirstTTL, MaxHops: raw.Options.MaxHops,
			ProbesPerHop: raw.Options.ProbesPerHop, TimeoutMS: ms(raw.Options.Timeout),
		},
	}
	if !raw.Source.IsValid() {
		j.Metadata.Source.IP = ""
	}
	if raw.Options.Protocol == traceroute.UDP {
		j.Metadata.Probe.DstPortBase = raw.Options.DstPortBase
	}
	if s.Req.Target != "" && s.Req.Target != raw.Target.String() {
		j.Metadata.Destination.Hostname = s.Req.Target
	}

	byTTL := map[int][]traceroute.ProbeResult{}
	last := 0
	for _, p := range raw.Probes {
		byTTL[p.TTL] = append(byTTL[p.TTL], p)
		if p.Responded && p.TTL > last {
			last = p.TTL
		}
	}
	if raw.Reached {
		last = raw.DestinationTTL
	}

	j.Hops = make([]model.Hop, 0, last)
	for ttl := raw.Options.FirstTTL; ttl <= last; ttl++ {
		hop, addr, minMS := buildHop(ttl, byTTL[ttl], raw.Target)
		j.Hops = append(j.Hops, hop)
		s.hopAddr = append(s.hopAddr, addr)
		s.hopMinMS = append(s.hopMinMS, minMS)
	}
	return nil
}

func buildHop(ttl int, probes []traceroute.ProbeResult, target netip.Addr) (model.Hop, netip.Addr, float64) {
	h := model.Hop{HopNumber: ttl, ProbesSent: len(probes), Role: "unknown", AddressClass: "unknown", RTT: model.RTTStats{Samples: []float64{}}}
	counts := map[netip.Addr]int{}
	var order []netip.Addr
	for _, p := range probes {
		if !p.Responded {
			continue
		}
		h.ProbesReceived++
		if counts[p.From] == 0 {
			order = append(order, p.From)
		}
		counts[p.From]++
	}
	if len(probes) > 0 {
		h.LossPct = round1(100 * float64(len(probes)-h.ProbesReceived) / float64(len(probes)))
	}
	if len(order) == 0 {
		return h, netip.Addr{}, 0
	}
	// Most frequent responder is the hop; others are ECMP/load-balanced siblings.
	primary := order[0]
	for _, a := range order[1:] {
		if counts[a] > counts[primary] {
			primary = a
		}
	}
	for _, a := range order {
		if a != primary {
			h.AlternateIPs = append(h.AlternateIPs, a.String())
		}
	}
	h.Responded = true
	h.IP = primary.String()
	h.AddressClass = addressClass(primary)

	var samples []float64
	labels := map[int]bool{}
	sort.SliceStable(probes, func(a, b int) bool { return probes[a].Attempt < probes[b].Attempt })
	for _, p := range probes {
		if !p.Responded || p.From != primary {
			continue
		}
		samples = append(samples, round3(float64(p.RTT)/float64(time.Millisecond)))
		if h.ICMPType == "" {
			h.ICMPType = p.Kind.String()
		}
		for _, l := range p.MPLSLabels {
			if !labels[l] {
				labels[l] = true
				h.MPLSLabels = append(h.MPLSLabels, l)
			}
		}
	}
	h.RTT = rttStats(samples)
	if primary == target {
		h.Role = "destination"
	}
	return h, primary, h.RTT.Min
}

func rttStats(samples []float64) model.RTTStats {
	st := model.RTTStats{Samples: samples}
	if len(samples) == 0 {
		st.Samples = []float64{}
		return st
	}
	st.Min, st.Max = samples[0], samples[0]
	sum := 0.0
	for _, v := range samples {
		sum += v
		st.Min = math.Min(st.Min, v)
		st.Max = math.Max(st.Max, v)
	}
	st.Avg = sum / float64(len(samples))
	var sq, jit float64
	for i, v := range samples {
		sq += (v - st.Avg) * (v - st.Avg)
		if i > 0 {
			jit += math.Abs(v - samples[i-1])
		}
	}
	st.StdDev = round3(math.Sqrt(sq / float64(len(samples))))
	if len(samples) > 1 {
		st.Jitter = round3(jit / float64(len(samples)-1)) // mean consecutive delta (RFC 3550 style)
	}
	st.Avg = round3(st.Avg)
	return st
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
