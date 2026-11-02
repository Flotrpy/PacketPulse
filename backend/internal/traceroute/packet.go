package traceroute

import (
	"encoding/binary"
	"errors"
	"net/netip"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	protoICMPv4 = 1
	protoUDP    = 17
	protoICMPv6 = 58
)

// probeKey identifies an outstanding probe. For ICMP probes A/B are the echo
// identifier and sequence; for UDP probes they are the source and destination
// ports. Dst guards against matching a quoted packet sent to another target.
type probeKey struct {
	Proto Protocol
	A, B  uint16
	Dst   netip.Addr
}

// reply is what the listener extracted from one inbound ICMP message.
type reply struct {
	Key        probeKey
	From       netip.Addr
	Kind       ReplyKind
	Code       int
	MPLSLabels []int
}

var errNotOurs = errors.New("icmp message does not quote one of our probes")

// buildEcho returns a marshalled ICMP echo request.
func buildEcho(v6 bool, id, seq uint16, payloadLen int) ([]byte, error) {
	payload := make([]byte, payloadLen)
	copy(payload, "packetpulse")
	var typ icmp.Type = ipv4.ICMPTypeEcho
	if v6 {
		typ = ipv6.ICMPTypeEchoRequest
	}
	m := icmp.Message{Type: typ, Body: &icmp.Echo{ID: int(id), Seq: int(seq), Data: payload}}
	// IPv6 checksum is filled in by the kernel for raw ICMPv6 sockets.
	return m.Marshal(nil)
}

// parseReply decodes an ICMP message (IP header already stripped) received
// from `from`.
func parseReply(v6 bool, b []byte, from netip.Addr) (reply, error) {
	proto := protoICMPv4
	if v6 {
		proto = protoICMPv6
	}
	m, err := icmp.ParseMessage(proto, b)
	if err != nil {
		return reply{}, err
	}
	r := reply{From: from.Unmap(), Code: m.Code}

	switch body := m.Body.(type) {
	case *icmp.Echo:
		if m.Type != ipv4.ICMPTypeEchoReply && m.Type != ipv6.ICMPTypeEchoReply {
			return reply{}, errNotOurs
		}
		r.Kind = ReplyEcho
		r.Key = probeKey{Proto: ICMP, A: uint16(body.ID), B: uint16(body.Seq), Dst: r.From}
		return r, nil
	case *icmp.TimeExceeded:
		r.Kind = ReplyTimeExceeded
		r.MPLSLabels = mplsLabels(body.Extensions)
		return withQuoted(r, v6, body.Data)
	case *icmp.DstUnreach:
		r.Kind = ReplyUnreachable
		r.MPLSLabels = mplsLabels(body.Extensions)
		return withQuoted(r, v6, body.Data)
	}
	return reply{}, errNotOurs
}

// withQuoted fills r.Key from the original datagram quoted inside an ICMP
// error (IP header + at least 8 bytes of the transport header, RFC 792/4443).
func withQuoted(r reply, v6 bool, data []byte) (reply, error) {
	var (
		proto int
		dst   netip.Addr
		l4    []byte
	)
	if v6 {
		if len(data) < 48 {
			return reply{}, errNotOurs
		}
		proto = int(data[6])
		dst = netip.AddrFrom16([16]byte(data[24:40]))
		l4 = data[40:]
	} else {
		if len(data) < 20 {
			return reply{}, errNotOurs
		}
		ihl := int(data[0]&0x0f) * 4
		if ihl < 20 || len(data) < ihl+8 {
			return reply{}, errNotOurs
		}
		proto = int(data[9])
		dst = netip.AddrFrom4([4]byte(data[16:20]))
		l4 = data[ihl:]
	}

	switch {
	case proto == protoICMPv4 && !v6 && l4[0] == byte(ipv4.ICMPTypeEcho),
		proto == protoICMPv6 && v6 && l4[0] == byte(ipv6.ICMPTypeEchoRequest):
		r.Key = probeKey{Proto: ICMP, A: binary.BigEndian.Uint16(l4[4:6]), B: binary.BigEndian.Uint16(l4[6:8]), Dst: dst}
	case proto == protoUDP:
		r.Key = probeKey{Proto: UDP, A: binary.BigEndian.Uint16(l4[0:2]), B: binary.BigEndian.Uint16(l4[2:4]), Dst: dst}
	default:
		return reply{}, errNotOurs
	}
	// Port/protocol unreachable from the target itself means "arrived".
	if r.Kind == ReplyUnreachable && r.From == dst {
		r.Kind = ReplyDestination
	}
	return r, nil
}

// mplsLabels extracts labels from RFC 4950 ICMP extensions.
func mplsLabels(exts []icmp.Extension) []int {
	var out []int
	for _, e := range exts {
		if st, ok := e.(*icmp.MPLSLabelStack); ok {
			for _, l := range st.Labels {
				out = append(out, l.Label)
			}
		}
	}
	return out
}
