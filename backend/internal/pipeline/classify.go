package pipeline

import (
	"context"
	"net/netip"

	"github.com/flotrpy/packetpulse/backend/internal/classify"
	"github.com/flotrpy/packetpulse/backend/internal/model"
)

// Classify tags the journey with the data classes its traffic carries. It
// runs after Resolve so rules can match the destination's network and cloud.
type Classify struct {
	Rules *classify.Classifier // nil: only explicit request labels are used
}

func (Classify) Name() string   { return "classify" }
func (Classify) Required() bool { return false }

func (c Classify) Process(_ context.Context, s *State) error {
	d := s.Journey.Metadata.Destination
	in := classify.Input{IP: s.Raw.Target, ASN: d.ASN, Cloud: d.Cloud, App: s.Req.App, SNI: s.Req.SNI, Port: s.Req.Port}
	if _, err := netip.ParseAddr(s.Req.Target); err != nil {
		in.Host = s.Req.Target
	}
	var r classify.Result
	switch {
	case c.Rules != nil:
		r = c.Rules.Classify(in, s.Req.Classifications)
	case len(s.Req.Classifications) > 0:
		r = classify.Result{Labels: classify.Normalize(s.Req.Classifications), Source: "request"}
	default:
		r = classify.Result{Labels: []string{}, Source: "none"}
	}
	s.Journey.Metadata.DataClassification = &model.DataClassification{
		Labels: r.Labels, Source: r.Source, RuleIDs: r.RuleIDs, App: s.Req.App, SNI: s.Req.SNI, Port: s.Req.Port,
	}
	return nil
}
