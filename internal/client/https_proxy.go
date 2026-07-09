package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

// ServiceInfo describes one service the proxy intercepts, as returned by the
// server. Used by FetchServices and displayed by `duckway hosts`.
type ServiceInfo struct {
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
	apiKeyID      string // non-empty when loan came from a key group
	groupID       string // non-empty when loan came from a key group
	expiresAt     time.Time
}

type httpsProxy struct {
	serverURL  string
	token      string
	ca         *services.CAManager
	certCache  sync.Map // hostname -> *tls.Certificate
	hostMu     sync.RWMutex
	hostMap    map[string]hostEntry
	httpClient *http.Client
	debug      bool // when true, log every request/response

	// loan_proxy state — only populated for services whose delivery_mode is loan_proxy
	loanMu    sync.RWMutex
	loanCache map[string]*loanedToken // keyed by service name

	// audit log batching for loan_proxy traffic
	auditMu     sync.Mutex
	auditBuffer []auditEntry
	auditClient *http.Client
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.r != nil && c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}

// RunHTTPSProxy starts the proxy that handles both HTTP and HTTPS CONNECT.
// When debug is true, every request and response is logged via the standard
// log package (stdout in foreground, ~/.duckway/proxy.log in daemon mode
// since the daemon redirects stdout/stderr there).
func RunHTTPSProxy(cfg *Config, syncInterval time.Duration, debug bool) error {
	configDir := DefaultConfigDir()
	updateCtx, cancelUpdateChecks := context.WithCancel(context.Background())
	defer cancelUpdateChecks()
	StartUpdateCheckLoop(updateCtx, cfg, "proxy")

	// Initial sync
	count, err := SyncKeys(configDir, cfg)
	if err != nil {
		log.Printf("Warning: initial key sync failed: %v", err)
	} else {
		log.Printf("Synced %d placeholder keys", count)
	}
	if ch := SyncSupplyChainRC(cfg); len(ch) > 0 {
		log.Printf("Supply-chain hardening: %s", SummarizeSupplyChainChanges(ch))
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

	if debug {
		log.Printf("Debug mode ON — logging every request/response")
	}

	proxy := &httpsProxy{
		serverURL: cfg.ServerURL,
		token:     cfg.Token,
		ca:        ca,
		hostMap:   hostMap,
		httpClient: &http.Client{
			// No Timeout here — streaming SSE responses (compaction, long generations)
			// must not be cut off by a total-request deadline. Connection-level
			// timeouts are set on the Transport instead.
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   4,
			},
		},
		loanCache:   make(map[string]*loanedToken),
		auditClient: &http.Client{Timeout: 10 * time.Second, Transport: directTransport},
		debug:       debug,
	}

	// Periodic audit-log flush + cache eviction
	go proxy.auditFlushLoop()

	// Background sync — keys, supply-chain rc, and host map.
	// Runs after proxy is constructed so reloadHostMap can update proxy.hostMap
	// directly (previous code updated a local variable that proxy never read).
	if syncInterval > 0 {
		go func() {
			ticker := time.NewTicker(syncInterval)
			defer ticker.Stop()
			for range ticker.C {
				n, _ := SyncKeys(configDir, cfg)
				log.Printf("Synced %d keys", n)
				if ch := SyncSupplyChainRC(cfg); SummarizeSupplyChainChanges(ch) != "up to date" {
					log.Printf("Supply-chain hardening: %s", SummarizeSupplyChainChanges(ch))
				}
				proxy.reloadHostMap(cfg.ServerURL, cfg.Token)
			}
		}()
	}

	// SIGUSR1 → immediate host-map reload (used by `duckway hosts reload`).
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGUSR1)
		for range sig {
			log.Printf("SIGUSR1: reloading host map")
			proxy.reloadHostMap(cfg.ServerURL, cfg.Token)
		}
	}()

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
	p.handleHTTP(w, r)
}

