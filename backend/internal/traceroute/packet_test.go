package traceroute

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var (
	target4 = netip.MustParseAddr("18.96.32.10")
	router4 = netip.MustParseAddr("80.231.200.1")
	target6 = netip.MustParseAddr("2600:1f18::10")
	router6 = netip.MustParseAddr("2001:5a0::1")
)

// quoteV4 builds the "original datagram" an ICMP error quotes: IPv4 header + L4.
func quoteV4(proto byte, dst netip.Addr, l4 []byte) []byte {
	h := make([]byte, 20)
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:], uint16(20+len(l4)))
	h[8] = 1
	h[9] = proto
	copy(h[12:16], []byte{10, 0, 0, 5})
	a := dst.As4()
	copy(h[16:20], a[:])
	return append(h, l4...)
}

func quoteV6(next byte, dst netip.Addr, l4 []byte) []byte {
	h := make([]byte, 40)
	h[0] = 0x60
	binary.BigEndian.PutUint16(h[4:], uint16(len(l4)))
	h[6] = next
	h[7] = 1
	a := dst.As16()
	copy(h[24:40], a[:])
	return append(h, l4...)
}

func udpHeader(src, dst uint16) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:], src)
	binary.BigEndian.PutUint16(b[2:], dst)
	binary.BigEndian.PutUint16(b[4:], 8)
	return b
}

func marshal(t *testing.T, m icmp.Message) []byte {
	t.Helper()
	b, err := m.Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseEchoReply(t *testing.T) {
	b := marshal(t, icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 4242, Seq: 7, Data: []byte("x")}})
	r, err := parseReply(false, b, target4)
	if err != nil {
		t.Fatal(err)
	}
	want := probeKey{Proto: ICMP, A: 4242, B: 7, Dst: target4}
	if r.Key != want || r.Kind != ReplyEcho || r.From != target4 {
		t.Fatalf("got %+v", r)
	}
}

func TestParseTimeExceededQuotingEcho(t *testing.T) {
	probe, err := buildEcho(false, 4242, 9, 32)
	if err != nil {
		t.Fatal(err)
	}
	b := marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quoteV4(protoICMPv4, target4, probe)}})
	r, err := parseReply(false, b, router4)
	if err != nil {
		t.Fatal(err)
	}
	if r.Key != (probeKey{Proto: ICMP, A: 4242, B: 9, Dst: target4}) || r.Kind != ReplyTimeExceeded || r.From != router4 {
		t.Fatalf("got %+v", r)
	}
}

func TestParseUDPPortUnreachableFromTarget(t *testing.T) {
	b := marshal(t, icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 3,
		Body: &icmp.DstUnreach{Data: quoteV4(protoUDP, target4, udpHeader(51000, 33440))}})
	r, err := parseReply(false, b, target4)
	if err != nil {
		t.Fatal(err)
	}
	if r.Key != (probeKey{Proto: UDP, A: 51000, B: 33440, Dst: target4}) || r.Kind != ReplyDestination || r.Code != 3 {
		t.Fatalf("got %+v", r)
	}
	// The same message from a router (not the target) is a plain unreachable.
	r, _ = parseReply(false, b, router4)
	if r.Kind != ReplyUnreachable {
		t.Fatalf("router unreachable classified as %v", r.Kind)
	}
}

func TestParseMPLSExtension(t *testing.T) {
	probe, _ := buildEcho(false, 1, 2, 32)
	b := marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{
		Data: quoteV4(protoICMPv4, target4, probe),
		Extensions: []icmp.Extension{&icmp.MPLSLabelStack{Class: 1, Type: 1,
			Labels: []icmp.MPLSLabel{{Label: 24021, TC: 0, S: true, TTL: 1}}}},
	}})
	r, err := parseReply(false, b, router4)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.MPLSLabels) != 1 || r.MPLSLabels[0] != 24021 {
		t.Fatalf("mpls labels = %v", r.MPLSLabels)
	}
}

func TestParseIPv6TimeExceeded(t *testing.T) {
	probe, err := buildEcho(true, 77, 3, 32)
	if err != nil {
		t.Fatal(err)
	}
	b := marshal(t, icmp.Message{Type: ipv6.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quoteV6(protoICMPv6, target6, probe)}})
	r, err := parseReply(true, b, router6)
	if err != nil {
		t.Fatal(err)
	}
	if r.Key != (probeKey{Proto: ICMP, A: 77, B: 3, Dst: target6}) || r.Kind != ReplyTimeExceeded {
		t.Fatalf("got %+v", r)
	}
	b = marshal(t, icmp.Message{Type: ipv6.ICMPTypeDestinationUnreachable, Code: 4,
		Body: &icmp.DstUnreach{Data: quoteV6(protoUDP, target6, udpHeader(40000, 33435))}})
	r, err = parseReply(true, b, target6)
	if err != nil || r.Kind != ReplyDestination || r.Key.Proto != UDP {
		t.Fatalf("got %+v err=%v", r, err)
	}
}

func TestParseRejectsForeignAndTruncated(t *testing.T) {
	// TCP quoted -> not ours.
	b := marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quoteV4(6, target4, make([]byte, 20))}})
	if _, err := parseReply(false, b, router4); err == nil {
		t.Fatal("expected TCP quote to be rejected")
	}
	// Truncated quote.
	b = marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: []byte{0x45, 0, 0}}})
	if _, err := parseReply(false, b, router4); err == nil {
		t.Fatal("expected truncated quote to be rejected")
	}
	// Echo *request* seen on the raw socket (our own outbound copy) is ignored.
	b = marshal(t, icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: 1, Seq: 1}})
	if _, err := parseReply(false, b, target4); err == nil {
		t.Fatal("expected echo request to be ignored")
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := (Options{}).WithDefaults().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	bad := []Options{
		{Protocol: "tcp"},
		{MaxHops: 65},
		{FirstTTL: 40, MaxHops: 30},
		{ProbesPerHop: 11},
		{Timeout: 10},
		{Protocol: UDP, DstPortBase: 65500, MaxHops: 30, ProbesPerHop: 3},
	}
	for _, o := range bad {
		if err := o.WithDefaults().Validate(); err == nil {
			t.Errorf("expected error for %+v", o)
		}
	}
}
