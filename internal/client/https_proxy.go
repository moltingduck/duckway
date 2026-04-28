package client

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

type serviceInfo struct {
	Name         string `json:"name"`
	HostPattern  string `json:"host_pattern"` // comma-separated list of hosts
	UpstreamURL  string `json:"upstream_url"`
	DeliveryMode string `json:"delivery_mode"` // "proxy" (default) or "loan_proxy"
}

// hostEntry is what the sidecar resolves a request host to: which service it
// belongs to, what delivery mode applies, and the upstream URL for direct
// forwarding (used in loan_proxy mode).
type hostEntry struct {
	Service      string
	DeliveryMode string
	UpstreamURL  string // e.g. https://api.github.com — used for direct forwarding in loan_proxy
}

// loanedToken is a cached real token for a service, plus the auth scheme to
// inject when rewriting outbound requests in loan_proxy mode.
type loanedToken struct {
	realToken     string
	authType      string
	authHeader    string
	authPrefix    string
	placeholderID string
	expiresAt     time.Time
}

type httpsProxy struct {
	serverURL  string
	token      string
	ca         *services.CAManager
	certCache  sync.Map // hostname -> *tls.Certificate
	hostMap    map[string]hostEntry
	httpClient *http.Client

	// loan_proxy state — only populated for services whose delivery_mode is loan_proxy
	loanMu    sync.RWMutex
	loanCache map[string]*loanedToken // keyed by service name

	// audit log batching for loan_proxy traffic
	auditMu      sync.Mutex
	auditBuffer  []auditEntry
	auditClient  *http.Client
}

// RunHTTPSProxy starts the proxy that handles both HTTP and HTTPS CONNECT.
func RunHTTPSProxy(cfg *Config, syncInterval time.Duration) error {
	configDir := DefaultConfigDir()

	// Initial sync
	count, err := SyncKeys(configDir, cfg)
	if err != nil {
		log.Printf("Warning: initial key sync failed: %v", err)
	} else {
		log.Printf("Synced %d placeholder keys", count)
	}

	// Load CA cert + key for MITM
	caDir := filepath.Dir(KeysEnvPath(configDir))
	ca, err := loadClientCA(caDir)
	if err != nil {
		log.Printf("Warning: no CA cert — HTTPS MITM disabled (%v)", err)
		log.Printf("Run 'duckway init' to download the CA cert from the server")
	}

	// Fetch service host map (now per-host with delivery_mode + upstream_url)
	hostMap := fetchServiceHosts(cfg.ServerURL, cfg.Token)
	if len(hostMap) > 0 {
		log.Printf("HTTPS MITM enabled for %d hosts:", len(hostMap))
		for host, e := range hostMap {
			label := "/proxy/" + e.Service + "/"
			if e.DeliveryMode == "loan_proxy" {
				label = "loan_proxy → " + e.UpstreamURL + "  (sidecar caches real token)"
			}
			log.Printf("  %s → %s", host, label)
		}
	}

	// Background sync
	if syncInterval > 0 {
		go func() {
			ticker := time.NewTicker(syncInterval)
			defer ticker.Stop()
			for range ticker.C {
				n, _ := SyncKeys(configDir, cfg)
				log.Printf("Synced %d keys", n)
				// Refresh host map
				if newMap := fetchServiceHosts(cfg.ServerURL, cfg.Token); len(newMap) > 0 {
					hostMap = newMap
				}
			}
		}()
	}

	proxy := &httpsProxy{
		serverURL:   cfg.ServerURL,
		token:       cfg.Token,
		ca:          ca,
		hostMap:     hostMap,
		httpClient:  &http.Client{Timeout: 120 * time.Second},
		loanCache:   make(map[string]*loanedToken),
		auditClient: &http.Client{Timeout: 10 * time.Second},
	}

	// Periodic audit-log flush + cache eviction
	go proxy.auditFlushLoop()

	addr := fmt.Sprintf(":%d", cfg.ProxyPort)
	log.Printf("Duckway proxy listening on %s (HTTP + HTTPS CONNECT)", addr)
	log.Printf("Configure agents with:")
	log.Printf("  export HTTPS_PROXY=http://localhost:%d", cfg.ProxyPort)
	log.Printf("  export HTTP_PROXY=http://localhost:%d", cfg.ProxyPort)

	return http.ListenAndServe(addr, proxy)
}

