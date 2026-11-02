// Package api exposes the coordinator over HTTP.
//
//	POST /v1/traces            submit a trace (202 + job)
//	GET  /v1/traces/{id}       job status
//	GET  /v1/journeys/{id}     enriched journey
//	GET  /v1/journeys          list (?destination=&verdict=&since=RFC3339&limit=)
//	GET  /v1/stream            Server-Sent Events: one "journey" event per finished trace
//	GET  /v1/policies          compliance policies
//	GET  /v1/schema            PacketJourney JSON Schema
//	GET  /healthz              liveness + counters
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/ingest"
	"github.com/flotrpy/packetpulse/backend/internal/store"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
	"github.com/flotrpy/packetpulse/backend/schema"
)

type Server struct {
	Coord *ingest.Coordinator
	Repo  store.JourneyRepository
	Ref   store.ReferenceStore
	Stats func() map[string]int // optional reference-table sizes
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", s.submit)
	mux.HandleFunc("GET /v1/traces/{id}", s.job)
	mux.HandleFunc("GET /v1/journeys/{id}", s.journey)
	mux.HandleFunc("GET /v1/journeys", s.journeys)
	mux.HandleFunc("GET /v1/stream", s.stream)
	mux.HandleFunc("GET /v1/policies", s.policies)
	mux.HandleFunc("GET /v1/schema", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/schema+json")
		w.Write(schema.PacketJourney)
	})
	mux.HandleFunc("GET /healthz", s.health)
	return mux
}

// TraceRequest is the POST /v1/traces body.
type TraceRequest struct {
	Target       string `json:"target"`
	Protocol     string `json:"protocol,omitempty"`
	IPVersion    int    `json:"ip_version,omitempty"`
	PolicyID     string `json:"policy_id,omitempty"`
	AccessType   string `json:"access_type,omitempty"`
	FirstTTL     int    `json:"first_ttl,omitempty"`
	MaxHops      int    `json:"max_hops,omitempty"`
	ProbesPerHop int    `json:"probes_per_hop,omitempty"`
	TimeoutMS    int    `json:"timeout_ms,omitempty"`
}

func (t TraceRequest) ToIngest() ingest.Request {
	return ingest.Request{
		Target: t.Target, IPVersion: t.IPVersion, PolicyID: t.PolicyID, AccessType: t.AccessType,
		Options: traceroute.Options{
			Protocol: traceroute.Protocol(t.Protocol), FirstTTL: t.FirstTTL, MaxHops: t.MaxHops,
			ProbesPerHop: t.ProbesPerHop, Timeout: time.Duration(t.TimeoutMS) * time.Millisecond,
		},
	}
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var body TraceRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	job, err := s.Coord.Submit(body.ToIngest())
	switch {
	case errors.Is(err, ingest.ErrBadRequest):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, ingest.ErrQueueFull):
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusTooManyRequests, err)
	case errors.Is(err, ingest.ErrStopped):
		writeErr(w, http.StatusServiceUnavailable, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		w.Header().Set("Location", "/v1/traces/"+job.ID)
		writeJSON(w, http.StatusAccepted, job)
	}
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	j, err := s.Coord.Job(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) journey(w http.ResponseWriter, r *http.Request) {
	j, err := s.Repo.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) journeys(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.JourneyFilter{DestinationIP: q.Get("destination"), Verdict: q.Get("verdict")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be a positive integer"))
			return
		}
		f.Limit = n
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("since must be RFC3339"))
			return
		}
		f.Since = t
	}
	list, err := s.Repo.List(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"journeys": list, "count": len(list)})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch, cancel := s.Coord.Subscribe()
	defer cancel()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case j, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(j)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: journey\nid: %s\ndata: %s\n\n", j.JourneyID, b)
			fl.Flush()
		}
	}
}

func (s *Server) policies(w http.ResponseWriter, _ *http.Request) {
	type pol struct {
		ID                 string   `json:"id"`
		Label              string   `json:"label"`
		Allowed            []string `json:"allowed_countries"`
		Restricted         []string `json:"restricted_countries"`
		TransitIsViolation bool     `json:"transit_counts_as_transfer"`
	}
	out := []pol{}
	for _, p := range s.Ref.Policies() {
		out = append(out, pol{ID: p.ID, Label: p.Label, Allowed: keys(p.Allowed), Restricted: keys(p.Restricted), TransitIsViolation: p.TransitIsViolation})
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	n, _ := s.Repo.Count(r.Context())
	body := map[string]any{"status": "ok", "ingest": s.Coord.Stats(), "journeys_stored": n}
	if s.Stats != nil {
		body["reference"] = s.Stats()
	}
	writeJSON(w, http.StatusOK, body)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
