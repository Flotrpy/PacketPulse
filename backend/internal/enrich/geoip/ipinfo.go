package geoip

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// IPinfo queries the IPinfo API.
//
//	Standard API (https://ipinfo.io/{ip}/json): loc, city, region, country,
//	  org ("AS15169 Google LLC"), hostname, anycast. Works without a token at
//	  a low rate limit; a token raises it.
//	Lite API (https://api.ipinfo.io/lite/{ip}, free token required): country,
//	  asn, as_name, no coordinates.
//
// Requests are bounded by MaxConcurrent and retried with backoff on 429/5xx,
// honouring Retry-After.
type IPinfo struct {
	BaseURL       string // default https://ipinfo.io
	Lite          bool   // use the /lite/{ip} response shape
	Token         string
	Client        *http.Client
	MaxConcurrent int
	MaxRetries    int
	// DefaultAccuracyKM: IPinfo returns city-level coordinates without a radius.
	DefaultAccuracyKM float64

	sem chan struct{}
}

func NewIPinfo(token string) *IPinfo {
	return &IPinfo{BaseURL: "https://ipinfo.io", Token: token}
}

// NewIPinfoLite targets the free Lite API (country + ASN only).
func NewIPinfoLite(token string) *IPinfo {
	return &IPinfo{BaseURL: "https://api.ipinfo.io", Lite: true, Token: token}
}

func (c *IPinfo) Name() string {
	if c.Lite {
		return "ipinfo_lite"
	}
	return "ipinfo"
}

type ipinfoResponse struct {
	IP       string `json:"ip"`
	Hostname string `json:"hostname"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Country  string `json:"country"`
	Loc      string `json:"loc"`
	Org      string `json:"org"`
	Anycast  bool   `json:"anycast"`
	Bogon    bool   `json:"bogon"`
	// Lite API fields
	ASN         string `json:"asn"`
	ASName      string `json:"as_name"`
	CountryCode string `json:"country_code"`
}

func (c *IPinfo) init() {
	if c.sem == nil {
		n := c.MaxConcurrent
		if n <= 0 {
			n = 8
		}
		c.sem = make(chan struct{}, n)
	}
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://ipinfo.io"
	}
	if c.DefaultAccuracyKM == 0 {
		c.DefaultAccuracyKM = 50
	}
}

func (c *IPinfo) Lookup(ctx context.Context, ip netip.Addr) (Record, error) {
	ip = ip.Unmap()
	if IsBogon(ip) {
		return Record{}, ErrBogon
	}
	c.init()
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return Record{}, ctx.Err()
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/" + ip.String() + "/json"
	if c.Lite {
		url = strings.TrimRight(c.BaseURL, "/") + "/lite/" + ip.String()
	}
	retries := c.MaxRetries
	if retries == 0 {
		retries = 3
	}
	backoff := 250 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		body, wait, err := c.get(ctx, url)
		if err == nil {
			return c.parse(ip, body)
		}
		lastErr = err
		if wait < 0 { // not retryable
			break
		}
		if wait == 0 {
			wait = backoff
			backoff *= 2
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return Record{}, ctx.Err()
		}
	}
	return Record{}, lastErr
}

// get returns the body, or a retry delay (0 = default backoff, <0 = do not retry).
func (c *IPinfo) get(ctx context.Context, url string) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, -1, ctx.Err()
		}
		return nil, 0, fmt.Errorf("ipinfo: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("ipinfo: read body: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return body, 0, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		wait := time.Duration(0)
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 30 {
			wait = time.Duration(s) * time.Second
		}
		return nil, wait, fmt.Errorf("ipinfo: HTTP %d", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, -1, ErrNotFound
	default:
		return nil, -1, fmt.Errorf("ipinfo: HTTP %d: %.200s", resp.StatusCode, body)
	}
}

func (c *IPinfo) parse(ip netip.Addr, body []byte) (Record, error) {
	var r ipinfoResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Record{}, fmt.Errorf("ipinfo: decode: %w", err)
	}
	if r.Bogon {
		return Record{}, ErrBogon
	}
	name := c.Name()
	rec := Record{IP: ip.String(), City: r.City, Region: r.Region, Hostname: r.Hostname, Anycast: r.Anycast}
	// Standard API: country = ISO2. Lite API: country = full name, country_code = ISO2.
	rec.CountryISO2 = r.CountryCode
	if rec.CountryISO2 == "" && len(r.Country) == 2 {
		rec.CountryISO2 = r.Country
	}
	if lat, lon, ok := parseLoc(r.Loc); ok {
		rec.Lat, rec.Lon, rec.HasLocation, rec.AccuracyKM = lat, lon, true, c.DefaultAccuracyKM
		if r.City == "" {
			rec.AccuracyKM = 500
		}
		rec.source("location", name)
	}
	// "AS15169 Google LLC" (standard) or asn="AS15169", as_name (lite).
	asnField, orgName := r.ASN, r.ASName
	if asnField == "" && r.Org != "" {
		asnField, orgName, _ = strings.Cut(r.Org, " ")
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(asnField), "AS")); err == nil && n > 0 {
		rec.ASN, rec.ASOrg = n, orgName
		rec.source("asn", name)
	}
	if !rec.HasLocation && rec.ASN == 0 && rec.CountryISO2 == "" {
		return Record{}, ErrNotFound
	}
	return rec, nil
}

func parseLoc(s string) (float64, float64, bool) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, false
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(a), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	return lat, lon, true
}

var _ Resolver = (*IPinfo)(nil)