func (p *httpsProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// Regular HTTP proxy (existing behavior)
	p.handleHTTP(w, r)
}

// handleHTTP forwards regular HTTP requests to the Duckway server.
func (p *httpsProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	targetURL := strings.TrimRight(p.serverURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}

	for key, values := range r.Header {
		for _, v := range values {
			proxyReq.Header.Add(key, v)
		}
	}
	proxyReq.Header.Set("X-Duckway-Token", p.token)

	resp, err := p.httpClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleConnect handles HTTPS CONNECT tunnels.
// For known service hosts: MITM, decrypt, forward via /proxy/{svc}/.
// For unknown hosts: transparent TCP tunnel.
func (p *httpsProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	entry, isMITM := p.hostMap[host]
	if !isMITM || p.ca == nil {
		// Unknown host or no CA: transparent tunnel
		p.tunnelConnect(w, r)
		return
	}

	// MITM: intercept, decrypt, forward via /proxy/{svc}/
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	// Tell client the tunnel is established
	w.WriteHeader(http.StatusOK)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Hijack error: %v", err)
		return
	}
	defer clientConn.Close()

	// Get or create TLS cert for this host
	tlsCert := p.getCert(host)
	if tlsCert == nil {
		log.Printf("Failed to create cert for %s", host)
		return
	}

	// TLS handshake with the client (pretending to be the target host)
	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*tlsCert},
	})
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("TLS handshake error for %s: %v", host, err)
		return
	}
	defer tlsConn.Close()

	// Read decrypted HTTP requests from the client
	reader := bufio.NewReader(tlsConn)
	for {
		tlsConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		req, err := http.ReadRequest(reader)
		if err != nil {
			break
		}

		// Dispatch by delivery mode
		if entry.DeliveryMode == "loan_proxy" {
			p.forwardLoan(tlsConn, req, entry, host)
		} else {
			p.forwardMITM(tlsConn, req, entry.Service, host)
		}
	}
}

func (p *httpsProxy) forwardMITM(tlsConn *tls.Conn, req *http.Request, svcName, host string) {
	path := req.URL.Path
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}

	targetURL := strings.TrimRight(p.serverURL, "/") + "/proxy/" + svcName + path

	var body io.Reader
	if req.Body != nil {
		body = req.Body
	}

	proxyReq, err := http.NewRequest(req.Method, targetURL, body)
	if err != nil {
		writeHTTPError(tlsConn, 502, "proxy error")
		return
	}

	// Copy headers (skip host, connection)
	for key, values := range req.Header {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "connection" || lower == "proxy-connection" {
			continue
		}
		for _, v := range values {
			proxyReq.Header.Add(key, v)
		}
	}
	proxyReq.Header.Set("X-Duckway-Token", p.token)

	resp, err := p.httpClient.Do(proxyReq)
	if err != nil {
		writeHTTPError(tlsConn, 502, "upstream error")
		return
	}
	defer resp.Body.Close()

	resp.Write(tlsConn)
}

// tunnelConnect creates a transparent TCP tunnel for unknown hosts.
func (p *httpsProxy) tunnelConnect(w http.ResponseWriter, r *http.Request) {
	targetConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, "connect failed", http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	go io.Copy(targetConn, clientConn)
	io.Copy(clientConn, targetConn)
}

func (p *httpsProxy) getCert(hostname string) *tls.Certificate {
	if cached, ok := p.certCache.Load(hostname); ok {
		return cached.(*tls.Certificate)
	}

	certPEM, keyPEM, err := p.ca.SignHost(hostname)
	if err != nil {
		log.Printf("Sign host cert error for %s: %v", hostname, err)
		return nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Printf("Parse cert error for %s: %v", hostname, err)
		return nil
	}

	p.certCache.Store(hostname, &cert)
	return &cert
}

