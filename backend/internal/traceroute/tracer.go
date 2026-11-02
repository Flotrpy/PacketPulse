// Package traceroute wraps a native, concurrent traceroute engine behind the
// Tracer interface. One Engine owns a raw ICMP listener per address family and
// demultiplexes replies to any number of traces running in parallel.
package traceroute

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

type Protocol string

const (
	ICMP Protocol = "icmp"
	UDP  Protocol = "udp"
)

// ReplyKind classifies what answered a probe.
type ReplyKind int

const (
	ReplyNone         ReplyKind = iota // timed out
	ReplyTimeExceeded                  // router on the path (TTL expired)
	ReplyUnreachable                   // router said destination unreachable
	ReplyEcho                          // echo reply from the target (ICMP probes)
	ReplyDestination                   // port/protocol unreachable from the target (UDP probes)
)

func (k ReplyKind) String() string {
	switch k {
	case ReplyTimeExceeded:
		return "time_exceeded"
	case ReplyUnreachable:
		return "dest_unreachable"
	case ReplyEcho:
		return "echo_reply"
	case ReplyDestination:
		return "port_unreachable"
	}
	return "none"
}

// FromTarget reports whether this reply means the probe reached the target.
func (k ReplyKind) FromTarget() bool { return k == ReplyEcho || k == ReplyDestination }

// Options controls one trace. Zero values are replaced by defaults.
type Options struct {
	Protocol     Protocol      `json:"protocol,omitempty"`
	FirstTTL     int           `json:"first_ttl,omitempty"`
	MaxHops      int           `json:"max_hops,omitempty"`
	ProbesPerHop int           `json:"probes_per_hop,omitempty"`
	Timeout      time.Duration `json:"timeout_ns,omitempty"`       // per probe
	SendInterval time.Duration `json:"send_interval_ns,omitempty"` // gap between probes of the same trace
	DstPortBase  int           `json:"dst_port_base,omitempty"`    // UDP only; classic traceroute uses 33434
	PayloadSize  int           `json:"payload_size,omitempty"`
}

func (o Options) WithDefaults() Options {
	if o.Protocol == "" {
		o.Protocol = ICMP
	}
	if o.FirstTTL == 0 {
		o.FirstTTL = 1
	}
	if o.MaxHops == 0 {
		o.MaxHops = 30
	}
	if o.ProbesPerHop == 0 {
		o.ProbesPerHop = 3
	}
	if o.Timeout == 0 {
		o.Timeout = 2 * time.Second
	}
	if o.SendInterval == 0 {
		o.SendInterval = 5 * time.Millisecond
	}
	if o.DstPortBase == 0 {
		o.DstPortBase = 33434
	}
	if o.PayloadSize == 0 {
		o.PayloadSize = 32
	}
	return o
}

func (o Options) Validate() error {
	switch {
	case o.Protocol != ICMP && o.Protocol != UDP:
		return fmt.Errorf("unsupported protocol %q (use icmp or udp)", o.Protocol)
	case o.FirstTTL < 1 || o.FirstTTL > o.MaxHops:
		return fmt.Errorf("first_ttl %d must be in [1, max_hops]", o.FirstTTL)
	case o.MaxHops > 64:
		return fmt.Errorf("max_hops %d exceeds 64", o.MaxHops)
	case o.ProbesPerHop < 1 || o.ProbesPerHop > 10:
		return fmt.Errorf("probes_per_hop %d must be in [1, 10]", o.ProbesPerHop)
	case o.Timeout < 50*time.Millisecond || o.Timeout > 30*time.Second:
		return fmt.Errorf("timeout %s must be in [50ms, 30s]", o.Timeout)
	case o.PayloadSize < 8 || o.PayloadSize > 1400:
		return fmt.Errorf("payload size %d must be in [8, 1400]", o.PayloadSize)
	case o.Protocol == UDP && o.DstPortBase+o.MaxHops*o.ProbesPerHop > 65535:
		return errors.New("udp destination port range overflows 65535")
	}
	return nil
}

// ProbeResult is one probe packet and its fate.
type ProbeResult struct {
	TTL        int
	Attempt    int
	SentAt     time.Time
	RTT        time.Duration
	Responded  bool
	From       netip.Addr
	Kind       ReplyKind
	Code       int
	MPLSLabels []int
}

// RawTrace is the unprocessed output of one traceroute.
type RawTrace struct {
	Target         netip.Addr
	Source         netip.Addr
	Options        Options
	StartedAt      time.Time
	FinishedAt     time.Time
	Probes         []ProbeResult // sorted by TTL, then attempt
	Reached        bool
	DestinationTTL int // 0 if never reached
}

// Tracer runs traceroutes. Implementations must be safe for concurrent use.
type Tracer interface {
	Trace(ctx context.Context, target netip.Addr, opts Options) (*RawTrace, error)
	Close() error
}

// ErrPermission is returned when raw sockets cannot be opened.
var ErrPermission = errors.New("raw ICMP sockets need root or CAP_NET_RAW (sudo setcap cap_net_raw+ep ./ingestd)")
