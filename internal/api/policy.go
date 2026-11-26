package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type policyBody struct {
	EnabledMetrics []string `json:"enabled_metrics"`
	IntervalS      int      `json:"interval_s"`
	Mounts         []string `json:"mounts"`
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	global, hosts, err := s.d.Store.Policies(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"global": global, "hosts": hosts})
}

func (s *Server) handleSaveGlobalPolicy(w http.ResponseWriter, r *http.Request) {
	s.savePolicy(w, r, "global", "")
}

func (s *Server) handleSaveHostPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !hostIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid host id")
		return
	}
	s.savePolicy(w, r, "host", id)
}

func (s *Server) savePolicy(w http.ResponseWriter, r *http.Request, scope, hostID string) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var b policyBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	enabled, err := normalizeMetrics(b.EnabledMetrics)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if b.IntervalS < 0 || b.IntervalS > 3600 {
		writeErr(w, http.StatusBadRequest, "interval_s must be 0..3600")
		return
	}
	if b.IntervalS > 0 && b.IntervalS < 5 {
		writeErr(w, http.StatusBadRequest, "interval_s must be 0 or >= 5")
		return
	}
	mounts, err := normalizeMounts(b.Mounts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := s.d.Store.SavePolicy(ctx, scope, hostID, enabled, b.IntervalS, mounts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.d.Store.Audit(ctx, "panel", "policy.save", hostID, map[string]any{
		"scope": scope, "rev": row.Rev, "enabled_metrics": row.EnabledMetrics,
		"interval_s": row.IntervalS, "mounts": row.Mounts,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "policy": row})
}

func (s *Server) handleDeleteHostPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !hostIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid host id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.d.Store.DeleteHostPolicy(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.d.Store.Audit(ctx, "panel", "policy.delete", id, map[string]any{"scope": "host"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// normalizeMetrics 去重并校验指标名/通配符模式
func normalizeMetrics(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if !metricPatternRe.MatchString(m) {
			return nil, errors.New("invalid metric pattern: " + m)
		}
		if i := strings.IndexByte(m, '*'); i >= 0 && i != len(m)-1 {
			return nil, errors.New("wildcard is only allowed at the end: " + m)
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) > 64 {
		return nil, errors.New("too many metric patterns (max 64)")
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// normalizeMounts 去重并校验挂载点
func normalizeMounts(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if !mountRe.MatchString(m) {
			return nil, errors.New("invalid mount point: " + m)
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) > 3 {
		return nil, errors.New("too many mount points (max 3)")
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
