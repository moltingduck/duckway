package client

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Fixed categories only: this snapshot never retains request content or origins.
var doctorServices = [...]string{"openai", "openai-chatgpt", "openai-auth", "anthropic"}

type doctorCounts struct {
	Phantom uint64 `json:"phantom"`
	Missing uint64 `json:"missing"`
	Other   uint64 `json:"other"`
}
type doctorSnapshot struct {
	Schema string          `json:"schema"`
	Counts [4]doctorCounts `json:"counts"`
}
type doctorObservations struct {
	mu     sync.Mutex
	counts [4]doctorCounts
}

func (p *httpsProxy) observeDoctor(entry hostEntry, r *http.Request, phantom bool) {
	for i, service := range doctorServices {
		if entry.Service != service {
			continue
		}
		p.doctor.mu.Lock()
		c := &p.doctor.counts[i]
		if phantom {
			c.Phantom++
		} else if strings.TrimSpace(r.Header.Get("Authorization")) == "" && strings.TrimSpace(r.Header.Get("X-Api-Key")) == "" {
			c.Missing++
		} else {
			c.Other++
		}
		p.doctor.mu.Unlock()
		return
	}
}

// An absolute URL targeting this listener must never loop back into diagnostics.
func doctorLocalAuthority(r *http.Request, authority string) bool {
	host, port, err := net.SplitHostPort(authority)
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if err != nil || !ok {
		return false
	}
	_, localPort, err := net.SplitHostPort(local.String())
	ip := net.ParseIP(host)
	return err == nil && port == localPort && (host == "localhost" || (ip != nil && ip.IsLoopback()))
}

func (p *httpsProxy) serveDoctor(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(peer)
	host, port, hostErr := net.SplitHostPort(r.Host)
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	localPort := ""
	if ok {
		_, localPort, _ = net.SplitHostPort(local.String())
	}
	n, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || hostErr != nil || portErr != nil || n < 1 || port != localPort || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) || r.URL.IsAbs() || r.URL.Host != "" || r.RequestURI != "/proxy/doctor" || r.Method != http.MethodGet || p.token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Duckway-Token")), []byte(p.token)) != 1 {
		http.Error(w, "local diagnostics unavailable", http.StatusForbidden)
		return
	}
	p.doctor.mu.Lock()
	snapshot := doctorSnapshot{Schema: "duckway-proxy-doctor-v1", Counts: p.doctor.counts}
	p.doctor.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}
