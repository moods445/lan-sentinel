package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/home-aio/lan-sentinel/internal/store"
)

var probeHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62})$`)
var nonHostIDRe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// handleNodes 返回「添加节点」所需信息：Agent 一键安装地址与控制面地址，以及面板添加的探测节点。
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	probes, err := s.d.Store.ListNodeTargets(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"install_url":     s.d.InstallURL,
		"server_url":      requestBaseURL(r),
		"agent_token":     s.d.AgentToken,
		"has_agent_token": s.d.AgentToken != "",
		"probes":          probes,
	})
}

type probeNodeBody struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
}

// handleAddProbeNode 为装不了 Agent 的设备创建 probe 节点（仅 TCP 可达性探测）。
func (s *Server) handleAddProbeNode(w http.ResponseWriter, r *http.Request) {
	if s.d.SyncCollectors == nil {
		writeErr(w, http.StatusServiceUnavailable, "collectors disabled (ENABLE_COLLECTORS=0)")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var b probeNodeBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	host, port, err := normalizeProbeAddress(b.Address, b.Port)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := sanitizeHostID(b.Name)
	if id == "" {
		id = sanitizeHostID(host)
	}
	if !hostIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid node name/id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	existing, err := s.d.Store.Host(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing != nil && existing.Kind != "probe" {
		writeErr(w, http.StatusConflict, fmt.Sprintf("host id %q 已被 %s 主机占用，请换一个名称", id, existing.Kind))
		return
	}

	node := store.NodeTarget{
		HostID:    id,
		Name:      firstNonEmpty(strings.TrimSpace(b.Name), host),
		Type:      "probe",
		Target:    net.JoinHostPort(host, strconv.Itoa(port)),
		IntervalS: 30,
	}
	saved, err := s.d.Store.SaveNodeTarget(ctx, node)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	node = saved
	if err := s.d.SyncCollectors(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "sync collectors: "+err.Error())
		return
	}
	if s.d.RunCollector != nil {
		_ = s.d.RunCollector(ctx, id) // 首探失败不影响节点创建
	}
	_ = s.d.Store.Audit(ctx, "panel", "node.add", id, map[string]any{
		"type": "probe", "target": node.Target, "name": node.Name,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": node})
}

func (s *Server) handleDeleteProbeNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !hostIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid host id")
		return
	}
	if s.d.SyncCollectors == nil {
		writeErr(w, http.StatusServiceUnavailable, "collectors disabled (ENABLE_COLLECTORS=0)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.d.Store.DeleteNodeTarget(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.d.SyncCollectors(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "sync collectors: "+err.Error())
		return
	}
	if err := s.d.Store.DeleteProbeHost(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.d.Store.Audit(ctx, "panel", "node.delete", id, map[string]any{"type": "probe"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// normalizeProbeAddress 校验 IP/主机名与端口，返回探测目标。
func normalizeProbeAddress(address string, port int) (string, int, error) {
	addr := strings.TrimSpace(address)
	if addr == "" {
		return "", 0, fmt.Errorf("请填写 IP 或主机名")
	}
	if len(addr) > 128 || strings.ContainsAny(addr, " \t/\\") {
		return "", 0, fmt.Errorf("地址格式不正确")
	}
	host := addr
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host = h
		if n, e := strconv.Atoi(p); e == nil {
			port = n
		}
	} else if strings.Contains(addr, ":") {
		return "", 0, fmt.Errorf("请使用 IPv4 或主机名（可带端口，如 192.168.1.1:80）")
	}
	if port == 0 {
		port = 80
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("端口需在 1-65535 之间")
	}
	if net.ParseIP(host) == nil && !probeHostRe.MatchString(host) {
		return "", 0, fmt.Errorf("地址格式不正确")
	}
	return host, port, nil
}

func sanitizeHostID(s string) string {
	s = strings.TrimSpace(s)
	s = nonHostIDRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "http" || p == "https" {
		scheme = p
	}
	return scheme + "://" + r.Host
}