// handleHTTP supports both historical direct Duckway paths
// (http://localhost:18080/proxy/openai/...) and real HTTP forward-proxy
// requests (GET http://example.com/path HTTP/1.1). `duckway proxy run` relies
// on the latter when a child process uses HTTP_PROXY for plain HTTP traffic.
func (p *httpsProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.IsAbs() {
		p.handleHTTPForwardProxy(w, r)
		return
	}

	// Historical local-proxy behavior: requests already addressed to the
	// Duckway proxy namespace are forwarded to the Duckway server as-is.
	start := time.Now()
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

	if p.debug {
		p.logDebug("http", "", r, resp, r.Host, time.Since(start))
	}

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (p *httpsProxy) handleHTTPForwardProxy(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	p.hostMu.RLock()
	entry, known := p.hostMap[host]
	p.hostMu.RUnlock()

	if known && entry.Service != "" && (entry.Service == "openai-auth" || entry.Service == "openai-chatgpt" || requestUsesDuckwayPhantom(r)) {
		p.forwardHTTPToGateway(w, r, entry.Service, host)
		return
	}
	p.forwardHTTPDirect(w, r, host, entry)
}

func (p *httpsProxy) forwardHTTPToGateway(w http.ResponseWriter, r *http.Request, svcName, host string) {
	start := time.Now()
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	if r.URL.RawQuery != "" {
		path += "?" + redactDebugRawQuery(r.URL.RawQuery)
	}
	targetURL := strings.TrimRight(p.serverURL, "/") + "/proxy/" + svcName + path

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	copyForwardHeaders(proxyReq.Header, r.Header)
	proxyReq.Header.Set("X-Duckway-Token", p.token)

	resp, err := p.httpClient.Do(proxyReq)
	if err != nil {
		if p.debug {
			log.Printf("[proxy %s] %s http://%s%s → ERR %v (%s)",
				svcName, r.Method, host, path, err, time.Since(start).Round(time.Millisecond))
		}
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if p.debug {
		p.logDebug("proxy", svcName, r, resp, host, time.Since(start))
	}
	copyResponse(w, resp)
}

func (p *httpsProxy) forwardHTTPDirect(w http.ResponseWriter, r *http.Request, host string, entry hostEntry) {
	start := time.Now()
	targetURL := r.URL.String()
	if entry.UpstreamURL != "" && entry.Service != "" {
		upstreamBase := strings.TrimRight(entry.UpstreamURL, "/")
		if upstreamBase == "" || !strings.Contains(upstreamBase, host) {
			upstreamBase = r.URL.Scheme + "://" + r.URL.Host
		}
		targetURL = upstreamBase + r.URL.RequestURI()
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	copyForwardHeaders(upstreamReq.Header, r.Header)

	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		if p.debug {
			log.Printf("[direct %s] %s %s → ERR %v (%s)",
				entry.Service, r.Method, targetURL, err, time.Since(start).Round(time.Millisecond))
		}
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if p.debug {
		p.logDebug("direct", entry.Service, r, resp, host, time.Since(start))
	}
	copyResponse(w, resp)
}

// handleConnect handles HTTPS CONNECT tunnels.
// For known service hosts: MITM, decrypt, forward via /proxy/{svc}/.
// For unknown hosts: transparent TCP tunnel.
func (p *httpsProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	p.hostMu.RLock()
	entry, isMITM := p.hostMap[host]
	p.hostMu.RUnlock()
	if !isMITM {
		if fallback, ok := fallbackMITMEntryForHost(host); ok {
			entry = fallback
			isMITM = true
		}
	}
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

	clientConn, rw, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Hijack error: %v", err)
		return
	}
	defer clientConn.Close()
	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	tunnelConn := &bufferedConn{Conn: clientConn, r: rw.Reader}

	// Get or create TLS cert for this host
	tlsCert := p.getCert(host)
	if tlsCert == nil {
		log.Printf("Failed to create cert for %s", host)
		return
	}

	// TLS handshake with the client (pretending to be the target host).
	//
	// Pin ALPN to HTTP/1.1. This MITM path is HTTP/1.x-only — it parses
	// requests with http.ReadRequest and serializes responses with
	// resp.Write, neither of which speaks HTTP/2. Modern clients (Claude
	// Code / Bun, curl, browsers) offer "h2" in their ALPN list; if we leave
	// NextProtos empty the server selects no protocol and the client is free
	// to proceed with HTTP/2 framing, which we then mis-handle as HTTP/1.1 —
	// surfacing on the agent as intermittent "InvalidHTTPResponse fetching
	// https://api.anthropic.com/v1/messages?beta=true". Advertising only
	// "http/1.1" forces every client to downgrade to the protocol we speak.
	tlsConn := tls.Server(tunnelConn, &tls.Config{
		Certificates: []tls.Certificate{*tlsCert},
		NextProtos:   []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("TLS handshake error for %s: %v", host, err)
		return
	}
	defer tlsConn.Close()

	// Read decrypted HTTP requests from the client
	reader := bufio.NewReader(tlsConn)
	for {
		// Deadline covers only the time to receive the request headers from the
		// client. Clear it before dispatch so upstream streaming responses
		// (LLM SSE, git pack transfers) are not cut short.
		tlsConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		req, err := http.ReadRequest(reader)
		if err != nil {
			break
		}
		tlsConn.SetReadDeadline(time.Time{}) // clear before forwarding

		if entry.Service != "openai-auth" && entry.Service != "openai-chatgpt" && !requestUsesDuckwayPhantom(req) {
			p.forwardDirect(tlsConn, req, host, entry)
			continue
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
	start := time.Now()
	path := req.URL.Path
	if req.URL.RawQuery != "" {
		path += "?" + redactDebugRawQuery(req.URL.RawQuery)
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
		if p.debug {
			log.Printf("[proxy %s] %s %s://%s%s → ERR %v (%s)",
				svcName, req.Method, "https", host, path, err, time.Since(start).Round(time.Millisecond))
		}
		writeHTTPError(tlsConn, 502, "upstream error")
		return
	}
	defer resp.Body.Close()

	if p.debug {
		p.logDebug("proxy", svcName, req, resp, host, time.Since(start))
	}

	resp.Write(tlsConn)
}

func (p *httpsProxy) forwardDirect(tlsConn *tls.Conn, req *http.Request, host string, entry hostEntry) {
	start := time.Now()
	path := req.URL.Path
	if path == "" {
		path = "/"
	}
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	upstreamBase := strings.TrimRight(entry.UpstreamURL, "/")
	if upstreamBase == "" || !strings.Contains(upstreamBase, host) {
		upstreamBase = "https://" + host
	}
	targetURL := upstreamBase + path
	var body io.Reader
	if req.Body != nil {
		body = req.Body
	}
	upstreamReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, body)
	if err != nil {
		writeHTTPError(tlsConn, 502, "proxy error")
		return
	}
	for key, values := range req.Header {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "connection" || lower == "proxy-connection" {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(key, v)
		}
	}
	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		if p.debug {
			log.Printf("[direct %s] %s https://%s%s → ERR %v (%s)",
				entry.Service, req.Method, host, path, err, time.Since(start).Round(time.Millisecond))
		}
		writeHTTPError(tlsConn, 502, "upstream error")
		return
	}
	defer resp.Body.Close()
	if p.debug {
		p.logDebug("direct", entry.Service, req, resp, host, time.Since(start))
	}
	resp.Write(tlsConn)
}

func requestUsesDuckwayPhantom(req *http.Request) bool {
	for _, header := range []string{"Authorization", "X-Api-Key"} {
		for _, value := range req.Header.Values(header) {
			if headerValueUsesDuckwayPhantom(value) {
				return true
			}
		}
	}
	return false
}

func headerValueUsesDuckwayPhantom(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "dw_") || strings.Contains(value, "rt.duckway.") {
		return true
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "basic ") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[len("Basic "):]))
		return err == nil && strings.Contains(string(raw), "dw_")
	}
	if strings.HasPrefix(lower, "bearer ") {
		return tokenUsesDuckwayPhantom(strings.TrimSpace(value[len("Bearer "):]))
	}
	return tokenUsesDuckwayPhantom(value)
}

