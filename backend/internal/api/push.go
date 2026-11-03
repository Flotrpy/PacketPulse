package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/flotrpy/packetpulse/backend/internal/classify"
	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/schema"
)

// pushJourney accepts a PacketJourney traced by a remote agent (a host that
// can open raw sockets), stores it and publishes it to /v1/stream and the
// compliance bridge like a local trace. Journeys the agent left untagged
// are classified with this server's rules.
func (s *Server) pushJourney(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(r)
	if !ok {
		actor, ok = matchBearer(s.AgentTokens, r)
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agents"`)
		writeErr(w, http.StatusUnauthorized, errors.New("agent or admin bearer token required"))
		return
	}
	var j model.Journey
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	if err := schema.Validate(&j); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	if dc := j.Metadata.DataClassification; s.Classifier != nil && (dc == nil || dc.Source == "none") {
		d := j.Metadata.Destination
		in := classify.Input{Host: d.Hostname, ASN: d.ASN, Cloud: d.Cloud}
		if a, err := netip.ParseAddr(d.IP); err == nil {
			in.IP = a
		}
		if dc != nil {
			in.App, in.SNI, in.Port = dc.App, dc.SNI, dc.Port
		}
		res := s.Classifier.Classify(in, nil)
		j.Metadata.DataClassification = &model.DataClassification{Labels: res.Labels, Source: res.Source, RuleIDs: res.RuleIDs, App: in.App, SNI: in.SNI, Port: in.Port}
	}
	if j.Metadata.AgentID == "" {
		j.Metadata.AgentID = actor
	}
	if err := s.Repo.Save(r.Context(), &j); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.Coord.Publish(&j)
	writeJSON(w, http.StatusAccepted, map[string]any{"journey_id": j.JourneyID, "data_classification": j.Metadata.DataClassification})
}

func matchBearer(tokens map[string]string, r *http.Request) (string, bool) {
	return (&Server{AdminTokens: tokens}).adminActor(r)
}
