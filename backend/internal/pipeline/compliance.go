package pipeline

import (
	"context"
	"fmt"
	"sort"

	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/store"
)

// Compliance evaluates the journey against a data-residency policy and sets
// the boolean border-violation flags.
//
// Only hops located with confidence >= MinConfidence count as physically
// being in a country. Registry-country guesses (confidence 0.2) are treated
// as unresolved: the registry names the legal holder of an address block, not
// where the router is.
type Compliance struct {
	Ref           store.ReferenceStore
	MinConfidence float64
}

func (Compliance) Name() string   { return "compliance" }
func (Compliance) Required() bool { return false }

func (c Compliance) Process(_ context.Context, s *State) error {
	j := s.Journey
	out := model.Compliance{
		PolicyID: s.Req.PolicyID, Verdict: "not_evaluated",
		AllowedCountries: []string{}, CountriesTraversed: []string{}, PossibleTransit: []string{},
		ViolatingHops: []int{}, Alerts: []model.Alert{},
	}
	minConf := c.MinConfidence
	if minConf == 0 {
		minConf = 0.5
	}

	// Countries actually traversed (confident locations only), in path order.
	seen := map[string]bool{}
	add := func(cc string) {
		if cc != "" && !seen[cc] {
			seen[cc] = true
			out.CountriesTraversed = append(out.CountriesTraversed, cc)
		}
	}
	if l := j.Metadata.Source.Location; l != nil && l.Confidence >= minConf {
		add(l.CountryISO2)
	}
	for _, h := range j.Hops {
		switch {
		case !h.Responded:
			out.Flags.UnresolvedHopsPresent = true
		case h.Location == nil || h.Location.Confidence < minConf:
			if h.AddressClass == "public" {
				out.Flags.UnresolvedHopsPresent = true
			}
		default:
			add(h.Location.CountryISO2)
		}
	}
	destCountry := ""
	if dl := j.Metadata.Destination.Location; dl != nil && dl.Confidence >= minConf {
		destCountry = dl.CountryISO2
	}
	if n := len(j.Hops); n > 0 && j.Hops[n-1].Role == "destination" && j.Hops[n-1].Location != nil && j.Hops[n-1].Location.Confidence >= minConf {
		destCountry = j.Hops[n-1].Location.CountryISO2
	}
	add(destCountry)

	for _, seg := range j.Infrastructure.Segments {
		if seg.Medium == model.MediumSubseaCable && len(seg.Countries) == 2 {
			out.Flags.InternationalSubseaCrossing = true
		}
	}

	if s.Req.PolicyID == "" {
		j.Compliance = out
		return nil
	}
	pol, ok := c.Ref.Policy(s.Req.PolicyID)
	if !ok {
		j.Compliance = out
		return fmt.Errorf("unknown policy %q", s.Req.PolicyID)
	}
	out.Evaluated = true
	for cc := range pol.Allowed {
		out.AllowedCountries = append(out.AllowedCountries, cc)
	}
	sort.Strings(out.AllowedCountries)
	outside := func(cc string) bool { return len(pol.Allowed) > 0 && !pol.Allowed[cc] }

	// Per-hop flags.
	for i := range j.Hops {
		h := &j.Hops[i]
		if !h.Responded || h.Location == nil || h.Location.Confidence < minConf {
			continue
		}
		cc := h.Location.CountryISO2
		isDest := h.Role == "destination"
		if pol.Restricted[cc] {
			h.BorderViolation = true
			out.Flags.TransitedRestrictedCountry = true
			out.ViolatingHops = append(out.ViolatingHops, h.HopNumber)
			out.Alerts = append(out.Alerts, model.Alert{Severity: "critical", Rule: "restricted_country", Country: cc, HopNumber: h.HopNumber,
				Message: fmt.Sprintf("Hop %d (%s) is in %s, a restricted jurisdiction under policy %s.", h.HopNumber, h.IP, cc, pol.ID)})
		} else if outside(cc) {
			h.BorderViolation = true
			out.Flags.LeftAllowedRegion = true
			out.ViolatingHops = append(out.ViolatingHops, h.HopNumber)
			if !isDest {
				sev := "warning"
				if pol.TransitIsViolation {
					sev = "critical"
				}
				out.Alerts = append(out.Alerts, model.Alert{Severity: sev, Rule: "transit_outside_region", Country: cc, HopNumber: h.HopNumber,
					Message: fmt.Sprintf("Hop %d (%s) transits %s, outside the allowed region of policy %s.", h.HopNumber, h.IP, cc, pol.ID)})
			}
		}
	}

	if destCountry != "" && (outside(destCountry) || pol.Restricted[destCountry]) {
		out.Flags.DestinationOutsideRegion = true
		out.Flags.LeftAllowedRegion = true
		out.Alerts = append(out.Alerts, model.Alert{Severity: "critical", Rule: "destination_outside_region", Country: destCountry,
			Message: fmt.Sprintf("Data terminates in %s (%s), outside the allowed region of policy %s.", destCountry, j.Metadata.Destination.IP, pol.ID)})
	}

	// Countries the selected subsea cable also lands in (possible transit).
	possible := map[string]bool{}
	for _, lcs := range s.cables {
		for _, cc := range lcs {
			if !seen[cc] && !possible[cc] {
				possible[cc] = true
				out.PossibleTransit = append(out.PossibleTransit, cc)
			}
		}
	}
	sort.Strings(out.PossibleTransit)
	possibleRisk := false
	for _, cc := range out.PossibleTransit {
		switch {
		case pol.Restricted[cc]:
			possibleRisk = true
			out.Alerts = append(out.Alerts, model.Alert{Severity: "warning", Rule: "possible_restricted_transit", Country: cc,
				Message: fmt.Sprintf("The inferred subsea cable also lands in %s (restricted). Traffic may pass its landing station.", cc)})
		case outside(cc):
			out.Alerts = append(out.Alerts, model.Alert{Severity: "info", Rule: "possible_transit_outside_region", Country: cc,
				Message: fmt.Sprintf("The inferred subsea cable also lands in %s, outside the allowed region.", cc)})
		}
	}
	if out.Flags.UnresolvedHopsPresent {
		out.Alerts = append(out.Alerts, model.Alert{Severity: "info", Rule: "unresolved_hops",
			Message: "Some hops did not answer or could not be located with confidence; their jurisdiction is unknown."})
	}

	out.Flags.BoundaryViolation = out.Flags.DestinationOutsideRegion || out.Flags.TransitedRestrictedCountry ||
		(pol.TransitIsViolation && out.Flags.LeftAllowedRegion)
	switch {
	case out.Flags.BoundaryViolation:
		out.Verdict = "violation"
	case out.Flags.LeftAllowedRegion || possibleRisk:
		out.Verdict = "risk"
	case destCountry == "" || (out.Flags.UnresolvedHopsPresent && len(pol.Allowed) > 0):
		// Nothing seen breaks the policy, but the path or endpoint could
		// not be placed, so compliance cannot be shown.
		out.Verdict = "inconclusive"
	default:
		out.Verdict = "compliant"
	}
	j.Compliance = out
	return nil
}

// Persist saves the finished journey through the repository.
type Persist struct {
	Repo store.JourneyRepository
}

func (Persist) Name() string   { return "persist" }
func (Persist) Required() bool { return true }

func (p Persist) Process(ctx context.Context, s *State) error {
	return p.Repo.Save(ctx, s.Journey)
}

// Default builds the standard pipeline.
func Default(ref store.ReferenceStore, repo store.JourneyRepository, dns ReverseResolver) *Pipeline {
	return New(
		Normalize{},
		Resolve{Ref: ref, DNS: dns, MinRTTSlack: 100},
		InferInfrastructure{Ref: ref},
		Compliance{Ref: ref},
		Persist{Repo: repo},
	)
}