func writeHTTPError(w io.Writer, code int, msg string) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\n\r\n%s", code, msg, len(msg), msg)
}

func loadClientCA(configDir string) (*services.CAManager, error) {
	certPath := filepath.Join(configDir, "ca.pem")
	keyPath := filepath.Join(configDir, "ca-key.pem")

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("no CA cert at %s", certPath)
	}

	// Client only needs cert for verification, but for MITM we need the key too.
	// The key is downloaded from the server during init.
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("no CA key at %s", keyPath)
	}

	return parseClientCA(certPEM, keyPEM)
}

func parseClientCA(certPEM, keyPEM []byte) (*services.CAManager, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("failed to decode CA cert")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("failed to decode CA key")
	}

	// Try EC key first, then PKCS8
	ecKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}

	return &services.CAManager{CertPEM: certPEM, KeyPEM: keyPEM, Cert: cert, Key: ecKey}, nil
}

func fetchServiceHosts(serverURL, token string) map[string]hostEntry {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", serverURL+"/client/services", nil)
	req.Header.Set("X-Duckway-Token", token)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Warning: failed to fetch service hosts: %v", err)
		return nil
	}
	defer resp.Body.Close()

	var svcs []serviceInfo
	json.NewDecoder(resp.Body).Decode(&svcs)

	// HostPattern can be a comma-separated list ("api.github.com,github.com")
	// — split and map each host individually so the sidecar matches on raw Host
	// header without any pattern-matching at lookup time.
	hostMap := make(map[string]hostEntry)
	for _, s := range svcs {
		mode := s.DeliveryMode
		if mode == "" {
			mode = "proxy"
		}
		for _, raw := range strings.Split(s.HostPattern, ",") {
			h := strings.TrimSpace(raw)
			if h == "" {
				continue
			}
			hostMap[h] = hostEntry{
				Service:      s.Name,
				DeliveryMode: mode,
				UpstreamURL:  s.UpstreamURL,
			}
		}
	}
	return hostMap
}

// =====================================================================
// loan_proxy: sidecar caches a real token from the gateway and forwards
// requests directly to upstream (gateway out of the data path).
// =====================================================================

type auditEntry struct {
	PlaceholderID string `json:"placeholder_id"`
	Service       string `json:"service"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Status        int    `json:"status"`
}

// getLoanedToken returns a cached real token for the service, fetching a fresh
// one from the gateway if missing or expired.
func (p *httpsProxy) getLoanedToken(svc string) (*loanedToken, error) {
	p.loanMu.RLock()
	tok := p.loanCache[svc]
	p.loanMu.RUnlock()
	if tok != nil && time.Now().Before(tok.expiresAt) {
		return tok, nil
	}

	// Cache miss / expired — request a fresh loan
	req, err := http.NewRequest("GET", p.serverURL+"/client/loan?service="+svc, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", p.token)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loan request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("loan failed (%d): %s", resp.StatusCode, string(body))
	}

	var r struct {
		RealToken     string `json:"real_token"`
		TTLSeconds    int    `json:"ttl_seconds"`
		AuthType      string `json:"auth_type"`
		AuthHeader    string `json:"auth_header"`
		AuthPrefix    string `json:"auth_prefix"`
		PlaceholderID string `json:"placeholder_id"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode loan: %w", err)
	}
	if r.TTLSeconds <= 0 {
		r.TTLSeconds = 60
	}
	tok = &loanedToken{
		realToken:     r.RealToken,
		authType:      r.AuthType,
		authHeader:    r.AuthHeader,
		authPrefix:    r.AuthPrefix,
		placeholderID: r.PlaceholderID,
		// Refresh slightly before the server-side TTL so we never hit an expired token
		expiresAt: time.Now().Add(time.Duration(r.TTLSeconds-2) * time.Second),
	}
	p.loanMu.Lock()
	p.loanCache[svc] = tok
	p.loanMu.Unlock()
	return tok, nil
}

