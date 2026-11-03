package agenthub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/ingest"
	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

type fakeIngest struct {
	got *ingest.Request
	src *model.Location
}

func (f *fakeIngest) IngestRaw(_ context.Context, raw *traceroute.RawTrace, req ingest.Request, agentID string, src *model.Location) (*model.Journey, error) {
	f.got, f.src = &req, src
	return &model.Journey{JourneyID: "j-1"}, nil
}

func do(t *testing.T, ts *httptest.Server, method, path, token string, body any) *http.Response {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestJobRoundTrip(t *testing.T) {
	fi := &fakeIngest{}
	mux := http.NewServeMux()
	(&Server{Hub: New(), Ingest: fi, TraceTokens: []map[string]string{{"trace-token-0123456789": "me"}},
		AgentTokens: map[string]string{"agent-token-0123456789": "agent"}}).Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// No agent online yet: the globe gets a clear error instead of a stuck job.
	if r := do(t, ts, "POST", "/v1/agent/jobs", "trace-token-0123456789", JobRequest{Target: "example.com"}); r.StatusCode != http.StatusConflict {
		t.Fatalf("no agent: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/v1/agent/poll?wait=0", "", Agent{ID: "laptop"}); r.StatusCode != 401 {
		t.Fatalf("poll without token: %d", r.StatusCode)
	}
	loc := &model.Location{Lat: 51.5, Lon: -0.12, City: "London", CountryISO2: "GB", Method: "prefix_db", Confidence: 0.7, AccuracyKM: 25}
	me := Agent{ID: "laptop", PublicIP: "81.2.69.160", Location: loc, Tracing: true}
	if r := do(t, ts, "POST", "/v1/agent/poll?wait=0", "agent-token-0123456789", me); r.StatusCode != http.StatusNoContent {
		t.Fatalf("idle poll: %d", r.StatusCode)
	}

	// The agent is long-polling when the job arrives.
	got := make(chan Job, 1)
	go func() {
		r := do(t, ts, "POST", "/v1/agent/poll?wait=5", "agent-token-0123456789", me)
		var j Job
		json.NewDecoder(r.Body).Decode(&j)
		got <- j
	}()
	time.Sleep(50 * time.Millisecond)
	r := do(t, ts, "POST", "/v1/agent/jobs", "trace-token-0123456789", JobRequest{Target: "api.razorpay.com", App: "upi-payments", Port: 443})
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("submit: %d", r.StatusCode)
	}
	var queued Job
	json.NewDecoder(r.Body).Decode(&queued)
	var j Job
	select {
	case j = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not wake up")
	}
	if j.ID != queued.ID || j.Target != "api.razorpay.com" || j.Status != "dispatched" {
		t.Fatalf("dispatched %+v", j)
	}

	raw := &traceroute.RawTrace{Target: netip.MustParseAddr("104.18.0.1"), Probes: []traceroute.ProbeResult{{TTL: 1}}}
	if r := do(t, ts, "POST", "/v1/agent/results", "agent-token-0123456789", Result{JobID: j.ID, AgentID: "someone-else", Raw: raw}); r.StatusCode != 404 {
		t.Fatalf("result from another agent: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/v1/agent/results", "agent-token-0123456789", Result{JobID: j.ID, AgentID: "laptop", Raw: raw}); r.StatusCode != 200 {
		t.Fatalf("result: %d", r.StatusCode)
	}
	if fi.got.Target != "api.razorpay.com" || fi.got.App != "upi-payments" || fi.src == nil || fi.src.City != "London" {
		t.Fatalf("ingest got %+v src %+v", fi.got, fi.src)
	}
	r = do(t, ts, "GET", "/v1/agent/jobs/"+j.ID, "", nil)
	var done Job
	json.NewDecoder(r.Body).Decode(&done)
	if done.Status != "done" || done.JourneyID != "j-1" || done.RanOn != "laptop" {
		t.Fatalf("job %+v", done)
	}
	r = do(t, ts, "GET", "/v1/agent/status", "", nil)
	var st struct {
		Online int
		Agents []Agent
	}
	json.NewDecoder(r.Body).Decode(&st)
	if st.Online != 1 || st.Agents[0].PublicIP != "81.2.69.160" {
		t.Fatalf("status %+v", st)
	}
}

func TestExpiry(t *testing.T) {
	h := New()
	now := time.Now()
	h.now = func() time.Time { return now }
	h.Poll(context.Background(), Agent{ID: "a"}, 0)
	j, err := h.Submit(Job{Target: "x.example"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := h.Poll(context.Background(), Agent{ID: "a"}, 0)
	if got == nil || got.ID != j.ID {
		t.Fatal("not dispatched")
	}
	now = now.Add(DispatchTimeout + time.Second)
	if j, _ := h.Job(j.ID); j.Status != "failed" {
		t.Fatalf("dispatched job did not time out: %+v", j)
	}
	now = now.Add(2 * time.Minute)
	if _, err := h.Submit(Job{Target: "y.example"}); err != ErrNoAgent {
		t.Fatalf("agent should be offline: %v", err)
	}
}
