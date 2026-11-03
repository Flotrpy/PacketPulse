package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/compliance"
)

const ne = "../../../../data/raw/naturalearth/"
const adminTok = "admin-token-0123456789abcdef"

func server(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	if _, err := os.Stat(ne + "ne_50m_admin_1_states_provinces.geojson.gz"); err != nil {
		t.Skip("Natural Earth data not present")
	}
	loc, err := compliance.LoadLocator(ne+"ne_50m_admin_0_countries.geojson.gz", ne+"ne_50m_admin_1_states_provinces.geojson.gz")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reg.json")
	reg, err := compliance.NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	alerts := compliance.NewAlertStore(100)
	s := &Server{Engine: &compliance.Engine{Registry: reg, Locator: loc, Sink: alerts}, Alerts: alerts, AdminTokens: map[string]string{adminTok: "alice"}}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, path
}

func do(t *testing.T, method, url, tok string, body any, hdr ...string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

var packet = map[string]any{"packet": map[string]any{
	"packet_id": "pkt-1", "timestamp": "2026-11-02T09:30:00Z", "classifications": []string{"sensitive_eu_citizen_data"},
	"route": []map[string]any{
		{"seq": 0, "kind": "source", "label": "Paris", "lat": 48.8566, "lon": 2.3522, "timestamp": "2026-11-02T09:30:00Z", "location_confidence": 1},
		{"seq": 1, "kind": "hop", "label": "Frankfurt", "lat": 50.1109, "lon": 8.6821, "timestamp": "2026-11-02T09:30:00.0026Z", "location_confidence": 0.85},
		{"seq": 2, "kind": "destination", "label": "New York", "ip": "198.51.100.7", "lat": 40.7306, "lon": -73.9866, "timestamp": "2026-11-02T09:30:00.0384Z", "location_confidence": 0.85},
	},
}}

func TestEvaluateAndAlerts(t *testing.T) {
	ts, _ := server(t)

	// Stream first so the alert is seen live.
	streamResp, err := http.Get(ts.URL + "/v1/compliance/alerts/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	live := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(streamResp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				live <- d
				return
			}
		}
	}()

	resp, ev := do(t, "POST", ts.URL+"/v1/compliance/evaluate", "", packet)
	if resp.StatusCode != 200 || ev["verdict"] != "violation" {
		t.Fatalf("%d %v", resp.StatusCode, ev)
	}
	alert := ev["alerts"].([]any)[0].(map[string]any)
	terr := alert["territory"].(map[string]any)
	law := alert["law"].(map[string]any)
	mit := alert["mitigation"].(map[string]any)
	if alert["type"] != "ComplianceViolationAlert" || terr["country_iso2"] != "US" || alert["entered_at"] != "2026-11-02T09:30:00.0384Z" ||
		!strings.Contains(law["citation"].(string), "Chapter V") || !strings.Contains(mit["recommendation"].(string), "Reroute via EU-isolated AWS") {
		t.Fatalf("alert %v", alert)
	}
	select {
	case d := <-live:
		if !strings.Contains(d, `"ComplianceViolationAlert"`) {
			t.Fatalf("stream %s", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no streamed alert")
	}
	if _, list := do(t, "GET", ts.URL+"/v1/compliance/alerts?profile=gdpr-ch5-strict", "", nil); list["count"].(float64) != 1 {
		t.Fatalf("alerts list %v", list)
	}
	if resp, _ := do(t, "POST", ts.URL+"/v1/compliance/evaluate", "", map[string]any{"packet": map[string]any{"packet_id": ""}}); resp.StatusCode != 400 {
		t.Fatalf("bad packet -> %d", resp.StatusCode)
	}
}

func TestDynamicProfileUpdates(t *testing.T) {
	ts, path := server(t)
	u := ts.URL + "/v1/compliance/profiles/gdpr-ch5-strict"

	// Mutations need the admin token.
	if resp, _ := do(t, "PATCH", u, "", map[string]any{"enabled": false}); resp.StatusCode != 401 {
		t.Fatalf("anonymous PATCH -> %d", resp.StatusCode)
	}
	resp, _ := do(t, "GET", u, "", nil)
	etag := resp.Header.Get("ETag")
	if etag != `"gdpr-ch5-strict-v1"` {
		t.Fatalf("etag %q", etag)
	}

	// Allow the adequacy countries too, no restart: the next evaluation uses it.
	resp, body := do(t, "PATCH", u, adminTok, map[string]any{"exceptions": []map[string]any{{"region": "gdpr-adequate", "effect": "allow", "reason": "board approved Art. 45 transfers"}}}, "If-Match", etag)
	if resp.StatusCode != 200 || body["version"].(float64) != 2 || body["updated_by"] != "alice" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	// Lost-update protection: the old ETag no longer matches.
	if resp, _ := do(t, "PATCH", u, adminTok, map[string]any{"enabled": false}, "If-Match", etag); resp.StatusCode != 412 {
		t.Fatalf("stale If-Match -> %d", resp.StatusCode)
	}
	// Disable entirely: the packet is no longer governed by any profile.
	if resp, _ := do(t, "PATCH", u, adminTok, map[string]any{"enabled": false}); resp.StatusCode != 200 {
		t.Fatalf("disable -> %d", resp.StatusCode)
	}
	if _, ev := do(t, "POST", ts.URL+"/v1/compliance/evaluate", "", packet); ev["verdict"] != "not_applicable" {
		t.Fatalf("after disable: %v", ev["verdict"])
	}

	// Invalid profile is rejected with 422 and changes nothing.
	if resp, _ := do(t, "PUT", ts.URL+"/v1/compliance/profiles/broken", adminTok, map[string]any{"name": "x", "classifications": []string{"a"}, "regions": []string{"nowhere"}}); resp.StatusCode != 422 {
		t.Fatalf("invalid profile -> %d", resp.StatusCode)
	}

	// New custom region + new profile, created via the API.
	region := map[string]any{"name": "Nordics", "countries": []string{"se", "no", "dk", "fi", "is"}}
	if resp, _ := do(t, "PUT", ts.URL+"/v1/compliance/regions/nordics", adminTok, region); resp.StatusCode != 201 {
		t.Fatalf("region -> %d", resp.StatusCode)
	}
	prof := map[string]any{
		"name": "Nordic health data stays in the Nordics", "enabled": true, "classifications": []string{"nordic_health_data"},
		"mode": "allow_list", "regions": []string{"nordics"}, "scope": "all_hops", "severity": "high",
		"rule":       "Health records stay within the Nordic region.",
		"law":        map[string]any{"code": "GDPR Art. 9", "title": "Special categories", "citation": "Regulation (EU) 2016/679, Art. 9"},
		"mitigation": map[string]any{"action": "reroute", "template": "Reroute via {{proxy}} ({{proxy_region}}) to avoid {{territory}}."},
	}
	if resp, body := do(t, "PUT", ts.URL+"/v1/compliance/profiles/nordic-health", adminTok, prof); resp.StatusCode != 201 {
		t.Fatalf("profile -> %d %v", resp.StatusCode, body)
	}
	if resp, _ := do(t, "DELETE", ts.URL+"/v1/compliance/regions/nordics", adminTok, nil); resp.StatusCode != 409 {
		t.Fatalf("deleting an in-use region -> %d", resp.StatusCode)
	}
	nordic := map[string]any{"packet": map[string]any{"packet_id": "n1", "timestamp": "2026-11-02T10:00:00Z", "classifications": []string{"nordic_health_data"},
		"route": []map[string]any{
			{"seq": 0, "kind": "source", "lat": 59.3293, "lon": 18.0686, "location_confidence": 1},
			{"seq": 1, "kind": "destination", "lat": 52.52, "lon": 13.405, "location_confidence": 0.9, "timestamp": "2026-11-02T10:00:00.008Z"},
		}}}
	_, ev := do(t, "POST", ts.URL+"/v1/compliance/evaluate", "", nordic)
	alerts := ev["alerts"].([]any)
	if len(alerts) != 1 || alerts[0].(map[string]any)["territory"].(map[string]any)["country_iso2"] != "DE" {
		t.Fatalf("nordic: %v", ev)
	}
	mit := alerts[0].(map[string]any)["mitigation"].(map[string]any)
	if mit["proxy"].(map[string]any)["id"] != "aws-eu-north-1" {
		t.Fatalf("proxy %v", mit["proxy"])
	}

	// Locate helper and audit log.
	if _, loc := do(t, "GET", ts.URL+"/v1/compliance/locate?lat=59.33&lon=18.07", "", nil); !strings.Contains(strings.Join(toStrings(loc["regions"]), ","), "nordics") {
		t.Fatalf("locate %v", loc)
	}
	_, audit := do(t, "GET", ts.URL+"/v1/compliance/audit?limit=10", adminTok, nil)
	// 2 PATCHes + region PUT + profile PUT; the two rejected mutations leave no entry.
	if n := len(audit["entries"].([]any)); n != 4 {
		t.Fatalf("audit entries %d", n)
	}
	// Everything was persisted to the store file.
	b, _ := os.ReadFile(path)
	if !bytes.Contains(b, []byte(`"nordic-health"`)) {
		t.Fatal("profile not persisted")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestReadTokensAndDisabledAdmin(t *testing.T) {
	loc, err := compliance.LoadLocator(ne+"ne_50m_admin_0_countries.geojson.gz", "")
	if err != nil {
		t.Skip(err)
	}
	reg, _ := compliance.NewRegistry("")
	s := &Server{Engine: &compliance.Engine{Registry: reg, Locator: loc}, Alerts: compliance.NewAlertStore(1), ReadTokens: map[string]string{"reader-token-0123456789": "dash"}}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp, _ := do(t, "GET", ts.URL+"/v1/compliance/profiles", "", nil); resp.StatusCode != 401 {
		t.Fatalf("no token -> %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/v1/compliance/profiles", "reader-token-0123456789", nil); resp.StatusCode != 200 {
		t.Fatalf("reader -> %d", resp.StatusCode)
	}
	if resp, _ := do(t, "DELETE", ts.URL+"/v1/compliance/profiles/gdpr-ch5-strict", "reader-token-0123456789", nil); resp.StatusCode != 403 {
		t.Fatalf("admin disabled -> %d", resp.StatusCode)
	}
}

func TestBusinessModeEndpoints(t *testing.T) {
	ts, _ := server(t)

	// Batch preview: statuses for every point, no alerts published.
	route := []map[string]any{
		{"seq": 0, "ttl": 1, "kind": "source", "lat": 48.8566, "lon": 2.3522, "location_confidence": 1},
		{"seq": 1, "ttl": 2, "kind": "hop", "lat": 51.5074, "lon": -0.1278, "location_confidence": 0.85},
		{"seq": 2, "ttl": 3, "kind": "hop", "lat": 53.4808, "lon": -2.2426, "location_confidence": 0.85},
		{"seq": 3, "ttl": 4, "kind": "destination", "lat": 50.1109, "lon": 8.6821, "location_confidence": 0.3},
	}
	batch := map[string]any{"publish": false, "packets": []map[string]any{
		{"packet_id": "a", "timestamp": "2026-11-02T10:00:00Z", "classifications": []string{"sensitive_eu_citizen_data"}, "route": route},
		{"packet_id": "b", "timestamp": "2026-11-02T10:00:00Z", "classifications": []string{"public"}, "route": route},
		{"packet_id": "", "classifications": []string{"x"}, "route": route},
	}}
	resp, body := do(t, "POST", ts.URL+"/v1/compliance/evaluate/batch", "", batch)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	evs := body["evaluations"].([]any)
	if len(evs) != 2 || len(body["errors"].([]any)) != 1 {
		t.Fatalf("evaluations %d errors %v", len(evs), body["errors"])
	}
	var statuses []string
	for _, p := range evs[0].(map[string]any)["route"].([]any) {
		statuses = append(statuses, p.(map[string]any)["status"].(string))
	}
	// London (entry, alert) and Manchester (same territory, no second alert) are both violations.
	if strings.Join(statuses, ",") != "compliant,violation,violation,compliant" {
		t.Fatalf("statuses %v", statuses)
	}
	if len(evs[0].(map[string]any)["alerts"].([]any)) != 1 || evs[1].(map[string]any)["verdict"] != "not_applicable" {
		t.Fatalf("alerts/verdicts %v", evs)
	}
	if _, list := do(t, "GET", ts.URL+"/v1/compliance/alerts", "", nil); list["count"].(float64) != 0 {
		t.Fatalf("preview published %v alerts", list["count"])
	}
	if _, ev := do(t, "POST", ts.URL+"/v1/compliance/evaluate?publish=false", "", packet); ev["verdict"] != "violation" {
		t.Fatal("single preview")
	}
	if _, list := do(t, "GET", ts.URL+"/v1/compliance/alerts", "", nil); list["count"].(float64) != 0 {
		t.Fatal("single preview published")
	}

	// Region borders as simplified GeoJSON.
	resp, fc := do(t, "GET", ts.URL+"/v1/compliance/regions/us-ca/geometry?tolerance=0.1", "", nil)
	feats := fc["features"].([]any)
	if resp.StatusCode != 200 || len(feats) != 1 || feats[0].(map[string]any)["properties"].(map[string]any)["name"] != "California" {
		t.Fatalf("us-ca geometry %d %v", resp.StatusCode, fc["features"])
	}
	_, eea := do(t, "GET", ts.URL+"/v1/compliance/regions/eea/geometry", "", nil)
	if n := len(eea["features"].([]any)); n != 30 {
		t.Fatalf("EEA members drawn: %d", n)
	}
	if resp, _ := do(t, "GET", ts.URL+"/v1/compliance/regions/nope/geometry", "", nil); resp.StatusCode != 404 {
		t.Fatal("unknown region")
	}
}

func TestLawDirectory(t *testing.T) {
	ts, _ := server(t)
	// No directory configured on the default test server.
	if resp, _ := do(t, "GET", ts.URL+"/v1/compliance/laws?countries=FR", "", nil); resp.StatusCode != 404 {
		t.Fatalf("laws without directory -> %d", resp.StatusCode)
	}
	d, err := compliance.LoadLawDirectory("../../../../data/curated/regulations/jurisdictions.json")
	if err != nil {
		t.Skip(err)
	}
	loc, _ := compliance.LoadLocator(ne+"ne_50m_admin_0_countries.geojson.gz", "")
	reg, _ := compliance.NewRegistry("")
	s := &Server{Engine: &compliance.Engine{Registry: reg, Locator: loc}, Alerts: compliance.NewAlertStore(1), Laws: d, CORSOrigins: []string{"http://globe.example"}}
	ts2 := httptest.NewServer(s.Handler())
	defer ts2.Close()
	resp, body := do(t, "GET", ts2.URL+"/v1/compliance/laws?countries=IN,FR,SA,XK", "", nil, "Origin", "http://globe.example")
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://globe.example" {
		t.Fatal("CORS header missing")
	}
	var codes []string
	for _, j := range body["jurisdictions"].([]any) {
		codes = append(codes, j.(map[string]any)["iso2"].(string))
	}
	// France brings in the EU-level entry (GDPR, NIS2...), listed first.
	// The curated file has no France-specific entry: France is covered by "EU".
	if strings.Join(codes, ",") != "EU,IN,SA" {
		t.Fatalf("codes %v", codes)
	}
	if missing := toStrings(body["not_in_directory"]); !slicesContains(missing, "XK") || !slicesContains(missing, "FR") {
		t.Fatalf("missing %v", missing)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
