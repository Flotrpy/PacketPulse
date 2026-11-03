// Package bridge sends classified journeys from ingestion to the compliance
// engine, which raises ComplianceViolationAlerts into its operational stream.
// Untagged journeys are skipped: no data class means no profile applies.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/compliance"
	"github.com/flotrpy/packetpulse/backend/internal/compliance/adapt"
	"github.com/flotrpy/packetpulse/backend/internal/model"
)

// Evaluator evaluates and publishes. *compliance.Engine satisfies it; HTTP
// talks to a remote complianced.
type Evaluator interface {
	Evaluate(ctx context.Context, p compliance.Packet) (*compliance.Evaluation, error)
}

type Stats struct {
	Evaluated int64 `json:"evaluated"`
	Skipped   int64 `json:"skipped_untagged"`
	Unlocated int64 `json:"skipped_unlocated"` // tagged, but fewer than two located points
	Alerts    int64 `json:"alerts"`
	Errors    int64 `json:"errors"`
}

type Bridge struct {
	Eval    Evaluator
	Log     *slog.Logger
	Timeout time.Duration

	evaluated, skipped, unlocated, alerts, errors atomic.Int64
}

func (b *Bridge) Stats() Stats {
	return Stats{Evaluated: b.evaluated.Load(), Skipped: b.skipped.Load(), Unlocated: b.unlocated.Load(), Alerts: b.alerts.Load(), Errors: b.errors.Load()}
}

// Run consumes journeys until the channel closes or ctx ends.
func (b *Bridge) Run(ctx context.Context, journeys <-chan *model.Journey) {
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-journeys:
			if !ok {
				return
			}
			b.Handle(ctx, j)
		}
	}
}

// Handle evaluates one journey; it returns nil for skipped journeys.
func (b *Bridge) Handle(ctx context.Context, j *model.Journey) *compliance.Evaluation {
	p := adapt.FromJourney(j)
	if len(p.Classifications) == 0 {
		b.skipped.Add(1)
		return nil
	}
	if len(p.Route) < 2 {
		b.unlocated.Add(1)
		b.log().Info("tagged journey has too few located points to evaluate", "journey", j.JourneyID, "points", len(p.Route))
		return nil
	}
	to := b.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	ectx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	ev, err := b.Eval.Evaluate(ectx, p)
	if err != nil {
		b.errors.Add(1)
		b.log().Warn("compliance evaluation failed", "journey", j.JourneyID, "err", err)
		return nil
	}
	b.evaluated.Add(1)
	b.alerts.Add(int64(len(ev.Alerts)))
	b.log().Info("journey evaluated", "journey", j.JourneyID, "classes", strings.Join(p.Classifications, ","),
		"verdict", ev.Verdict, "alerts", len(ev.Alerts))
	return ev
}

func (b *Bridge) log() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}
	return b.Log
}

// HTTP evaluates on a remote complianced (POST /v1/compliance/evaluate).
type HTTP struct {
	BaseURL string // e.g. http://complianced:8090
	Token   string // optional bearer token (read or admin)
	Client  *http.Client
}

func (h HTTP) Evaluate(ctx context.Context, p compliance.Packet) (*compliance.Evaluation, error) {
	body, err := json.Marshal(map[string]any{"packet": p})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h.BaseURL, "/")+"/v1/compliance/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	c := h.Client
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("complianced: %s: %.200s", res.Status, b)
	}
	var ev compliance.Evaluation
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// Forwarder pushes journeys to a remote PacketPulse server
// (POST /v1/journeys), for agents tracing on hosts the server can't reach.
type Forwarder struct {
	BaseURL string
	Token   string
	Client  *http.Client
	Log     *slog.Logger
}

func (f Forwarder) Send(ctx context.Context, j *model.Journey) error {
	body, err := json.Marshal(j)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.BaseURL, "/")+"/v1/journeys", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}
	c := f.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("forward %s: %s: %s", j.JourneyID, res.Status, b)
	}
	return nil
}

// Run forwards journeys until the channel closes or ctx ends.
func (f Forwarder) Run(ctx context.Context, journeys <-chan *model.Journey) {
	log := f.Log
	if log == nil {
		log = slog.Default()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-journeys:
			if !ok {
				return
			}
			if err := f.Send(ctx, j); err != nil {
				log.Warn("journey not forwarded", "err", err)
			} else {
				log.Info("journey forwarded", "journey", j.JourneyID)
			}
		}
	}
}
