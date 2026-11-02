package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/ingest"
	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/pipeline"
	"github.com/flotrpy/packetpulse/backend/internal/store"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

type oneHopTracer struct{}

func (oneHopTracer) Trace(_ context.Context, target netip.Addr, opts traceroute.Options) (*traceroute.RawTrace, error) {
	now := time.Now()
	return &traceroute.RawTrace{Target: target, Source: netip.MustParseAddr("10.0.0.5"), Options: opts.WithDefaults(), StartedAt: now, FinishedAt: now,
		Probes:  []traceroute.ProbeResult{{TTL: 1, Responded: true, From: target, Kind: traceroute.ReplyEcho, RTT: 3 * time.Millisecond}},
		Reached: true, DestinationTTL: 1}, nil
}
func (oneHopTracer) Close() error { return nil }

func newServer(t *testing.T) *httptest.Server {
	ref := store.NewSeededReference()
	repo := store.NewMemoryRepository(10)
	coord := ingest.New(ingest.Config{Workers: 2}, oneHopTracer{}, pipeline.Default(ref, repo, nil), ref, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	coord.Start(ctx)
	ts := httptest.NewServer((&Server{Coord: coord, Repo: repo, Ref: ref, Stats: ref.Stats}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func decode(t *testing.T, r *http.Response, v any) {
	t.Helper()
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitPollFetch(t *testing.T) {
	ts := newServer(t)

	// Live stream first, so the journey event is not missed.
	streamResp, err := http.Get(ts.URL + "/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	events := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(streamResp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				events <- strings.TrimPrefix(sc.Text(), "data: ")
				return
			}
		}
	}()

	resp, err := http.Post(ts.URL+"/v1/traces", "application/json", strings.NewReader(`{"target":"18.96.32.10","policy_id":"eu-gdpr-strict","max_hops":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var job ingest.Job
	decode(t, resp, &job)

	deadline := time.Now().Add(3 * time.Second)
	for job.Status != ingest.StatusDone && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		r, _ := http.Get(ts.URL + "/v1/traces/" + job.ID)
		decode(t, r, &job)
	}
	if job.Status != ingest.StatusDone {
		t.Fatalf("job %+v", job)
	}

	r, _ := http.Get(ts.URL + "/v1/journeys/" + job.JourneyID)
	var j model.Journey
	decode(t, r, &j)
	if j.Metadata.Destination.Cloud != "aws" || j.Compliance.Verdict != "compliant" || j.Hops[0].Location.CountryISO2 != "DE" {
		t.Fatalf("journey dest=%+v verdict=%s", j.Metadata.Destination, j.Compliance.Verdict)
	}

	select {
	case ev := <-events:
		if !strings.Contains(ev, j.JourneyID) {
			t.Fatalf("stream event for another journey: %.80s", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE event")
	}

	r, _ = http.Get(ts.URL + "/v1/journeys?verdict=compliant&limit=5")
	var list struct{ Count int }
	decode(t, r, &list)
	if list.Count != 1 {
		t.Fatalf("list count %d", list.Count)
	}
}

func TestErrorsAndMetadataEndpoints(t *testing.T) {
	ts := newServer(t)
	for body, code := range map[string]int{
		`{"target":""}`:                        http.StatusBadRequest,
		`{"target":"1.1.1.1","policy_id":"x"}`: http.StatusBadRequest,
		`{"target":"1.1.1.1","unknown":true}`:  http.StatusBadRequest,
		`not json`:                             http.StatusBadRequest,
	} {
		r, _ := http.Post(ts.URL+"/v1/traces", "application/json", strings.NewReader(body))
		if r.StatusCode != code {
			t.Errorf("%s -> %d, want %d", body, r.StatusCode, code)
		}
		r.Body.Close()
	}
	for path, code := range map[string]int{
		"/v1/traces/nope": 404, "/v1/journeys/nope": 404, "/v1/journeys?limit=-1": 400,
		"/v1/schema": 200, "/v1/policies": 200, "/healthz": 200,
	} {
		r, _ := http.Get(ts.URL + path)
		if r.StatusCode != code {
			t.Errorf("%s -> %d, want %d", path, r.StatusCode, code)
		}
		r.Body.Close()
	}
}
