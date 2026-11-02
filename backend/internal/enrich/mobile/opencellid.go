package mobile

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
)

// CellKey identifies a serving cell, as reported by the device's telephony
// API (Android TelephonyManager: CellIdentityLte/Nr). LAC is the TAC for LTE/NR.
type CellKey struct {
	Radio  string `json:"radio,omitempty"` // GSM | UMTS | LTE | NR
	MCC    int    `json:"mcc"`
	MNC    int    `json:"mnc"`
	LAC    int64  `json:"lac"`
	CellID int64  `json:"cell_id"`
}

func (k CellKey) String() string {
	return fmt.Sprintf("%s %d-%02d lac=%d cell=%d", k.Radio, k.MCC, k.MNC, k.LAC, k.CellID)
}

// Cell is a located tower.
type Cell struct {
	CellKey
	gis.LatLon
	RangeM  int    `json:"range_m"`
	Samples int    `json:"samples"`
	Source  string `json:"source"`
}

var ErrCellNotFound = errors.New("cell not found")

// CellLocator resolves a cell to a tower position.
type CellLocator interface {
	Locate(ctx context.Context, k CellKey) (Cell, error)
}

// OpenCellIDClient queries the OpenCellID API (free key from opencellid.org;
// data CC BY-SA 4.0): GET /cell/get?key=&mcc=&mnc=&lac=&cellid=&radio=&format=json
type OpenCellIDClient struct {
	BaseURL string // default https://opencellid.org
	Key     string
	Client  *http.Client
}

func (c *OpenCellIDClient) Locate(ctx context.Context, k CellKey) (Cell, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://opencellid.org"
	}
	if c.Key == "" {
		return Cell{}, errors.New("opencellid: API key required")
	}
	hc := c.Client
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	q := url.Values{
		"key": {c.Key}, "mcc": {strconv.Itoa(k.MCC)}, "mnc": {strconv.Itoa(k.MNC)},
		"lac": {strconv.FormatInt(k.LAC, 10)}, "cellid": {strconv.FormatInt(k.CellID, 10)}, "format": {"json"},
	}
	if k.Radio != "" {
		q.Set("radio", k.Radio)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/cell/get?"+q.Encode(), nil)
	if err != nil {
		return Cell{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Cell{}, fmt.Errorf("opencellid: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusNotFound {
		return Cell{}, ErrCellNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return Cell{}, fmt.Errorf("opencellid: HTTP %d: %.200s", resp.StatusCode, body)
	}
	var r struct {
		Lat     float64 `json:"lat"`
		Lon     float64 `json:"lon"`
		Range   int     `json:"range"`
		Samples int     `json:"samples"`
		Radio   string  `json:"radio"`
		Error   string  `json:"error"`
		Code    int     `json:"code"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Cell{}, fmt.Errorf("opencellid: decode: %w", err)
	}
	if r.Error != "" {
		if strings.Contains(strings.ToLower(r.Error), "not found") || r.Code == 1 {
			return Cell{}, ErrCellNotFound
		}
		return Cell{}, fmt.Errorf("opencellid: %s", r.Error)
	}
	out := Cell{CellKey: k, LatLon: gis.LatLon{Lat: r.Lat, Lon: r.Lon}, RangeM: r.Range, Samples: r.Samples, Source: "opencellid_api"}
	if r.Radio != "" {
		out.Radio = r.Radio
	}
	return out, nil
}

// CSVLocator serves cells from an OpenCellID dump (cell_towers.csv or a
// per-MCC file): radio,mcc,net,area,cell,unit,lon,lat,range,samples,...
type CSVLocator struct {
	cells map[[4]int64]Cell
}

// LoadCSV reads a dump. mccFilter limits memory to the listed MCCs (nil = all).
func LoadCSV(r io.Reader, mccFilter map[int]bool) (*CSVLocator, error) {
	l := &CSVLocator{cells: map[[4]int64]Cell{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		f := strings.Split(sc.Text(), ",")
		if len(f) < 10 || f[0] == "radio" {
			continue
		}
		mcc, err1 := strconv.Atoi(f[1])
		mnc, err2 := strconv.Atoi(f[2])
		area, err3 := strconv.ParseInt(f[3], 10, 64)
		cid, err4 := strconv.ParseInt(f[4], 10, 64)
		lon, err5 := strconv.ParseFloat(f[6], 64)
		lat, err6 := strconv.ParseFloat(f[7], 64)
		if err := errors.Join(err1, err2, err3, err4, err5, err6); err != nil {
			return nil, fmt.Errorf("opencellid csv line %d: %w", line, err)
		}
		if mccFilter != nil && !mccFilter[mcc] {
			continue
		}
		rng, _ := strconv.Atoi(f[8])
		smp, _ := strconv.Atoi(f[9])
		k := CellKey{Radio: f[0], MCC: mcc, MNC: mnc, LAC: area, CellID: cid}
		l.cells[[4]int64{int64(mcc), int64(mnc), area, cid}] = Cell{CellKey: k, LatLon: gis.LatLon{Lat: lat, Lon: lon}, RangeM: rng, Samples: smp, Source: "opencellid_dump"}
	}
	return l, sc.Err()
}

func (l *CSVLocator) Locate(ctx context.Context, k CellKey) (Cell, error) {
	if err := ctx.Err(); err != nil {
		return Cell{}, err
	}
	c, ok := l.cells[[4]int64{int64(k.MCC), int64(k.MNC), k.LAC, k.CellID}]
	if !ok {
		return Cell{}, ErrCellNotFound
	}
	return c, nil
}

func (l *CSVLocator) Len() int { return len(l.cells) }
