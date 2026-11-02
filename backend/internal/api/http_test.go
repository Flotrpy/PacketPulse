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

	"github.com/flotrpy/packetpulse/backend/internal/classify"
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

const adminToken = "test-admin-token-0123456789"

func newServer(t *testing.T) *httptest.Server {
	ref := store.NewSeededReference()
	repo := store.NewMemoryRepository(10)
	cls, err := classify.New("", classify.RuleSet{Rules: []classify.Rule{
		{ID: "aws-eu-crm", Enabled: true, Match: classify.Match{CloudProviders: []string{"aws"}, Apps: []string{"crm-eu"}}, Classifications: []string{"eu_personal_data"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	coord := ingest.New(ingest.Config{Workers: 2}, oneHopTracer{}, pipeline.Default(ref, repo, nil, cls), ref, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	coord.Start(ctx)
	ts := httptest.NewServer((&Server{Coord: coord, Repo: repo, Ref: ref, Stats: ref.Stats,
		Classifier: cls, AdminTokens: map[string]string{adminToken: "alice"}}).Handler())
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

// trace submits body and returns the finished journey.
func trace(t *testing.T, ts *httptest.Server, body string) model.Journey {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/traces", "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit %s: %v %v", body, err, resp.Status)
	}
	var job ingest.Job
	decode(t, resp, &job)
	for deadline := time.Now().Add(3 * time.Second); job.Status != ingest.StatusDone && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		r, _ := http.Get(ts.URL + "/v1/traces/" + job.ID)
		decode(t, r, &job)
	}
	r, _ := http.Get(ts.URL + "/v1/journeys/" + job.JourneyID)
	var j model.Journey
	decode(t, r, &j)
	return j
}

func TestClassificationAtIngestion(t *testing.T) {
	ts := newServer(t)

	// Rule match: AWS destination + the agent's app label.
	j := trace(t, ts, `{"target":"18.96.32.10","app":"crm-eu","port":443,"max_hops":3}`)
	dc := j.Metadata.DataClassification
	if dc == nil || dc.Source != "rules" || len(dc.Labels) != 1 || dc.Labels[0] != "eu_personal_data" || dc.RuleIDs[0] != "aws-eu-crm" || dc.App != "crm-eu" {
		t.Fatalf("rules: %+v", dc)
	}
	// Explicit labels win over rules (and the different labels must not
	// coalesce with the job above).
	j = trace(t, ts, `{"target":"18.96.32.10","app":"crm-eu","data_classifications":["payment_system_data"],"max_hops":3}`)
	if dc := j.Metadata.DataClassification; dc.Source != "request" || dc.Labels[0] != "payment_system_data" {
		t.Fatalf("explicit: %+v", dc)
	}
	// Untagged traffic is recorded as such.
	j = trace(t, ts, `{"target":"18.96.32.10","max_hops":3}`)
	if dc := j.Metadata.DataClassification; dc.Source != "none" || len(dc.Labels) != 0 {
		t.Fatalf("none: %+v", dc)
	}

	for body, code := range map[string]int{
		`{"target":"1.1.1.1","data_classifications":["Bad-Label!"]}`: 400,
		`{"target":"1.1.1.1","port":70000}`:                          400,
	} {
		r, _ := http.Post(ts.URL+"/v1/traces", "application/json", strings.NewReader(body))
		if r.StatusCode != code {
			t.Errorf("%s -> %d", body, r.StatusCode)
		}
		r.Body.Close()
	}
}

func TestClassificationRulesAPI(t *testing.T) {
	ts := newServer(t)
	put := func(token, ifMatch, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/classification/rules", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	rules := `{"rules":[{"id":"pg","enabled":true,"match":{"ports":[5432]},"classifications":["db_replication"]}]}`
	for _, c := range []struct {
		token, ifMatch, body string
		code                 int
	}{
		{"", "", rules, 401},
		{"wrong-token-wrong-token", "", rules, 401},
		{adminToken, "", `{"rules":[{"id":"x1","enabled":true,"match":{},"classifications":["a_b"]}]}`, 422},
		{adminToken, `"7"`, rules, 412},
		{adminToken, `"1"`, rules, 200},
	} {
		r := put(c.token, c.ifMatch, c.body)
		if r.StatusCode != c.code {
			t.Errorf("PUT token=%q if-match=%s -> %d, want %d", c.token, c.ifMatch, r.StatusCode, c.code)
		}
		r.Body.Close()
	}
	r, _ := http.Get(ts.URL + "/v1/classification/rules")
	var set classify.RuleSet
	decode(t, r, &set)
	if set.Version != 2 || set.UpdatedBy != "alice" || set.Rules[0].ID != "pg" {
		t.Fatalf("%+v", set)
	}
	// The new rule applies to the next trace without a restart.
	r, _ = http.Post(ts.URL+"/v1/classification/test", "application/json", strings.NewReader(`{"target":"db.example.com","port":5432}`))
	var res classify.Result
	decode(t, r, &res)
	if res.Source != "rules" || res.Labels[0] != "db_replication" {
		t.Fatalf("%+v", res)
	}
	if j := trace(t, ts, `{"target":"18.96.32.10","port":5432,"max_hops":3}`); j.Metadata.DataClassification.Labels[0] != "db_replication" {
		t.Fatalf("%+v", j.Metadata.DataClassification)
	}
}

func TestPushJourneyFromAgent(t *testing.T) {
	ts := newServer(t)
	// A remote agent's journey, untagged at the agent: the server classifies it.
	src := trace(t, ts, `{"target":"18.96.32.10","max_hops":3}`)
	src.JourneyID = "11111111-2222-4333-8444-555555555555"
	src.Metadata.AgentID = "laptop-1"
	src.Metadata.DataClassification = &model.DataClassification{Labels: []string{}, Source: "none", App: "crm-eu"}
	body, _ := json.Marshal(src)
	post := func(token string, b []byte) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/journeys", strings.NewReader(string(b)))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := post("", body); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r := post(adminToken, []byte(`{"journey_id":"x"}`)); r.StatusCode != 422 && r.StatusCode != 400 {
		t.Fatalf("invalid journey: %d", r.StatusCode)
	}
	r := post(adminToken, body)
	if r.StatusCode != 202 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("push: %d %s", r.StatusCode, b)
	}
	r.Body.Close()
	r, _ = http.Get(ts.URL + "/v1/journeys/" + src.JourneyID)
	var got model.Journey
	decode(t, r, &got)
	if dc := got.Metadata.DataClassification; dc.Source != "rules" || dc.Labels[0] != "eu_personal_data" || got.Metadata.AgentID != "laptop-1" {
		t.Fatalf("%+v %s", dc, got.Metadata.AgentID)
	}
}
