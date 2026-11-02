// Package api exposes the compliance engine over HTTP. Profile, region and
// proxy changes are validated, persisted and swapped into the live registry
// atomically: the next evaluation uses them, with no restart.
//
//	GET    /v1/compliance/profiles                list profiles
//	GET    /v1/compliance/profiles/{id}           one profile (ETag)
//	PUT    /v1/compliance/profiles/{id}           create/replace       [admin, If-Match optional]
//	PATCH  /v1/compliance/profiles/{id}           JSON merge patch     [admin, If-Match optional]
//	DELETE /v1/compliance/profiles/{id}                                [admin]
//	GET    /v1/compliance/regions[/{id}]          boundary definitions
//	PUT    /v1/compliance/regions/{id}            countries / ISO 3166-2 subdivisions / GeoJSON polygon [admin]
//	DELETE /v1/compliance/regions/{id}            refused while a profile uses it [admin]
//	GET    /v1/compliance/proxies                 mitigation relays
//	PUT    /v1/compliance/proxies/{id}            [admin]
//	DELETE /v1/compliance/proxies/{id}            [admin]
//	GET    /v1/compliance/regions/{id}/geometry   GeoJSON borders (?tolerance=deg, default 0.05)
//	POST   /v1/compliance/evaluate                {"packet": Packet} or {"inframap": InfraMap, "classifications": [...]}; ?publish=false previews without raising alerts
//	POST   /v1/compliance/evaluate/batch          {"packets": [Packet...], "publish": false}  (max 500)
//	GET    /v1/compliance/laws?countries=IN,FR    law directory entries for the territories a route touches
//	GET    /v1/compliance/alerts                  ?profile=&severity=&packet=&since=&limit=
//	GET    /v1/compliance/alerts/stream           Server-Sent Events, one "alert" event per ComplianceViolationAlert
//	GET    /v1/compliance/locate?lat=&lon=        which country / subdivision / regions contain a point
//	GET    /v1/compliance/audit?limit=            registry change log
//	GET    /v1/compliance/schema/alert            ComplianceViolationAlert JSON Schema
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/compliance"
	"github.com/flotrpy/packetpulse/backend/internal/compliance/adapt"
	"github.com/flotrpy/packetpulse/backend/internal/enrich"
	"github.com/flotrpy/packetpulse/backend/schema"
)

type Server struct {
	Engine *compliance.Engine
	Alerts *compliance.AlertStore
	// AdminTokens maps bearer tokens to actor names recorded in the audit
	// log. Empty = all mutations are refused.
	AdminTokens map[string]string
	// ReadTokens, when non-empty, are required for read endpoints too
	// (admin tokens are always accepted).
	ReadTokens map[string]string
	// Laws backs GET /v1/compliance/laws (optional).
	Laws *compliance.LawDirectory
	// CORSOrigins lists browser origins allowed to call the API directly
	// ("*" = any). Empty = same-origin only (e.g. behind the globe's proxy).
	CORSOrigins []string
}

