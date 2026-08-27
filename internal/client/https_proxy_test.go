package client

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

func readConnectResponse(t *testing.T, raw net.Conn) *http.Response {
	t.Helper()
	var buf []byte
	tmp := make([]byte, 1)
	for !bytes.HasSuffix(buf, []byte("\r\n\r\n")) {
		if len(buf) > 8192 {
			t.Fatalf("CONNECT response headers too large")
		}
		if _, err := raw.Read(tmp); err != nil {
			t.Fatalf("read CONNECT response: %v", err)
		}
		buf = append(buf, tmp[0])
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf)), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("parse CONNECT response: %v", err)
	}
	return resp
}

func TestRedactDebugRawQuery(t *testing.T) {
	got := redactDebugRawQuery("access_token=ghs_real_secret&ok=value&token=github_pat_real_secret&x=sk-ant-real&y=xai-real-secret&phantom=github_pat_dw_fake")
	for _, secret := range []string{"ghs_real_secret", "github_pat_real_secret", "sk-ant-real", "xai-real-secret", "github_pat_dw_fake"} {
		if bytes.Contains([]byte(got), []byte(secret)) {
			t.Fatalf("query leaked %q in %q", secret, got)
		}
	}
	for _, want := range []string{"access_token=%5Bredacted%5D", "token=%5Bredacted%5D", "x=%5Bredacted%5D", "y=%5Bredacted%5D", "phantom=%5Bredacted%5D", "ok=value"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("query %q missing %q", got, want)
		}
	}
	redactedURL := redactedDebugURL("https://github.com/OWNER/REPO?access_token=ghs_real_secret&ok=value")
	if bytes.Contains([]byte(redactedURL), []byte("ghs_real_secret")) {
		t.Fatalf("url leaked token: %s", redactedURL)
	}
	if !bytes.Contains([]byte(redactedURL), []byte("ok=value")) {
		t.Fatalf("url should keep non-sensitive query values: %s", redactedURL)
	}
	redactedUserinfoURL := redactedDebugURL("https://x-access-token:github_pat_real_secret@github.com/OWNER/REPO.git?ok=1")
	if bytes.Contains([]byte(redactedUserinfoURL), []byte("github_pat_real_secret")) || bytes.Contains([]byte(redactedUserinfoURL), []byte("x-access-token")) {
		t.Fatalf("url leaked userinfo credential: %s", redactedUserinfoURL)
	}
	if !bytes.Contains([]byte(redactedUserinfoURL), []byte("ok=1")) {
		t.Fatalf("url should keep non-sensitive query values: %s", redactedUserinfoURL)
	}
}

func TestFetchServicesReportsHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad token"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	_, err := FetchServices(srv.URL, "bad")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "server returned 401") || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFallbackMITMEntryForGitHub(t *testing.T) {
	entry, ok := fallbackMITMEntryForHost("github.com")
	if !ok {
		t.Fatal("github.com should have a built-in MITM fallback")
	}
	if entry.Service != "github" || entry.DeliveryMode != "proxy" {
		t.Fatalf("fallback = %+v, want github proxy", entry)
	}
	if _, ok := fallbackMITMEntryForHost("example.com"); ok {
		t.Fatal("example.com should not have a built-in MITM fallback")
	}
}

// newTestMITMProxy wires an httpsProxy that MITMs api.anthropic.com and
// forwards to the given backend (standing in for the duckway server's
// /proxy/{svc}/ endpoint). Returns the proxy's listen address.
func newTestMITMProxy(t *testing.T, backendURL string) (*httpsProxy, *x509.CertPool, string) {
	t.Helper()

	ca, err := services.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		t.Fatal("append CA cert to pool")
	}

	p := &httpsProxy{
		serverURL: backendURL,
		token:     "test-token",
		ca:        ca,
		hostMap: map[string]hostEntry{"api.anthropic.com": {
			Service: "anthropic", DeliveryMode: "proxy", AssignmentKnown: true, Assigned: true,
		}},
		httpClient:  &http.Client{Transport: directTransport},
		loanCache:   make(map[string]*loanedToken),
		auditClient: &http.Client{Timeout: time.Second},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: p}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	return p, pool, ln.Addr().String()
}

