package agenthub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/classify"
	"github.com/flotrpy/packetpulse/backend/internal/ingest"
	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

// Ingester runs the pipeline on an agent's raw trace. *ingest.Coordinator satisfies it.
type Ingester interface {
	IngestRaw(ctx context.Context, raw *traceroute.RawTrace, req ingest.Request, agentID string, src *model.Location) (*model.Journey, error)
}

// Server exposes the hub:
//
//	POST /v1/agent/jobs        queue a trace on your agent      [trace or admin token]
//	GET  /v1/agent/jobs/{id}   job status (journey_id when done)
//	GET  /v1/agent/status      agents seen in the last hour, with their public IP and location
//	POST /v1/agent/poll        agent heartbeat + long-poll for a job (?wait=seconds, max 25) [agent token]
//	POST /v1/agent/results     agent posts the raw trace (or an error)                     [agent token]
type Server struct {
	Hub         *Hub
	Ingest      Ingester
	TraceTokens []map[string]string // any of these may queue jobs
	AgentTokens map[string]string
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/agent/jobs", s.submit)
	mux.HandleFunc("GET /v1/agent/jobs/{id}", s.job)
	mux.HandleFunc("GET /v1/agent/status", s.status)
	mux.HandleFunc("POST /v1/agent/poll", s.poll)
	mux.HandleFunc("POST /v1/agent/results", s.results)
}

// JobRequest is the POST /v1/agent/jobs body.
type JobRequest struct {
	Target              string   `json:"target"`
	Protocol            string   `json:"protocol,omitempty"`
	MaxHops             int      `json:"max_hops,omitempty"`
	AgentID             string   `json:"agent_id,omitempty"`
	DataClassifications []string `json:"data_classifications,omitempty"`
	App                 string   `json:"app,omitempty"`
	SNI                 string   `json:"sni,omitempty"`
	Port                int      `json:"port,omitempty"`
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	actor := ""
	for _, set := range s.TraceTokens {
		if a, ok := bearerActor(set, r); ok {
			actor = a
			break
		}
	}
	if actor == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="traces"`)
		writeErr(w, http.StatusUnauthorized, errors.New("trace token required"))
		return
	}
	var req JobRequest
	if !decode(w, r, &req, 1<<16) {
		return
	}
	req.Target = strings.TrimSpace(req.Target)
	switch {
	case req.Target == "" || len(req.Target) > 253 || strings.ContainsAny(req.Target, " /\\?#@"):
		writeErr(w, http.StatusBadRequest, errors.New("target must be a hostname or IP address"))
		return
	case req.Protocol != "" && req.Protocol != "icmp" && req.Protocol != "udp":
		writeErr(w, http.StatusBadRequest, errors.New("protocol must be icmp or udp"))
		return
	case req.MaxHops < 0 || req.MaxHops > 64, req.Port < 0 || req.Port > 65535, len(req.DataClassifications) > 16:
		writeErr(w, http.StatusBadRequest, errors.New("max_hops 1-64, port 0-65535, at most 16 classifications"))
		return
	}
	for _, l := range classify.Normalize(req.DataClassifications) {
		if !classify.ValidLabel(l) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("data classification %q must be lower_snake_case", l))
			return
		}
	}
	j, err := s.Hub.Submit(Job{Target: req.Target, Protocol: req.Protocol, MaxHops: req.MaxHops, AgentID: req.AgentID,
		DataClassifications: req.DataClassifications, App: req.App, SNI: req.SNI, Port: req.Port, SubmittedBy: actor})
	switch {
	case errors.Is(err, ErrNoAgent):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, ErrQueueFull):
		writeErr(w, http.StatusTooManyRequests, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusAccepted, j)
	}
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	j, err := s.Hub.Job(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	agents := s.Hub.Agents()
	online := 0
	for _, a := range agents {
		if a.Online {
			online++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"online": online, "agents": agents})
}

func (s *Server) agentAuth(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := bearerActor(s.AgentTokens, r); ok {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="agents"`)
	writeErr(w, http.StatusUnauthorized, errors.New("agent token required"))
	return false
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	if !s.agentAuth(w, r) {
		return
	}
	var a Agent
	if !decode(w, r, &a, 1<<14) {
		return
	}
	if a.ID == "" || len(a.ID) > 64 {
		writeErr(w, http.StatusBadRequest, errors.New("agent_id is required (max 64 chars)"))
		return
	}
	wait := 25 * time.Second
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 25 {
			writeErr(w, http.StatusBadRequest, errors.New("wait must be 0-25 seconds"))
			return
		}
		wait = time.Duration(n) * time.Second
	}
	j, err := s.Hub.Poll(r.Context(), a, wait)
	if err != nil || j == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// Result is the POST /v1/agent/results body.
type Result struct {
	JobID   string               `json:"job_id"`
	AgentID string               `json:"agent_id"`
	Raw     *traceroute.RawTrace `json:"raw_trace,omitempty"`
	Error   string               `json:"error,omitempty"`
}

func (s *Server) results(w http.ResponseWriter, r *http.Request) {
	if !s.agentAuth(w, r) {
		return
	}
	var res Result
	if !decode(w, r, &res, 2<<20) {
		return
	}
	job, ok := s.Hub.Dispatched(res.JobID, res.AgentID)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("no such job running on this agent"))
		return
	}
	if res.Error != "" || res.Raw == nil {
		msg := res.Error
		if msg == "" {
			msg = "agent returned no trace"
		}
		s.Hub.Finish(job.ID, res.AgentID, "", msg)
		writeJSON(w, http.StatusOK, map[string]string{"status": "failed"})
		return
	}
	var src *model.Location
	if a, ok := s.Hub.Agent(res.AgentID); ok && a.Location != nil {
		l := *a.Location
		src = &l
	}
	j, err := s.Ingest.IngestRaw(r.Context(), res.Raw, ingest.Request{Target: job.Target,
		DataClassifications: job.DataClassifications, App: job.App, SNI: job.SNI, Port: job.Port}, res.AgentID, src)
	if err != nil {
		s.Hub.Finish(job.ID, res.AgentID, "", err.Error())
		code := http.StatusInternalServerError
		if errors.Is(err, ingest.ErrBadRequest) {
			code = http.StatusUnprocessableEntity
		}
		writeErr(w, code, err)
		return
	}
	s.Hub.Finish(job.ID, res.AgentID, j.JourneyID, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "done", "journey_id": j.JourneyID})
}

func bearerActor(tokens map[string]string, r *http.Request) (string, bool) {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || t == "" {
		return "", false
	}
	for tok, actor := range tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			return actor, true
		}
	}
	return "", false
}

func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