// forwardLoan handles a request in loan_proxy mode: rewrite the auth header
// using the cached real token, then forward DIRECTLY to the upstream — gateway
// is NOT in the data path. This means git pushes / large bodies stream through
// the sidecar without buffering.
func (p *httpsProxy) forwardLoan(tlsConn *tls.Conn, req *http.Request, entry hostEntry, host string) {
	tok, err := p.getLoanedToken(entry.Service)
	if err != nil {
		log.Printf("loan_proxy %s: token loan failed: %v", entry.Service, err)
		writeHTTPError(tlsConn, 502, "duckway loan failed")
		return
	}

	// Build upstream URL: strip our /api prefix if the upstream URL has one
	// (e.g. discord.com/api), use the request path as-is otherwise. For
	// github.com (git operations), upstream URL has no path → just append.
	upstreamBase := strings.TrimRight(entry.UpstreamURL, "/")
	// If the host the agent connected to differs from the upstream host (common
	// for the multi-host case: hostMap key=github.com but upstream=api.github.com),
	// rewrite the upstream base to the host the agent actually wants.
	if !strings.Contains(upstreamBase, host) {
		upstreamBase = "https://" + host
	}
	target := upstreamBase + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}

	// Stream the body — no ReadAll, so git pushes don't buffer in RAM
	upstreamReq, err := http.NewRequest(req.Method, target, req.Body)
	if err != nil {
		writeHTTPError(tlsConn, 502, "upstream request failed")
		return
	}

	// Copy headers, dropping anything that shouldn't propagate. The agent's
	// own Authorization header carries the phantom — we drop it and inject the
	// real one based on the service's auth scheme.
	for key, values := range req.Header {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "connection" || lower == "proxy-connection" ||
			lower == "authorization" || lower == "x-api-key" {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(key, v)
		}
	}

	switch tok.authType {
	case "bearer":
		upstreamReq.Header.Set(tok.authHeader, tok.authPrefix+tok.realToken)
	case "header":
		upstreamReq.Header.Set(tok.authHeader, tok.realToken)
	case "query":
		q := upstreamReq.URL.Query()
		q.Set(tok.authHeader, tok.realToken)
		upstreamReq.URL.RawQuery = q.Encode()
	}

	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		log.Printf("loan_proxy %s: upstream error: %v", entry.Service, err)
		writeHTTPError(tlsConn, 502, "upstream unreachable")
		return
	}
	defer resp.Body.Close()

	// Buffer audit entry — flushed asynchronously to gateway
	p.recordAudit(auditEntry{
		PlaceholderID: tok.placeholderID,
		Service:       entry.Service,
		Method:        req.Method,
		Path:          req.URL.Path,
		Status:        resp.StatusCode,
	})

	// Stream response back to the agent
	resp.Write(tlsConn)
}

func (p *httpsProxy) recordAudit(e auditEntry) {
	p.auditMu.Lock()
	p.auditBuffer = append(p.auditBuffer, e)
	full := len(p.auditBuffer) >= 100
	p.auditMu.Unlock()
	if full {
		go p.flushAudit()
	}
}

func (p *httpsProxy) flushAudit() {
	p.auditMu.Lock()
	if len(p.auditBuffer) == 0 {
		p.auditMu.Unlock()
		return
	}
	batch := p.auditBuffer
	p.auditBuffer = nil
	p.auditMu.Unlock()

	body, _ := json.Marshal(batch)
	req, err := http.NewRequest("POST", p.serverURL+"/client/audit", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Duckway-Token", p.token)
	resp, err := p.auditClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// auditFlushLoop runs continuously: flushes the audit buffer every 30s and
// evicts expired token cache entries (best-effort zero of secret bytes).
func (p *httpsProxy) auditFlushLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		p.flushAudit()
		p.evictExpiredLoans()
	}
}

func (p *httpsProxy) evictExpiredLoans() {
	now := time.Now()
	p.loanMu.Lock()
	for svc, tok := range p.loanCache {
		if now.After(tok.expiresAt) {
			// Best-effort scrub — Go's GC may have copied the string elsewhere,
			// but at least clear the entry our struct points at.
			tok.realToken = ""
			delete(p.loanCache, svc)
		}
	}
	p.loanMu.Unlock()
}
