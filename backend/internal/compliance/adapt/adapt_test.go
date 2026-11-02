package adapt

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/flotrpy/packetpulse/backend/internal/compliance"
	"github.com/flotrpy/packetpulse/backend/internal/enrich"
)

const ne = "../../../../data/raw/naturalearth/"

func example(t *testing.T) *enrich.InfraMap {
	t.Helper()
	b, err := os.ReadFile("../../../schema/examples/mumbai-jio-to-aws-frankfurt.inframap.json")
	if err != nil {
		t.Skip("example inframap not present")
	}
	var m enrich.InfraMap
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestFromInfraMapRoute(t *testing.T) {
	p := FromInfraMap(example(t), "", []string{"payment_system_data"})
	var kinds, labels []string
	for i, rp := range p.Route {
		if rp.Seq != i {
			t.Fatal("route not sequential")
		}
		kinds = append(kinds, rp.Kind)
		labels = append(labels, rp.Label)
		if i > 0 && rp.Timestamp.Before(p.Route[i-1].Timestamp) {
			t.Fatalf("timestamps go backwards at %d (%s)", i, rp.Label)
		}
	}
	if kinds[0] != "source" || kinds[len(kinds)-1] != "destination" || !slices.Contains(kinds, "subsea_landing") {
		t.Fatalf("kinds %v", kinds)
	}
	// SeaMeWe-4's landings between Mumbai and Marseille: Jeddah, Suez, Alexandria.
	// Confidence = probability mass of candidate cables whose matched stretch
	// lands in that country. Every candidate lands in Egypt (Suez crossing);
	// only SeaMeWe-4's stretch lands at Jeddah (the others pass Saudi Arabia
	// via offshore branching units), so Saudi Arabia stays uncertain.
	var landings []string
	conf := map[string]float64{}
	for _, rp := range p.Route {
		if rp.Kind == "subsea_landing" {
			landings = append(landings, rp.CountryHint)
			conf[rp.CountryHint] = rp.LocationConfidence
		}
	}
	if conf["EG"] < 0.8 || conf["SA"] > 0.3 {
		t.Errorf("landing confidence %v", conf)
	}
	if !slices.Equal(landings, []string{"SA", "EG", "EG"}) {
		t.Fatalf("landings %v (%v)", landings, labels)
	}
	if p.PacketID == "" || p.Source != "infraenrich" {
		t.Fatalf("%+v", p)
	}
}

func TestEvaluateEnrichedTrace(t *testing.T) {
	if _, err := os.Stat(ne + "ne_50m_admin_1_states_provinces.geojson.gz"); err != nil {
		t.Skip("Natural Earth data not present")
	}
	loc, err := compliance.LoadLocator(ne+"ne_50m_admin_0_countries.geojson.gz", ne+"ne_50m_admin_1_states_provinces.geojson.gz")
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := compliance.NewRegistry("")
	e := &compliance.Engine{Registry: reg, Locator: loc}
	m := example(t)

	// Sensitive EU data on this path: India at the start, then the Red Sea landings.
	ev, err := e.Evaluate(context.Background(), FromInfraMap(m, "pkt-in-eu", []string{"sensitive_eu_citizen_data"}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range ev.Alerts {
		got = append(got, a.Territory.CountryISO2)
	}
	if !slices.Equal(got, []string{"IN", "SA", "EG"}) {
		t.Fatalf("territories %v", got)
	}
	if sa := ev.Alerts[1]; sa.Point.Kind != "subsea_landing" || sa.EntryTimeSource != "interpolated_along_cable" || sa.Point.Operator != "SeaMeWe-4" ||
		!sa.Suspected || sa.Severity != "high" {
		t.Fatalf("SA alert should be a suspected (downgraded) landing: %+v", sa)
	}
	if eg := ev.Alerts[2]; eg.Suspected || eg.Severity != "critical" {
		t.Fatalf("EG alert should be firm: %+v", eg)
	}

	// Indian payment data terminating in Frankfurt: one storage violation at the destination.
	ev, _ = e.Evaluate(context.Background(), FromInfraMap(m, "pkt-pay", []string{"payment_system_data"}))
	if len(ev.Alerts) != 1 || ev.Alerts[0].Territory.CountryISO2 != "DE" || ev.Alerts[0].Point.IP != "18.96.32.10" {
		t.Fatalf("RBI: %+v", ev.Alerts)
	}
}
