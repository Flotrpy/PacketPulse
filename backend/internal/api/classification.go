package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/flotrpy/packetpulse/backend/internal/classify"
)

func (s *Server) getRules(w http.ResponseWriter, _ *http.Request) {
	set := s.Classifier.Rules()
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(set.Version)))
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) putRules(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ingest-admin"`)
		writeErr(w, http.StatusUnauthorized, errors.New("admin bearer token required"))
		return
	}
	var set classify.RuleSet
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	ifVersion := 0
	if im := strings.Trim(r.Header.Get("If-Match"), `" `); im != "" {
		v, err := strconv.Atoi(im)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("If-Match must be the rules version"))
			return
		}
		ifVersion = v
	}
	saved, err := s.Classifier.Replace(set, ifVersion, actor)
	switch {
	case errors.Is(err, classify.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, err)
	case errors.Is(err, classify.ErrConflict):
		writeErr(w, http.StatusPreconditionFailed, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		w.Header().Set("ETag", strconv.Quote(strconv.Itoa(saved.Version)))
		writeJSON(w, http.StatusOK, saved)
	}
}

// ClassifyTest is the POST /v1/classification/test body: a flow to classify
// without tracing it.
type ClassifyTest struct {
	Target              string   `json:"target"` // hostname or IP
	IP                  string   `json:"ip,omitempty"`
	ASN                 int      `json:"asn,omitempty"`
	CloudProvider       string   `json:"cloud_provider,omitempty"`
	App                 string   `json:"app,omitempty"`
	SNI                 string   `json:"sni,omitempty"`
	Port                int      `json:"port,omitempty"`
	DataClassifications []string `json:"data_classifications,omitempty"`
}

func (s *Server) testRules(w http.ResponseWriter, r *http.Request) {
	var t ClassifyTest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	in := classify.Input{ASN: t.ASN, Cloud: t.CloudProvider, App: t.App, SNI: t.SNI, Port: t.Port}
	if a, err := netip.ParseAddr(t.Target); err == nil {
		in.IP = a
	} else {
		in.Host = t.Target
	}
	if t.IP != "" {
		a, err := netip.ParseAddr(t.IP)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("bad ip %q", t.IP))
			return
		}
		in.IP = a
	}
	if in.IP.IsValid() && s.Ref != nil && (in.ASN == 0 || in.Cloud == "") {
		if info, ok := s.Ref.LookupASN(in.IP); ok {
			if in.ASN == 0 {
				in.ASN = info.ASN
			}
			if p, ok := s.Ref.CloudProvider(info.ASN); ok && in.Cloud == "" {
				in.Cloud = p
			}
		}
	}
	writeJSON(w, http.StatusOK, s.Classifier.Classify(in, t.DataClassifications))
}

func (s *Server) adminActor(r *http.Request) (string, bool) {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || t == "" {
		return "", false
	}
	for tok, actor := range s.AdminTokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			return actor, true
		}
	}
	return "", false
}