// dialMITM performs CONNECT through the proxy and completes a TLS handshake
// offering the given ALPN protocols. Returns the established *tls.Conn.
func dialMITM(t *testing.T, proxyAddr string, alpn []string, rootCAs *x509.CertPool) *tls.Conn {
	t.Helper()
	return dialMITMHost(t, proxyAddr, "api.anthropic.com:443", "api.anthropic.com", alpn, rootCAs)
}

func dialMITMHost(t *testing.T, proxyAddr, connectHost, serverName string, alpn []string, rootCAs *x509.CertPool) *tls.Conn {
	t.Helper()
	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}

	fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", connectHost, connectHost)
	resp := readConnectResponse(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	tlsConn := tls.Client(raw, &tls.Config{
		ServerName: serverName,
		RootCAs:    rootCAs,
		NextProtos: alpn,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	t.Cleanup(func() { tlsConn.Close() })
	return tlsConn
}

// TestMITMPinsHTTP1 verifies that a client offering HTTP/2 in its ALPN list is
// forced down to http/1.1 — the only protocol the MITM forward path speaks.
// Regression test for intermittent "InvalidHTTPResponse" when an h2-capable
// client (Claude Code / Bun) was allowed to use HTTP/2 framing the proxy then
// serialized as HTTP/1.1.
func TestMITMPinsHTTP1(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	_, pool, proxyAddr := newTestMITMProxy(t, backend.URL)

	// Client offers h2 first, then http/1.1 — exactly what Bun/Chrome send.
	conn := dialMITM(t, proxyAddr, []string{"h2", "http/1.1"}, pool)

	if got := conn.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("negotiated ALPN = %q, want \"http/1.1\"", got)
	}
}

// TestTunnelPassesThroughNonMITMHost verifies that a host NOT in the MITM map
// — e.g. downloads.claude.ai, where `claude --update` fetches release binaries
// and manifests — is transparently tunneled end-to-end (real TLS, no MITM, no
// key injection). This is the path `claude --update` traffic takes when the
// agent's HTTPS_PROXY points at the duckway proxy; if tunnelConnect mishandled
// the CONNECT handshake, updates would break.
func TestTunnelPassesThroughNonMITMHost(t *testing.T) {
	// Origin stands in for downloads.claude.ai — its own TLS cert, never MITM'd.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("release-binary-bytes"))
	}))
	defer origin.Close()
	originHost := origin.Listener.Addr().String() // 127.0.0.1:port — not in hostMap

	// hostMap only knows api.anthropic.com, so originHost takes the tunnel path.
	_, _, proxyAddr := newTestMITMProxy(t, "http://unused.invalid")

	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer raw.Close()

	fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", originHost, originHost)
	connResp := readConnectResponse(t, raw)
	if connResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connResp.StatusCode)
	}
	// RFC 9110 §9.3.6 forbids Transfer-Encoding and Content-Length in a
	// successful CONNECT response. Bun/undici (used by Claude Code) treats any
	// such header as applying to the tunnel data, causing it to parse raw TLS
	// bytes as HTTP chunks — which makes `claude update` fail with "canceled".
	if te := connResp.Header.Get("Transfer-Encoding"); te != "" {
		t.Errorf("CONNECT 200 must not include Transfer-Encoding (got %q); violates RFC 9110 §9.3.6", te)
	}
	if cl := connResp.Header.Get("Content-Length"); cl != "" {
		t.Errorf("CONNECT 200 must not include Content-Length (got %q); violates RFC 9110 §9.3.6", cl)
	}

	// Real end-to-end TLS with the origin's own cert proves there's no MITM in
	// the middle: the duckway CA is irrelevant here, only the origin cert is.
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	tlsConn := tls.Client(raw, &tls.Config{ServerName: "example.com", RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("end-to-end TLS handshake through tunnel: %v", err)
	}
	defer tlsConn.Close()

	fmt.Fprintf(tlsConn, "GET /claude-code-releases/stable HTTP/1.1\r\nHost: downloads.claude.ai\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("read origin response through tunnel: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "release-binary-bytes" {
		t.Fatalf("tunneled body = %q, want \"release-binary-bytes\"", string(body))
	}
}

func TestTunnelOnlyHostPreservesWebSocketUpgradeAndStream(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("origin does not support hijacking")
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("origin hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(rw, payload); err != nil {
			t.Errorf("origin read stream: %v", err)
			return
		}
		_, _ = rw.Write(payload)
		_ = rw.Flush()
	}))
	defer origin.Close()
	originHost := origin.Listener.Addr().String()
	host, port, err := net.SplitHostPort(originHost)
	if err != nil {
		t.Fatalf("split origin host: %v", err)
	}

	p, _, proxyAddr := newTestMITMProxy(t, "http://unused.invalid")
	p.hostMap[host] = hostEntry{Service: "discord", TunnelOnly: true, TunnelPort: port}

	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer raw.Close()
	_, _ = fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", originHost, originHost)
	if resp := readConnectResponse(t, raw); resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	tlsConn := tls.Client(raw, &tls.Config{ServerName: "example.com", RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("end-to-end TLS handshake: %v", err)
	}
	defer tlsConn.Close()

	_, _ = fmt.Fprintf(tlsConn, "GET /?v=10 HTTP/1.1\r\nHost: gateway.discord.gg\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1rZXk=\r\n\r\n")
	reader := bufio.NewReader(tlsConn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", resp.StatusCode)
	}
	if _, err := tlsConn.Write([]byte("ping")); err != nil {
		t.Fatalf("write upgraded stream: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil {
		t.Fatalf("read upgraded stream: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("upgraded stream echo = %q, want ping", echo)
	}
}

func TestTunnelOnlyHostRejectsUnexpectedPort(t *testing.T) {
	p := &httpsProxy{hostMap: map[string]hostEntry{
		"gateway.discord.gg": {Service: "discord", TunnelOnly: true, TunnelPort: "443"},
	}}
	req := httptest.NewRequest(http.MethodConnect, "http://gateway.discord.gg:80", nil)
	req.Host = "GATEWAY.DISCORD.GG.:80"
	rec := httptest.NewRecorder()
	p.handleConnect(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("CONNECT status = %d, want 403", rec.Code)
	}
}

func TestOpenAIVirtualHostOnlyUsesGatewayForPhantomCredential(t *testing.T) {
	var gatewayHits int
	var directHits int
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if strings.HasPrefix(req.URL.Path, "/proxy/openai-auth/") {
			gatewayHits++
			status = http.StatusNoContent
		} else {
			directHits++
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})

	p := &httpsProxy{
		serverURL: "http://duckway.test",
		token:     "client-token",
		hostMap: map[string]hostEntry{"auth.openai.com": {
			Service: "openai-auth", UpstreamURL: "https://auth.openai.com", AssignmentKnown: true, Assigned: true,
		}},
		httpClient: &http.Client{Transport: transport},
	}

	directReq := httptest.NewRequest(http.MethodPost, "http://auth.openai.com/api/accounts/deviceauth/usercode", nil)
	directRec := httptest.NewRecorder()
	p.handleHTTPForwardProxy(directRec, directReq)
	if directRec.Code != http.StatusOK || directHits != 1 || gatewayHits != 0 {
		t.Fatalf("non-phantom route: status=%d direct=%d gateway=%d", directRec.Code, directHits, gatewayHits)
	}

	phantomReq := httptest.NewRequest(
		http.MethodPost,
		"http://auth.openai.com/oauth/token",
		strings.NewReader(`{"grant_type":"refresh_token","refresh_token":"rt.duckway.placeholder"}`),
	)
	phantomReq.Header.Set("Content-Type", "application/json")
	phantomRec := httptest.NewRecorder()
	p.handleHTTPForwardProxy(phantomRec, phantomReq)
	if phantomRec.Code != http.StatusNoContent || directHits != 1 || gatewayHits != 1 {
		t.Fatalf("phantom route: status=%d direct=%d gateway=%d", phantomRec.Code, directHits, gatewayHits)
	}

	for _, tc := range []struct {
		name  string
		entry hostEntry
		want  int
	}{
		{name: "unassigned", entry: hostEntry{Service: "openai-auth", AssignmentKnown: true}, want: http.StatusForbidden},
		{name: "unknown", entry: hostEntry{Service: "openai-auth"}, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.hostMap["auth.openai.com"] = tc.entry
			req := httptest.NewRequest(
				http.MethodPost,
				"http://auth.openai.com/oauth/token",
				strings.NewReader(`{"refresh_token":"rt.duckway.placeholder"}`),
			)
			rec := httptest.NewRecorder()
			p.handleHTTPForwardProxy(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if directHits != 1 || gatewayHits != 1 {
				t.Fatalf("rejected phantom escaped locally: direct=%d gateway=%d", directHits, gatewayHits)
			}
		})
	}
}

func TestOpenAIRefreshPhantomDetectionPreservesRequestBody(t *testing.T) {
	entry := hostEntry{Service: "openai-auth"}
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{name: "json phantom", contentType: "application/json", body: `{"grant_type":"refresh_token","refresh_token":"rt.duckway.dw_openai"}`, want: true},
		{name: "form phantom", contentType: "application/x-www-form-urlencoded", body: "grant_type=refresh_token&refresh_token=rt.duckway.dw_openai", want: true},
		{name: "real token", contentType: "application/json", body: `{"grant_type":"refresh_token","refresh_token":"rt.1.real"}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://auth.openai.com/oauth/token", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			if got := requestUsesDuckwayPhantom(entry, req); got != tc.want {
				t.Fatalf("phantom detection = %v, want %v", got, tc.want)
			}
			gotBody, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read restored body: %v", err)
			}
			if string(gotBody) != tc.body {
				t.Fatalf("restored body = %q, want %q", gotBody, tc.body)
			}
		})
	}
}

// TestMITMRoundTripsBody verifies an HTTP/1.1 request over the MITM tunnel is
// forwarded to the backend and the response body is returned intact.
func TestMITMRoundTripsBody(t *testing.T) {
	var gotPath string
	var mu sync.Mutex
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello-from-upstream"))
	}))
	defer backend.Close()

	_, pool, proxyAddr := newTestMITMProxy(t, backend.URL)
	conn := dialMITM(t, proxyAddr, []string{"http/1.1"}, pool)

	fmt.Fprintf(conn, "GET /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer sk-ant-dw_fake\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-upstream" {
		t.Fatalf("body = %q, want \"hello-from-upstream\"", string(body))
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/proxy/anthropic/v1/messages?beta=true" {
		t.Fatalf("backend saw path %q, want \"/proxy/anthropic/v1/messages?beta=true\"", gotPath)
	}
}

func TestHTTPForwardProxyRoutesKnownPhantomThroughGateway(t *testing.T) {
	var gotPath, gotAuth string
	var mu sync.Mutex
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte("gateway-http"))
	}))
	defer gateway.Close()

	p := &httpsProxy{
		serverURL: gateway.URL,
		token:     "test-token",
		hostMap: map[string]hostEntry{
			"api.anthropic.com": {
				Service: "anthropic", DeliveryMode: "proxy", UpstreamURL: "https://api.anthropic.com",
				AssignmentKnown: true, Assigned: true,
			},
		},
		httpClient: &http.Client{Transport: directTransport},
		loanCache:  make(map[string]*loanedToken),
	}
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	req, err := http.NewRequest(http.MethodGet, "http://api.anthropic.com/v1/messages?access_token=ghs_secret&beta=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant-dw_fake")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("forward proxy request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "gateway-http" {
		t.Fatalf("body = %q, want gateway-http", string(body))
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/proxy/anthropic/v1/messages?access_token=ghs_secret&beta=true" {
		t.Fatalf("gateway path = %q, want sensitive query forwarded unmodified", gotPath)
	}
	if gotAuth != "Bearer sk-ant-dw_fake" {
		t.Fatalf("gateway Authorization = %q", gotAuth)
	}
}

func TestHTTPForwardProxyDirectsUnknownAbsoluteURL(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/plain-http" {
			t.Fatalf("origin path = %q, want /plain-http", got)
		}
		w.Write([]byte("direct-http-origin"))
	}))
	defer origin.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gateway should not be used", http.StatusBadGateway)
	}))
	defer gateway.Close()

	p := &httpsProxy{
		serverURL:  gateway.URL,
		token:      "test-token",
		hostMap:    map[string]hostEntry{},
		httpClient: &http.Client{Transport: directTransport},
		loanCache:  make(map[string]*loanedToken),
	}
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(origin.URL + "/plain-http")
	if err != nil {
		t.Fatalf("forward proxy direct request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "direct-http-origin" {
		t.Fatalf("body = %q, want direct-http-origin", string(body))
	}
}

func TestMITMDirectsManagedGitHubSmartHTTPWithRealCredential(t *testing.T) {
	var gatewayHits int
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayHits++
		http.Error(w, "gateway should not be used", http.StatusBadGateway)
	}))
	defer gateway.Close()

	var originHits int
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits++
		w.Write([]byte("direct-origin"))
	}))
	defer origin.Close()
	_, originPort, err := net.SplitHostPort(origin.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	originHost := "localhost:" + originPort

	ca, err := services.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		t.Fatal("append CA cert to pool")
	}
	p := &httpsProxy{
		serverURL: gateway.URL,
		token:     "test-token",
		ca:        ca,
		hostMap:   map[string]hostEntry{"localhost": {Service: "github", DeliveryMode: "proxy", UpstreamURL: "https://" + originHost}},
		// Test-only client for httptest's self-signed origin certificate.
		httpClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}, //nolint:gosec
		loanCache:  make(map[string]*loanedToken),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: p}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	connectHost := originHost
	serverName := "localhost"
	conn := dialMITMHost(t, ln.Addr().String(), connectHost, serverName, []string{"http/1.1"}, pool)
	fmt.Fprintf(conn, "GET /OWNER/REPO.git/info/refs HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer real-user-token\r\nConnection: close\r\n\r\n", connectHost)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "direct-origin" {
		t.Fatalf("status = %d body=%q, want direct origin", resp.StatusCode, body)
	}
	if gatewayHits != 0 {
		t.Fatalf("gateway hits = %d, want 0", gatewayHits)
	}
	if originHits != 1 {
		t.Fatalf("origin hits = %d, want 1", originHits)
	}
}

func TestMITMProxiesNativeCodexChatGPTPhantomTraffic(t *testing.T) {
	var gotPath, gotAuth string
	var mu sync.Mutex
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte("gateway-chatgpt"))
	}))
	defer gateway.Close()

	ca, err := services.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		t.Fatal("append CA cert to pool")
	}
	p := &httpsProxy{
		serverURL: gateway.URL,
		token:     "test-token",
		ca:        ca,
		hostMap: map[string]hostEntry{
			"chatgpt.com": {
				Service: "openai-chatgpt", DeliveryMode: "proxy", UpstreamURL: "https://chatgpt.com",
				AssignmentKnown: true, Assigned: true,
			},
		},
		httpClient: &http.Client{Transport: directTransport},
		loanCache:  make(map[string]*loanedToken),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: p}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	conn := dialMITMHost(t, ln.Addr().String(), "chatgpt.com:443", "chatgpt.com", []string{"http/1.1"}, pool)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"duckway-phantom","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"auth0|duckway-phantom","jti":"dw-phantom-access"}`))
	fakeAccessJWT := header + "." + payload + ".signature"
	fmt.Fprintf(conn, "GET /backend-api/codex/responses HTTP/1.1\r\nHost: chatgpt.com\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", fakeAccessJWT)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "gateway-chatgpt" {
		t.Fatalf("body = %q, want gateway-chatgpt", string(body))
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/proxy/openai-chatgpt/backend-api/codex/responses" {
		t.Fatalf("gateway path = %q, want /proxy/openai-chatgpt/backend-api/codex/responses", gotPath)
	}
	if gotAuth != "Bearer "+fakeAccessJWT {
		t.Fatalf("gateway Authorization = %q", gotAuth)
	}
}

