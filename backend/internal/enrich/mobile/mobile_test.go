package mobile

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
)

const raw = "../../../../data/raw/"

func seeded() *Registry {
	r := NewRegistry()
	for _, op := range Seed() {
		r.Add(op)
	}
	return r
}

func TestRegistrySeed(t *testing.T) {
	r := seeded()
	op, ok := r.ByASN(55836)
	if !ok || op.Brand != "Jio" || op.Source != "seed" {
		t.Fatalf("%+v", op)
	}
	ops := r.ByMCCMNC("405", "874")
	if len(ops) == 0 || ops[0].ASN != 55836 {
		t.Fatalf("405-874 -> %+v", ops)
	}
	if _, ok := r.ByASN(6453); ok {
		t.Fatal("Tata backbone is not an MNO")
	}
}

func TestPeeringDBMatchesKeepSeedPriority(t *testing.T) {
	if _, err := os.Stat(raw + "peeringdb/net.json.gz"); err != nil {
		t.Skip("PeeringDB snapshot not present")
	}
	r := seeded()
	n, err := r.LoadPeeringDBMatches(raw+"mcc-mnc/mcc-mnc-list.json.gz", raw+"peeringdb/net.json.gz", raw+"peeringdb/org.json.gz")
	if err != nil || n < 100 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if op, _ := r.ByASN(55836); op.Source != "seed" {
		t.Fatalf("seed must win: %+v", op)
	}
	op, ok := r.ByASN(4775) // Globe Telecom (PH), found by name
	if !ok || op.Source != "peeringdb_mccmnc" || op.Country != "PH" || op.Confidence >= 0.95 {
		t.Fatalf("%+v ok=%v", op, ok)
	}
}

func TestHubDirectory(t *testing.T) {
	if _, err := os.Stat(raw + "peeringdb/fac.json.gz"); err != nil {
		t.Skip("PeeringDB snapshot not present")
	}
	d, err := LoadHubDirectory(raw+"peeringdb/fac.json.gz", raw+"peeringdb/netfac.json.gz", raw+"peeringdb/netixlan.json.gz", raw+"peeringdb/ixfac.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	// T-Mobile USA lists its own facilities.
	h, km, ok := Nearest(d.HubsFor(21928, "US"), gis.LatLon{Lat: 40.71, Lon: -74.0})
	if !ok || h.Kind != "operator_facility" || km > 50 {
		t.Fatalf("T-Mobile NYC hub %+v %.0f km", h, km)
	}
	// Jio publishes no facilities or IXPs: fall back to India's interconnection metros.
	h, km, ok = Nearest(d.HubsFor(55836, "IN"), gis.LatLon{Lat: 19.06, Lon: 72.83})
	if !ok || h.Kind != "country_interconnection_metro" || h.City != "Mumbai" || h.Confidence > 0.5 || km > 30 {
		t.Fatalf("Jio Mumbai hub %+v %.0f km", h, km)
	}
	h, _, _ = Nearest(d.HubsFor(55836, "IN"), gis.LatLon{Lat: 13.08, Lon: 80.27})
	if h.City != "Chennai" {
		t.Fatalf("Chennai subscriber -> %s", h.City)
	}
}

func TestOpenCellIDClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("key") != "k" {
			w.Write([]byte(`{"error":"Invalid token","code":2}`))
			return
		}
		if q.Get("cellid") == "26511" {
			w.Write([]byte(`{"lat":52.2296,"lon":21.0122,"mcc":260,"mnc":2,"lac":10250,"cellid":26511,"range":1200,"samples":34,"radio":"GSM"}`))
			return
		}
		w.Write([]byte(`{"error":"Cell not found","code":1}`))
	}))
	defer srv.Close()
	c := &OpenCellIDClient{BaseURL: srv.URL, Key: "k"}
	cell, err := c.Locate(context.Background(), CellKey{MCC: 260, MNC: 2, LAC: 10250, CellID: 26511})
	if err != nil || cell.Lat != 52.2296 || cell.RangeM != 1200 || cell.Radio != "GSM" || cell.Source != "opencellid_api" {
		t.Fatalf("%+v %v", cell, err)
	}
	if _, err := c.Locate(context.Background(), CellKey{MCC: 260, MNC: 2, LAC: 1, CellID: 1}); !errors.Is(err, ErrCellNotFound) {
		t.Fatalf("want ErrCellNotFound, got %v", err)
	}
	bad := &OpenCellIDClient{BaseURL: srv.URL, Key: "nope"}
	if _, err := bad.Locate(context.Background(), CellKey{MCC: 260}); err == nil || !strings.Contains(err.Error(), "Invalid token") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenCellIDCSV(t *testing.T) {
	csv := "radio,mcc,net,area,cell,unit,lon,lat,range,samples,changeable,created,updated,averageSignal\n" +
		"LTE,405,874,41021,123456789,0,72.8295,19.0596,850,12,1,1600000000,1700000000,0\n" +
		"GSM,262,1,100,200,0,13.40,52.52,500,3,1,1600000000,1700000000,0\n"
	l, err := LoadCSV(strings.NewReader(csv), map[int]bool{405: true})
	if err != nil {
		t.Fatal(err)
	}
	if l.Len() != 1 {
		t.Fatalf("MCC filter kept %d cells", l.Len())
	}
	c, err := l.Locate(context.Background(), CellKey{MCC: 405, MNC: 874, LAC: 41021, CellID: 123456789})
	if err != nil || c.Lat != 19.0596 || c.Radio != "LTE" {
		t.Fatalf("%+v %v", c, err)
	}
}