const maxBatch = 500

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	r, a := s.reader, s.admin
	mux.HandleFunc("GET /v1/compliance/profiles", r(s.listProfiles))
	mux.HandleFunc("GET /v1/compliance/profiles/{id}", r(s.getProfile))
	mux.HandleFunc("PUT /v1/compliance/profiles/{id}", a(s.putProfile))
	mux.HandleFunc("PATCH /v1/compliance/profiles/{id}", a(s.patchProfile))
	mux.HandleFunc("DELETE /v1/compliance/profiles/{id}", a(s.deleteProfile))
	mux.HandleFunc("GET /v1/compliance/regions", r(s.listRegions))
	mux.HandleFunc("GET /v1/compliance/regions/{id}", r(s.getRegion))
	mux.HandleFunc("GET /v1/compliance/regions/{id}/geometry", r(s.regionGeometry))
	mux.HandleFunc("POST /v1/compliance/evaluate/batch", r(s.evaluateBatch))
	mux.HandleFunc("GET /v1/compliance/laws", r(s.laws))
	mux.HandleFunc("PUT /v1/compliance/regions/{id}", a(s.putRegion))
	mux.HandleFunc("DELETE /v1/compliance/regions/{id}", a(s.deleteRegion))
	mux.HandleFunc("GET /v1/compliance/proxies", r(s.listProxies))
	mux.HandleFunc("PUT /v1/compliance/proxies/{id}", a(s.putProxy))
	mux.HandleFunc("DELETE /v1/compliance/proxies/{id}", a(s.deleteProxy))
	mux.HandleFunc("POST /v1/compliance/evaluate", r(s.evaluate))
	mux.HandleFunc("GET /v1/compliance/alerts", r(s.listAlerts))
	mux.HandleFunc("GET /v1/compliance/alerts/stream", r(s.streamAlerts))
	mux.HandleFunc("GET /v1/compliance/locate", r(s.locate))
	mux.HandleFunc("GET /v1/compliance/audit", r(s.audit))
	mux.HandleFunc("GET /v1/compliance/schema/alert", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/schema+json")
		w.Write(schema.ComplianceAlert)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		snap := s.Engine.Registry.Snapshot()
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "registry_snapshot": snap.Seq,
			"profiles": len(snap.Profiles), "regions": len(snap.Regions), "proxies": len(snap.Proxies)})
	})
	if len(s.CORSOrigins) > 0 {
		return s.cors(mux)
	}
	return mux
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := ""
		for _, o := range s.CORSOrigins {
			if o == "*" || o == origin {
				allowed = origin
				break
			}
		}
		if allowed != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "ETag")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) regionGeometry(w http.ResponseWriter, r *http.Request, _ string) {
	tol := 0.05
	if v := r.URL.Query().Get("tolerance"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 2 {
			writeErr(w, http.StatusBadRequest, errors.New("tolerance must be in [0, 2] degrees"))
			return
		}
		tol = f
	}
	fc, err := s.Engine.RegionGeometry(r.PathValue("id"), tol)
	if err != nil {
		writeRegistryErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache") // regions change at runtime; ETag-free but cheap
	writeJSON(w, http.StatusOK, fc)
}

func (s *Server) laws(w http.ResponseWriter, r *http.Request, _ string) {
	if s.Laws == nil {
		writeErr(w, http.StatusNotFound, errors.New("law directory not loaded"))
		return
	}
	countries := strings.Split(r.URL.Query().Get("countries"), ",")
	found, missing := s.Engine.LawsFor(s.Laws, countries)
	if found == nil {
		found = []json.RawMessage{}
	}
	if missing == nil {
		missing = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"as_of": s.Laws.AsOf, "jurisdictions": found, "not_in_directory": missing})
}

// BatchRequest evaluates many packets in one call (e.g. every path on the globe).
type BatchRequest struct {
	Packets []compliance.Packet `json:"packets"`
	// Publish raises alerts into the alert store/stream. Visualisation
	// clients re-checking the same traffic should leave it false.
	Publish bool `json:"publish"`
}

type batchError struct {
	Index    int    `json:"index"`
	PacketID string `json:"packet_id"`
	Error    string `json:"error"`
}

func (s *Server) evaluateBatch(w http.ResponseWriter, r *http.Request, _ string) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	var req BatchRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Packets) > maxBatch {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("at most %d packets per batch", maxBatch))
		return
	}
	evs := make([]*compliance.Evaluation, 0, len(req.Packets))
	errs := []batchError{}
	for i, p := range req.Packets {
		var ev *compliance.Evaluation
		var err error
		if req.Publish {
			ev, err = s.Engine.Evaluate(r.Context(), p)
		} else {
			ev, err = s.Engine.Preview(r.Context(), p)
		}
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			errs = append(errs, batchError{Index: i, PacketID: p.PacketID, Error: err.Error()})
			continue
		}
		evs = append(evs, ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"evaluations": evs, "errors": errs, "registry_snapshot": s.Engine.Registry.Snapshot().Seq})
}

// ---------- auth ----------

type handler func(w http.ResponseWriter, r *http.Request, actor string)

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func match(tokens map[string]string, tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	for t, actor := range tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			return actor, true
		}
	}
	return "", false
}

func (s *Server) admin(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.AdminTokens) == 0 {
			writeErr(w, http.StatusForbidden, errors.New("admin API disabled: no admin tokens configured"))
			return
		}
		actor, ok := match(s.AdminTokens, bearer(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="compliance-admin"`)
			writeErr(w, http.StatusUnauthorized, errors.New("admin bearer token required"))
			return
		}
		h(w, r, actor)
	}
}

func (s *Server) reader(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if actor, ok := match(s.AdminTokens, tok); ok {
			h(w, r, actor)
			return
		}
		if len(s.ReadTokens) > 0 {
			actor, ok := match(s.ReadTokens, tok)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="compliance"`)
				writeErr(w, http.StatusUnauthorized, errors.New("bearer token required"))
				return
			}
			h(w, r, actor)
			return
		}
		h(w, r, "anonymous")
	}
}

// ---------- profiles ----------