func TestMITMBridgesNativeCodexChatGPTWebSocket(t *testing.T) {
	var gotAuth, gotClientToken string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotClientToken = r.Header.Get("X-Duckway-Token")
		if !isWebSocketUpgrade(r) {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: mock\r\n\r\n")
		_ = rw.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(rw, payload); err == nil && string(payload) == "ping" {
			_, _ = conn.Write([]byte("pong"))
		}
	}))
	defer gateway.Close()

	ca, err := services.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM)
	proxy := &httpsProxy{
		serverURL: gateway.URL, token: "client-token", ca: ca,
		hostMap: map[string]hostEntry{"chatgpt.com": {
			Service: "openai-chatgpt", DeliveryMode: "proxy",
			AssignmentKnown: true, Assigned: true,
		}},
		httpClient: &http.Client{Transport: directTransport},
		loanCache:  make(map[string]*loanedToken),
	}
	server := httptest.NewServer(proxy)
	defer server.Close()

	conn := dialMITMHost(t, strings.TrimPrefix(server.URL, "http://"), "chatgpt.com:443", "chatgpt.com", []string{"http/1.1"}, pool)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"duckway-phantom"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"auth0|duckway-phantom","jti":"dw-phantom-access"}`))
	phantom := header + "." + payload + ".signature"
	_, _ = fmt.Fprintf(conn, "GET /backend-api/codex/responses HTTP/1.1\r\nHost: chatgpt.com\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: test\r\n\r\nping", phantom)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status=%d, want 101", resp.StatusCode)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "pong" {
		t.Fatalf("websocket payload=%q, want pong", got)
	}
	if gotAuth != "Bearer "+phantom || gotClientToken != "client-token" {
		t.Fatalf("gateway auth/client token mismatch")
	}
}

func TestMITMDirectsNativeCodexWebSocketWithRealCredential(t *testing.T) {
	var gotURL, gotAuth string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		gotAuth = req.Header.Get("Authorization")
		proxyEnd, upstreamEnd := net.Pipe()
		go func() {
			defer upstreamEnd.Close()
			payload := make([]byte, 4)
			if _, err := io.ReadFull(upstreamEnd, payload); err == nil && string(payload) == "ping" {
				_, _ = upstreamEnd.Write([]byte("pong"))
			}
		}()
		return &http.Response{
			StatusCode: http.StatusSwitchingProtocols, Status: "101 Switching Protocols", Proto: "HTTP/1.1",
			Header: http.Header{"Connection": []string{"Upgrade"}, "Upgrade": []string{"websocket"}},
			Body:   proxyEnd, Request: req,
		}, nil
	})
	ca, err := services.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM)
	proxy := &httpsProxy{
		serverURL: "http://gateway.invalid", token: "client-token", ca: ca,
		hostMap: map[string]hostEntry{"chatgpt.com": {
			Service: "openai-chatgpt", DeliveryMode: "proxy", UpstreamURL: "https://chatgpt.com",
			AssignmentKnown: true, Assigned: false,
		}},
		httpClient: &http.Client{Transport: transport}, loanCache: make(map[string]*loanedToken),
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	conn := dialMITMHost(t, strings.TrimPrefix(server.URL, "http://"), "chatgpt.com:443", "chatgpt.com", []string{"http/1.1"}, pool)
	_, _ = fmt.Fprint(conn, "GET /backend-api/codex/responses HTTP/1.1\r\nHost: chatgpt.com\r\nAuthorization: Bearer real-token\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: test\r\n\r\nping")
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status=%d, want 101", resp.StatusCode)
	}
	pong := make([]byte, 4)
	if _, err := io.ReadFull(reader, pong); err != nil {
		t.Fatal(err)
	}
	if string(pong) != "pong" || gotURL != "https://chatgpt.com/backend-api/codex/responses" || gotAuth != "Bearer real-token" {
		t.Fatalf("direct websocket mismatch payload=%q url=%q auth=%q", pong, gotURL, gotAuth)
	}
}

func TestWriteHTTPResponseStreamChunkedBody(t *testing.T) {
	const want = "packfile-response"
	var out bytes.Buffer
	resp := benchmarkGitPackResponse(io.NopCloser(strings.NewReader(want)))

	if err := writeHTTPResponseStream(&out, resp); err != nil {
		t.Fatalf("write response: %v", err)
	}
	gotResp, err := http.ReadResponse(bufio.NewReader(&out), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer gotResp.Body.Close()
	body, err := io.ReadAll(gotResp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}
