// Package agenthub lets traces run on the user's own machines. The globe
// queues a trace job; an agent (cmd/ppagent) on the user's computer
// long-polls for jobs, traces from its own network and public IP, and posts
// the raw trace back. The server enriches, classifies and evaluates it like
// a local trace, so the path on the globe starts where the user really is.
package agenthub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/model"
)

const (
	OnlineWindow    = 60 * time.Second  // an agent is online if it polled this recently
	DispatchTimeout = 150 * time.Second // a dispatched job with no result fails
	queuedTimeout   = 10 * time.Minute
	maxJobs         = 2000
)

var (
	ErrUnknownJob = errors.New("unknown job")
	ErrNoAgent    = errors.New("no agent is online: start ppagent on your computer")
	ErrQueueFull  = errors.New("agent job queue is full")
)

// Job is one trace to run on an agent.
type Job struct {
	ID                  string    `json:"job_id"`
	Target              string    `json:"target"`
	Protocol            string    `json:"protocol,omitempty"`
	MaxHops             int       `json:"max_hops,omitempty"`
	AgentID             string    `json:"agent_id,omitempty"` // requested agent; empty = any
	DataClassifications []string  `json:"data_classifications,omitempty"`
	App                 string    `json:"app,omitempty"`
	SNI                 string    `json:"sni,omitempty"`
	Port                int       `json:"port,omitempty"`
	Status              string    `json:"status"` // queued | dispatched | done | failed
	RanOn               string    `json:"ran_on,omitempty"`
	JourneyID           string    `json:"journey_id,omitempty"`
	Error               string    `json:"error,omitempty"`
	SubmittedBy         string    `json:"submitted_by,omitempty"`
	SubmittedAt         time.Time `json:"submitted_at"`
	DispatchedAt        time.Time `json:"dispatched_at,omitzero"`
	FinishedAt          time.Time `json:"finished_at,omitzero"`
}

// Agent is what an agent reports about itself on every poll.
type Agent struct {
	ID       string          `json:"agent_id"`
	PublicIP string          `json:"public_ip,omitempty"`
	ASOrg    string          `json:"as_org,omitempty"`
	Location *model.Location `json:"location,omitempty"`
	OS       string          `json:"os,omitempty"`
	Version  string          `json:"version,omitempty"`
	Tracing  bool            `json:"tracing"`
	LastSeen time.Time       `json:"last_seen"`
	Online   bool            `json:"online"`
}

type Hub struct {
	mu     sync.Mutex
	jobs   map[string]*Job
	order  []string // submission order, for trimming
	queue  []string
	agents map[string]*Agent
	wake   chan struct{} // closed and replaced whenever a job is queued
	now    func() time.Time
}

func New() *Hub {
	return &Hub{jobs: map[string]*Job{}, agents: map[string]*Agent{}, wake: make(chan struct{}), now: time.Now}
}

// Submit queues a job. It fails fast when no agent that could run it is online.
func (h *Hub) Submit(j Job) (Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expire()
	if !h.onlineLocked(j.AgentID) {
		return Job{}, ErrNoAgent
	}
	if len(h.queue) >= 100 {
		return Job{}, ErrQueueFull
	}
	j.ID, j.Status, j.SubmittedAt = newID(), "queued", h.now().UTC()
	h.jobs[j.ID] = &j
	h.order = append(h.order, j.ID)
	h.queue = append(h.queue, j.ID)
	for len(h.order) > maxJobs {
		delete(h.jobs, h.order[0])
		h.order = h.order[1:]
	}
	close(h.wake)
	h.wake = make(chan struct{})
	return j, nil
}

// Poll records the agent's heartbeat and waits up to wait for a job.
func (h *Hub) Poll(ctx context.Context, a Agent, wait time.Duration) (*Job, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		h.expire()
		a.LastSeen = h.now().UTC()
		h.agents[a.ID] = &a
		if j := h.takeLocked(a.ID); j != nil {
			cp := *j
			h.mu.Unlock()
			return &cp, nil
		}
		wake := h.wake
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, nil
		case <-wake:
		}
	}
}

func (h *Hub) takeLocked(agentID string) *Job {
	for i, id := range h.queue {
		j := h.jobs[id]
		if j == nil {
			continue
		}
		if j.AgentID != "" && j.AgentID != agentID {
			continue
		}
		h.queue = append(h.queue[:i:i], h.queue[i+1:]...)
		j.Status, j.RanOn, j.DispatchedAt = "dispatched", agentID, h.now().UTC()
		return j
	}
	return nil
}

// Finish records the outcome reported by the agent that ran the job.
func (h *Hub) Finish(id, agentID, journeyID, errMsg string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[id]
	if !ok || j.RanOn != agentID {
		return ErrUnknownJob
	}
	j.FinishedAt = h.now().UTC()
	if errMsg != "" {
		j.Status, j.Error = "failed", errMsg
	} else {
		j.Status, j.JourneyID = "done", journeyID
	}
	return nil
}

// Dispatched returns the job if agentID is running it.
func (h *Hub) Dispatched(id, agentID string) (Job, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[id]
	if !ok || j.RanOn != agentID || j.Status != "dispatched" {
		return Job{}, false
	}
	return *j, true
}

func (h *Hub) Job(id string) (Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expire()
	j, ok := h.jobs[id]
	if !ok {
		return Job{}, ErrUnknownJob
	}
	return *j, nil
}

func (h *Hub) Agent(id string) (Agent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[id]
	if !ok {
		return Agent{}, false
	}
	return *a, true
}

// Agents lists agents seen in the last hour, online first.
func (h *Hub) Agents() []Agent {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := []Agent{}
	for id, a := range h.agents {
		if now.Sub(a.LastSeen) > time.Hour {
			delete(h.agents, id)
			continue
		}
		cp := *a
		cp.Online = now.Sub(a.LastSeen) <= OnlineWindow
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

func (h *Hub) onlineLocked(id string) bool {
	now := h.now()
	for _, a := range h.agents {
		if (id == "" || a.ID == id) && now.Sub(a.LastSeen) <= OnlineWindow {
			return true
		}
	}
	return false
}

// expire fails jobs nobody picked up or finished in time.
func (h *Hub) expire() {
	now := h.now()
	keep := h.queue[:0]
	for _, id := range h.queue {
		j := h.jobs[id]
		if j != nil && now.Sub(j.SubmittedAt) > queuedTimeout {
			j.Status, j.Error, j.FinishedAt = "failed", "no agent picked the job up", now.UTC()
			continue
		}
		keep = append(keep, id)
	}
	h.queue = keep
	for _, j := range h.jobs {
		if j.Status == "dispatched" && now.Sub(j.DispatchedAt) > DispatchTimeout {
			j.Status, j.Error, j.FinishedAt = "failed", "agent did not report a result in time", now.UTC()
		}
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "aj-" + hex.EncodeToString(b)
}