func tokenUsesDuckwayPhantom(token string) bool {
	if strings.Contains(token, "dw_") || strings.Contains(token, "rt.duckway.") {
		return true
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts[1:] {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err == nil && strings.Contains(string(raw), "dw_") {
			return true
		}
	}
	return false
}

func copyForwardHeaders(dst, src http.Header) {
	for key, values := range src {
		if shouldSkipForwardHeader(key) {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	for key, values := range resp.Header {
		if shouldSkipForwardHeader(key) {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func shouldSkipForwardHeader(key string) bool {
	switch strings.ToLower(key) {
	case "host", "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

// tunnelConnect creates a transparent TCP tunnel for unknown hosts.
func (p *httpsProxy) tunnelConnect(w http.ResponseWriter, r *http.Request) {
	// KeepAlive: 30s — the OS sends keepalive probes after 30 s of inactivity
	// and considers the connection dead after a few unanswered probes (~90 s
	// total). Without this, io.Copy blocks forever when the CDN stops sending
	// mid-download without a FIN/RST (NAT table expiry is the common trigger).
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	targetConn, err := dialer.Dial("tcp", r.Host)
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

	// Hijack before writing the 200 so we bypass Go's ResponseWriter, which
	// appends Transfer-Encoding: chunked — a header RFC 9110 §9.3.6 forbids in
	// CONNECT 200 responses. Some HTTP/2 clients (Bun/undici used by Claude Code)
	// treat the subsequent raw TLS bytes as HTTP chunk data when they see that
	// header, breaking the tunnel without any proxy-side error.
	clientConn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}

	// Two-direction tunnel. Each goroutine closes the *write* side of the
	// opposite connection when it finishes, so the other direction sees EOF
	// and exits cleanly instead of blocking until a TCP timeout.
	go func() {
		io.Copy(targetConn, &bufferedConn{Conn: clientConn, r: rw.Reader})
		targetConn.Close()
	}()
	io.Copy(clientConn, targetConn)
	// clientConn is closed by defer above; targetConn may already be closed
	// by the goroutine, which is fine — double-close on net.Conn is a no-op.
}

func fallbackMITMEntryForHost(host string) (hostEntry, bool) {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "github.com":
		return hostEntry{Service: "github", DeliveryMode: "proxy", UpstreamURL: "https://github.com"}, true
	default:
		return hostEntry{}, false
	}
}

func (p *httpsProxy) getCert(hostname string) *tls.Certificate {
	// Cached cert: only reuse if it's not within 1 hour of expiry. This stops
	// a long-running daemon from handing out an expired cert just because we
	// cached it before it aged out. We pre-parse Leaf for cheap NotAfter access.
	if cached, ok := p.certCache.Load(hostname); ok {
		c := cached.(*tls.Certificate)
		if c.Leaf != nil && time.Now().Add(time.Hour).Before(c.Leaf.NotAfter) {
			return c
		}
		// Stale — drop and regenerate
		p.certCache.Delete(hostname)
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
	// Parse Leaf so the expiry check above doesn't have to re-parse on every call.
	if leaf, perr := x509.ParseCertificate(cert.Certificate[0]); perr == nil {
		cert.Leaf = leaf
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

	// Try raw EC first (SEC1 format), then PKCS8-wrapped EC.
	var ecKey *ecdsa.PrivateKey
	if raw, ecErr := x509.ParseECPrivateKey(keyBlock.Bytes); ecErr == nil {
		ecKey = raw
	} else if p8, p8Err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); p8Err == nil {
		var ok bool
		if ecKey, ok = p8.(*ecdsa.PrivateKey); !ok {
			return nil, fmt.Errorf("parse CA key: PKCS8 key is not EC (type %T)", p8)
		}
	} else {
		return nil, fmt.Errorf("parse CA key: not SEC1 (%v), not PKCS8 (%v)", ecErr, p8Err)
	}

	return &services.CAManager{CertPEM: certPEM, KeyPEM: keyPEM, Cert: cert, Key: ecKey}, nil
}

// FetchServices queries the server for the current interception list.
// Used by `duckway hosts` to display what the proxy intercepts.
func FetchServices(serverURL, token string) ([]ServiceInfo, error) {
	cli := &http.Client{Timeout: 10 * time.Second, Transport: directTransport}
	req, err := http.NewRequest("GET", serverURL+"/client/services", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", token)
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var svcs []ServiceInfo
	if err := json.NewDecoder(resp.Body).Decode(&svcs); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return svcs, nil
}

// buildHostMap expands a ServiceInfo slice into the per-host lookup table.
// HostPattern can be comma-separated ("api.github.com,github.com") — each
// host is mapped individually so handleConnect can look up by raw Host header.
func buildHostMap(svcs []ServiceInfo) map[string]hostEntry {
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
	// Codex OAuth refreshes go to auth.openai.com. Treat it as a virtual
	// service so the client can MITM that host and the gateway can replace the
	// fake refresh token with the real server-side token.
	hostMap["auth.openai.com"] = hostEntry{
		Service:      "openai-auth",
		DeliveryMode: "proxy",
		UpstreamURL:  "https://auth.openai.com",
	}
	// Native Codex OAuth talks to ChatGPT's Codex backend instead of the
	// OpenAI-compatible /v1 Responses API provider. Treat it as a virtual
	// OpenAI service so fake Codex OAuth access tokens are swapped server-side.
	hostMap["chatgpt.com"] = hostEntry{
		Service:      "openai-chatgpt",
		DeliveryMode: "proxy",
		UpstreamURL:  "https://chatgpt.com",
	}
	return hostMap
}

func fetchServiceHosts(serverURL, token string) map[string]hostEntry {
	svcs, err := FetchServices(serverURL, token)
	if err != nil {
		log.Printf("Warning: failed to fetch service hosts: %v", err)
		return nil
	}
	return buildHostMap(svcs)
}

// reloadHostMap fetches the current service list from the server and atomically
// replaces proxy.hostMap. Safe to call from any goroutine concurrently with
// handleConnect.
func (p *httpsProxy) reloadHostMap(serverURL, token string) {
	newMap := fetchServiceHosts(serverURL, token)
	if len(newMap) == 0 {
		return
	}
	p.hostMu.Lock()
	p.hostMap = newMap
	p.hostMu.Unlock()
	log.Printf("Host map refreshed: %d hosts", len(newMap))
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
	req, err := http.NewRequest("GET", p.serverURL+"/client/loan?service="+url.QueryEscape(svc), nil)
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
		APIKeyID      string `json:"api_key_id"`
		GroupID       string `json:"group_id"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode loan: %w", err)
	}
	if r.TTLSeconds <= 0 {
		r.TTLSeconds = 60
	}
	ttl := r.TTLSeconds
	if ttl > 2 {
		ttl -= 2
	}
	tok = &loanedToken{
		realToken:     r.RealToken,
		authType:      r.AuthType,
		authHeader:    r.AuthHeader,
		authPrefix:    r.AuthPrefix,
		placeholderID: r.PlaceholderID,
		apiKeyID:      r.APIKeyID,
		groupID:       r.GroupID,
		expiresAt:     time.Now().Add(time.Duration(ttl) * time.Second),
	}
	p.loanMu.Lock()
	// Another goroutine may have populated the cache while we were fetching;
	// only overwrite if still stale to avoid discarding a fresher token.
	if existing := p.loanCache[svc]; existing == nil || time.Now().After(existing.expiresAt) {
		p.loanCache[svc] = tok
	}
	p.loanMu.Unlock()
	return tok, nil
}

// forwardLoan handles a request in loan_proxy mode: rewrite the auth header
// using the cached real token, then forward DIRECTLY to the upstream — gateway
// is NOT in the data path. This means git pushes / large bodies stream through
// the sidecar without buffering.
func (p *httpsProxy) forwardLoan(tlsConn *tls.Conn, req *http.Request, entry hostEntry, host string) {
	start := time.Now()
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

	// Buffer small request bodies so we can replay them on a 429 retry.
	// Large bodies (git pushes, uploads) are streamed and cannot be replayed.
	const maxBodyBuffer = 1 << 20 // 1 MiB
	var bodyBytes []byte
	if req.Body != nil && req.ContentLength >= 0 && req.ContentLength <= maxBodyBuffer {
		bodyBytes, _ = io.ReadAll(io.LimitReader(req.Body, maxBodyBuffer+1))
		req.Body.Close()
	}
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	} else {
		bodyReader = req.Body // streaming — retry will be skipped
	}

	upstreamReq, err := http.NewRequest(req.Method, target, bodyReader)
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

	if p.debug {
		p.logDebug("loan", entry.Service, req, resp, host, time.Since(start))
	}

	// Capture Anthropic rate-limit headers and report them asynchronously.
	rateLimitHeaders := map[string]string{}
	for _, h := range []string{
		"x-ratelimit-limit-requests", "x-ratelimit-remaining-requests",
		"x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens",
		"x-ratelimit-reset-tokens",
	} {
		if v := resp.Header.Get(h); v != "" {
			rateLimitHeaders[h] = v
		}
	}
	if len(rateLimitHeaders) > 0 && tok.apiKeyID != "" {
		go p.reportUsage(tok.apiKeyID, rateLimitHeaders)
	}

	// Handle 429: mark the key exhausted, re-select from the group, retry.
	if resp.StatusCode == http.StatusTooManyRequests && tok.groupID != "" {
		resetAt := resp.Header.Get("x-ratelimit-reset-tokens")
		if resetAt != "" {
			go p.markExhausted(tok.groupID, tok.apiKeyID, resetAt)
		}
		// Evict cached loan so the next call fetches a fresh key from the group.
		p.loanMu.Lock()
		delete(p.loanCache, entry.Service)
		p.loanMu.Unlock()
		// Retry with the exhausted key excluded.
		newTok, retryErr := p.getLoanedTokenExcluding(entry.Service, tok.groupID, tok.apiKeyID)
		if retryErr == nil && bodyBytes != nil {
			tok = newTok
			// Re-build and send upstream request with new token.
			retryResp := p.retryWithToken(req, bytes.NewReader(bodyBytes), target, tok, entry)
			if retryResp != nil {
				defer retryResp.Body.Close()
				retryResp.Header.Set("Connection", "close")
				retryResp.Close = true
				retryResp.Write(tlsConn)
				return
			}
		}
		// Retry failed — fall through and return the original 429.
	}

	// Buffer audit entry — flushed asynchronously to gateway
	p.recordAudit(auditEntry{
		PlaceholderID: tok.placeholderID,
		Service:       entry.Service,
		Method:        req.Method,
		Path:          req.URL.Path,
		Status:        resp.StatusCode,
	})

	// Force connection close so single-shot clients (curl, one-off git ops)
	// exit cleanly after reading the body. Without this the agent's TLS
	// connection stays open up to the 30s read deadline waiting for another
	// request, which makes curl --max-time appear to hang.
	resp.Header.Set("Connection", "close")
	resp.Close = true
	resp.Write(tlsConn)
}

// reportUsage sends captured rate-limit headers to /client/usage asynchronously.
func (p *httpsProxy) reportUsage(apiKeyID string, headers map[string]string) {
	body, _ := json.Marshal(map[string]interface{}{
		"api_key_id": apiKeyID,
		"headers":    headers,
	})
	req, err := http.NewRequest("POST", p.serverURL+"/client/usage", bytes.NewReader(body))
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

// markExhausted notifies the server that a group key is exhausted.
func (p *httpsProxy) markExhausted(groupID, apiKeyID, resetAt string) {
	body, _ := json.Marshal(map[string]string{
		"group_id":   groupID,
		"api_key_id": apiKeyID,
		"reset_at":   resetAt,
	})
	req, err := http.NewRequest("POST", p.serverURL+"/client/loan/exhaust", bytes.NewReader(body))
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

// getLoanedTokenExcluding fetches a fresh group loan excluding a specific key.
func (p *httpsProxy) getLoanedTokenExcluding(svc, groupID, excludeKeyID string) (*loanedToken, error) {
	loanURL := p.serverURL + "/client/loan?service=" + url.QueryEscape(svc) + "&group=" + url.QueryEscape(groupID) + "&exclude_key=" + url.QueryEscape(excludeKeyID)
	req, err := http.NewRequest("GET", loanURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", p.token)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("loan retry failed (%d): %s", resp.StatusCode, string(body))
	}
	var r struct {
		RealToken  string `json:"real_token"`
		TTLSeconds int    `json:"ttl_seconds"`
		AuthType   string `json:"auth_type"`
		AuthHeader string `json:"auth_header"`
		AuthPrefix string `json:"auth_prefix"`
		APIKeyID   string `json:"api_key_id"`
		GroupID    string `json:"group_id"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.TTLSeconds <= 0 {
		r.TTLSeconds = 60
	}
	ttl2 := r.TTLSeconds
	if ttl2 > 2 {
		ttl2 -= 2
	}
	tok := &loanedToken{
		realToken:  r.RealToken,
		authType:   r.AuthType,
		authHeader: r.AuthHeader,
		authPrefix: r.AuthPrefix,
		apiKeyID:   r.APIKeyID,
		groupID:    r.GroupID,
		expiresAt:  time.Now().Add(time.Duration(ttl2) * time.Second),
	}
	p.loanMu.Lock()
	p.loanCache[svc] = tok
	p.loanMu.Unlock()
	return tok, nil
}

// retryWithToken re-sends a request with a new loaned token. Returns the response or nil on error.
func (p *httpsProxy) retryWithToken(orig *http.Request, body io.Reader, target string, tok *loanedToken, entry hostEntry) *http.Response {
	retryReq, err := http.NewRequest(orig.Method, target, body)
	if err != nil {
		return nil
	}
	for key, values := range orig.Header {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "connection" || lower == "proxy-connection" ||
			lower == "authorization" || lower == "x-api-key" {
			continue
		}
		for _, v := range values {
			retryReq.Header.Add(key, v)
		}
	}
	switch tok.authType {
	case "bearer":
		retryReq.Header.Set(tok.authHeader, tok.authPrefix+tok.realToken)
	case "header":
		retryReq.Header.Set(tok.authHeader, tok.realToken)
	case "query":
		q := retryReq.URL.Query()
		q.Set(tok.authHeader, tok.realToken)
		retryReq.URL.RawQuery = q.Encode()
	}
	resp, err := p.httpClient.Do(retryReq)
	if err != nil {
		log.Printf("loan_proxy %s: retry upstream error: %v", entry.Service, err)
		return nil
	}
	return resp
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

// logDebug emits a one-line summary of a proxied request/response.
//
// Format:
//
//	[mode service] METHOD https://host/path → 200 (req=1.2KB → resp=4.8KB, 543ms) ct=application/json
//
// mode is "proxy", "loan", or "http". service is the duckway service name
// (empty for plain HTTP). req size is the agent's Content-Length when set;
// resp size comes from the upstream's Content-Length (may be empty for
// chunked responses).
func (p *httpsProxy) logDebug(mode, service string, req *http.Request, resp *http.Response, host string, dur time.Duration) {
	tag := "[" + mode
	if service != "" {
		tag += " " + service
	}
	tag += "]"

	reqSize := humanSize(req.ContentLength)
	respSize := humanSize(resp.ContentLength)

	path := req.URL.Path
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "-"
	}

	log.Printf("%s %s https://%s%s → %d (req=%s → resp=%s, %s) ct=%s",
		tag, req.Method, host, path, resp.StatusCode,
		reqSize, respSize, dur.Round(time.Millisecond), ct)
}

func redactDebugRawQuery(rawQuery string) string {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "[redacted]"
	}
	for key, vals := range values {
		for i, val := range vals {
			if isSensitiveQueryKey(key) || services.IsPlaceholder(val) || looksLikeSensitiveTokenValue(val) {
				vals[i] = "[redacted]"
			}
		}
		values[key] = vals
	}
	return values.Encode()
}

func isSensitiveQueryKey(key string) bool {
	switch strings.ToLower(key) {
	case "access_token", "token", "key", "api_key", "client_secret", "refresh_token", "id_token":
		return true
	default:
		return false
	}
}

func looksLikeSensitiveTokenValue(value string) bool {
	for _, prefix := range []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "sk-", "sk-ant-", "xoxb-", "rt."} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func humanSize(n int64) string {
	if n < 0 {
		return "?"
	}
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}