func (s *Server) listProfiles(w http.ResponseWriter, _ *http.Request, _ string) {
	snap := s.Engine.Registry.Snapshot()
	out := make([]compliance.Profile, 0, len(snap.Profiles))
	for _, p := range snap.Profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out, "registry_snapshot": snap.Seq})
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request, _ string) {
	p, ok := s.Engine.Registry.Snapshot().Profiles[r.PathValue("id")]
	if !ok {
		writeErr(w, http.StatusNotFound, compliance.ErrNotFound)
		return
	}
	w.Header().Set("ETag", compliance.ETag(p.ID, p.Version))
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) putProfile(w http.ResponseWriter, r *http.Request, actor string) {
	var p compliance.Profile
	if !decode(w, r, &p) {
		return
	}
	id := r.PathValue("id")
	if p.ID != "" && p.ID != id {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("body id %q does not match path id %q", p.ID, id))
		return
	}
	p.ID = id
	_, existed := s.Engine.Registry.Snapshot().Profiles[id]
	saved, err := s.Engine.Registry.PutProfile(p, r.Header.Get("If-Match"), actor)
	if err != nil {
		writeRegistryErr(w, err)
		return
	}
	w.Header().Set("ETag", compliance.ETag(saved.ID, saved.Version))
	code := http.StatusOK
	if !existed {
		code = http.StatusCreated
	}
	writeJSON(w, code, saved)
}

// patchProfile applies an RFC 7396 JSON merge patch, e.g. {"enabled": false}
// or {"regions": ["eea", "gdpr-adequate"]}.
func (s *Server) patchProfile(w http.ResponseWriter, r *http.Request, actor string) {
	id := r.PathValue("id")
	cur, ok := s.Engine.Registry.Snapshot().Profiles[id]
	if !ok {
		writeErr(w, http.StatusNotFound, compliance.ErrNotFound)
		return
	}
	var patch map[string]any
	if !decode(w, r, &patch) {
		return
	}
	base, _ := json.Marshal(cur)
	var doc map[string]any
	json.Unmarshal(base, &doc)
	for _, k := range []string{"id", "version", "updated_at", "updated_by"} {
		delete(patch, k) // server-managed
	}
	merged, _ := json.Marshal(mergePatch(doc, patch))
	var next compliance.Profile
	if err := json.Unmarshal(merged, &next); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, fmt.Errorf("patched profile: %w", err))
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		ifMatch = compliance.ETag(cur.ID, cur.Version) // patch is relative to what we just read
	}
	saved, err := s.Engine.Registry.PutProfile(next, ifMatch, actor)
	if err != nil {
		writeRegistryErr(w, err)
		return
	}
	w.Header().Set("ETag", compliance.ETag(saved.ID, saved.Version))
	writeJSON(w, http.StatusOK, saved)
}

