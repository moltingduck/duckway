package client

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

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
		httpClient:  &http.Client{},
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

	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}

	fmt.Fprintf(raw, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	tlsConn := tls.Client(raw, &tls.Config{
		ServerName: "api.anthropic.com",
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
	br := bufio.NewReader(raw)
	connResp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
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

	fmt.Fprintf(conn, "GET /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\nConnection: close\r\n\r\n")
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
