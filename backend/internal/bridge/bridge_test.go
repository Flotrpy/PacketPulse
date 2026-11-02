package bridge

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/flotrpy/packetpulse/backend/internal/compliance"
	"github.com/flotrpy/packetpulse/backend/internal/compliance/api"
	"github.com/flotrpy/packetpulse/backend/internal/model"
)

const ne = "../../../data/raw/naturalearth/"

func setup(t *testing.T) (*compliance.Engine, *compliance.AlertStore, *model.Journey) {
	t.Helper()
	if _, err := os.Stat(ne + "ne_50m_admin_1_states_provinces.geojson.gz"); err != nil {
		t.Skip("Natural Earth data not present")
	}
	loc, err := compliance.LoadLocator(ne+"ne_50m_admin_0_countries.geojson.gz", ne+"ne_50m_admin_1_states_provinces.geojson.gz")
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := compliance.NewRegistry("")
	alerts := compliance.NewAlertStore(100)
	b, err := os.ReadFile("../../schema/examples/mumbai-jio-to-aws-frankfurt.journey.json")
	if err != nil {
		t.Fatal(err)
	}
	var j model.Journey
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	return &compliance.Engine{Registry: reg, Locator: loc, Sink: alerts}, alerts, &j
}

// The example journey is tagged payment_system_data by the ingestion rules;
// RBI's localisation profile keeps that data in India, so the route to
// Frankfurt must raise published alerts.
func TestTaggedJourneyRaisesAlerts(t *testing.T) {
	eng, alerts, j := setup(t)
	b := &Bridge{Eval: eng}
	ev := b.Handle(context.Background(), j)
	if ev == nil || ev.Verdict != "violation" || len(ev.Alerts) == 0 {
		t.Fatalf("evaluation %+v", ev)
	}
	if got := alerts.List(compliance.AlertFilter{}); len(got) != len(ev.Alerts) || got[0].PacketID != j.JourneyID {
		t.Fatalf("published %d alerts, evaluation had %d", len(got), len(ev.Alerts))
	}

	untagged := *j
	untagged.Metadata.DataClassification = &model.DataClassification{Labels: []string{}, Source: "none"}
	if b.Handle(context.Background(), &untagged) != nil {
		t.Fatal("untagged journey evaluated")
	}
	if s := b.Stats(); s.Evaluated != 1 || s.Skipped != 1 || s.Alerts != int64(len(ev.Alerts)) {
		t.Fatalf("stats %+v", s)
	}
}

func TestHTTPEvaluator(t *testing.T) {
	eng, alerts, j := setup(t)
	ts := httptest.NewServer((&api.Server{Engine: eng, Alerts: alerts, AdminTokens: map[string]string{"admin-token-0123456789": "admin"}}).Handler())
	defer ts.Close()
	ev := (&Bridge{Eval: HTTP{BaseURL: ts.URL}}).Handle(context.Background(), j)
	if ev == nil || ev.Verdict != "violation" || len(alerts.List(compliance.AlertFilter{})) == 0 {
		t.Fatalf("%+v", ev)
	}
}