func mergePatch(doc, patch map[string]any) map[string]any {
	for k, v := range patch {
		if v == nil {
			delete(doc, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			if dm, ok := doc[k].(map[string]any); ok {
				doc[k] = mergePatch(dm, pm)
				continue
			}
		}
		doc[k] = v
	}
	return doc
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request, actor string) {
	if err := s.Engine.Registry.DeleteProfile(r.PathValue("id"), r.Header.Get("If-Match"), actor); err != nil {
		writeRegistryErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- regions ----------

func (s *Server) listRegions(w http.ResponseWriter, _ *http.Request, _ string) {
	snap := s.Engine.Registry.Snapshot()
	out := make([]compliance.Region, 0, len(snap.Regions))
	for _, x := range snap.Regions {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"regions": out, "registry_snapshot": snap.Seq})
}

func (s *Server) getRegion(w http.ResponseWriter, r *http.Request, _ string) {
	x, ok := s.Engine.Registry.Snapshot().Regions[r.PathValue("id")]
	if !ok {
		writeErr(w, http.StatusNotFound, compliance.ErrNotFound)
		return
	}
	w.Header().Set("ETag", compliance.ETag(x.ID, x.Version))
	writeJSON(w, http.StatusOK, x)
}

func (s *Server) putRegion(w http.ResponseWriter, r *http.Request, actor string) {
	var x compliance.Region
	if !decode(w, r, &x) {
		return
	}
	id := r.PathValue("id")
	if x.ID != "" && x.ID != id {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("body id %q does not match path id %q", x.ID, id))
		return
	}
	x.ID = id
	_, existed := s.Engine.Registry.Snapshot().Regions[id]
	saved, err := s.Engine.Registry.PutRegion(x, r.Header.Get("If-Match"), actor)
	if err != nil {
		writeRegistryErr(w, err)
		return
	}
	w.Header().Set("ETag", compliance.ETag(saved.ID, saved.Version))
	code := http.StatusOK
	if !existed {
		code = http.StatusCreated
	}
	writeJSON(w, code, saved)
}

func (s *Server) deleteRegion(w http.ResponseWriter, r *http.Request, actor string) {
	if err := s.Engine.Registry.DeleteRegion(r.PathValue("id"), r.Header.Get("If-Match"), actor); err != nil {
		writeRegistryErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- proxies ----------

func (s *Server) listProxies(w http.ResponseWriter, _ *http.Request, _ string) {
	snap := s.Engine.Registry.Snapshot()
	out := make([]compliance.Proxy, 0, len(snap.Proxies))
	for _, x := range snap.Proxies {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"proxies": out})
}

func (s *Server) putProxy(w http.ResponseWriter, r *http.Request, actor string) {
	var p compliance.Proxy
	if !decode(w, r, &p) {
		return
	}
	p.ID = r.PathValue("id")
	saved, err := s.Engine.Registry.PutProxy(p, actor)
	if err != nil {
		writeRegistryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) deleteProxy(w http.ResponseWriter, r *http.Request, actor string) {
	if err := s.Engine.Registry.DeleteProxy(r.PathValue("id"), actor); err != nil {
		writeRegistryErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- evaluation & alerts ----------

// EvaluateRequest carries either a ready Packet or an enriched InfraMap.
type EvaluateRequest struct {
	Packet          *compliance.Packet `json:"packet,omitempty"`
	InfraMap        *enrich.InfraMap   `json:"inframap,omitempty"`
	PacketID        string             `json:"packet_id,omitempty"`
	Classifications []string           `json:"classifications,omitempty"`
}

func (s *Server) evaluate(w http.ResponseWriter, r *http.Request, _ string) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	var req EvaluateRequest
	if !decode(w, r, &req) {
		return
	}
	var p compliance.Packet
	switch {
	case req.Packet != nil && req.InfraMap != nil:
		writeErr(w, http.StatusBadRequest, errors.New("send packet or inframap, not both"))
		return
	case req.Packet != nil:
		p = *req.Packet
	case req.InfraMap != nil:
		if len(req.Classifications) == 0 {
			writeErr(w, http.StatusBadRequest, errors.New("classifications are required with an inframap"))
			return
		}
		p = adapt.FromInfraMap(req.InfraMap, req.PacketID, req.Classifications)
	default:
		writeErr(w, http.StatusBadRequest, errors.New("body needs packet or inframap"))
		return
	}
	eval := s.Engine.Evaluate
	if r.URL.Query().Get("publish") == "false" {
		eval = s.Engine.Preview
	}
	ev, err := eval(r.Context(), p)
	if errors.Is(err, compliance.ErrBadPacket) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	f := compliance.AlertFilter{ProfileID: q.Get("profile"), Severity: q.Get("severity"), PacketID: q.Get("packet")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be a positive integer"))
			return
		}
		f.Limit = n
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("since must be RFC3339"))
			return
		}
		f.Since = t
	}
	list := s.Alerts.List(f)
	writeJSON(w, http.StatusOK, map[string]any{"alerts": list, "count": len(list)})
}

func (s *Server) streamAlerts(w http.ResponseWriter, r *http.Request, _ string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch, cancel := s.Alerts.Subscribe()
	defer cancel()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case a, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(a)
			fmt.Fprintf(w, "event: alert\nid: %s\ndata: %s\n\n", a.AlertID, b)
			fl.Flush()
		}
	}
}

func (s *Server) locate(w http.ResponseWriter, r *http.Request, _ string) {
	lat, err1 := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lon, err2 := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		writeErr(w, http.StatusBadRequest, errors.New("lat and lon are required"))
		return
	}
	snap := s.Engine.Registry.Snapshot()
	loc := s.Engine.Locator.Locate(compliance.LatLon{Lat: lat, Lon: lon}, r.URL.Query().Get("hint"))
	writeJSON(w, http.StatusOK, map[string]any{"location": loc, "regions": snap.RegionsAt(loc, compliance.LatLon{Lat: lat, Lon: lon})})
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request, _ string) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 1000 {
		n = 100
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": s.Engine.Registry.Audit(n)})
}

// ---------- helpers ----------

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return false
	}
	return true
}

func writeRegistryErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, compliance.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, compliance.ErrConflict):
		writeErr(w, http.StatusPreconditionFailed, err)
	case errors.Is(err, compliance.ErrInUse):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, compliance.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
