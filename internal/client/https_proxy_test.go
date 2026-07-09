package client

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	got := redactDebugRawQuery("access_token=ghs_real_secret&ok=value&token=github_pat_real_secret&x=sk-ant-real&phantom=github_pat_dw_fake")
	for _, secret := range []string{"ghs_real_secret", "github_pat_real_secret", "sk-ant-real", "github_pat_dw_fake"} {
		if bytes.Contains([]byte(got), []byte(secret)) {
			t.Fatalf("query leaked %q in %q", secret, got)
		}
	}
	for _, want := range []string{"access_token=%5Bredacted%5D", "token=%5Bredacted%5D", "x=%5Bredacted%5D", "phantom=%5Bredacted%5D", "ok=value"} {
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
		serverURL:   backendURL,
		token:       "test-token",
		ca:          ca,
		hostMap:     map[string]hostEntry{"api.anthropic.com": {Service: "anthropic", DeliveryMode: "proxy"}},
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
			"api.anthropic.com": {Service: "anthropic", DeliveryMode: "proxy", UpstreamURL: "https://api.anthropic.com"},
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

func TestMITMDirectForKnownHostWithoutPhantom(t *testing.T) {
	var gatewayHits int
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayHits++
		http.Error(w, "gateway should not be used", http.StatusBadGateway)
	}))
	defer gateway.Close()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer real-user-token" {
			t.Fatalf("origin Authorization = %q", got)
		}
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
	if string(body) != "direct-origin" {
		t.Fatalf("body = %q, want direct-origin", string(body))
	}
	if gatewayHits != 0 {
		t.Fatalf("gateway hits = %d, want 0", gatewayHits)
	}
}

func TestMITMAlwaysProxiesNativeCodexChatGPTTraffic(t *testing.T) {
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
			"chatgpt.com": {Service: "openai-chatgpt", DeliveryMode: "proxy", UpstreamURL: "https://chatgpt.com"},
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
	fakeAccessJWT := "header.payload.duckway-phantom-access"
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
