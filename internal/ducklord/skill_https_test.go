package ducklord

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func makeSkillArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return b.Bytes()
}

func TestFetchHTTPSArchive(t *testing.T) {
	archive := makeSkillArchive(t, "SKILL.md", []byte("hello"))
	oldClient, oldLookup := skillHTTPSClient, skillHTTPSLookupIP
	skillHTTPSClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(archive)), Header: make(http.Header)}, nil
	})}
	skillHTTPSLookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.1")}}, nil
	}
	defer func() { skillHTTPSClient, skillHTTPSLookupIP = oldClient, oldLookup }()
	info, err := FetchHTTPSArchive(context.Background(), "https://example.test/skill", false, t.TempDir(), "demo")
	if err != nil || info.Files != 1 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchHTTPSArchiveValidation(t *testing.T) {
	for _, raw := range []string{"http://example.test/a", "https://", "https://user@example.test/a", "https://example.test/a#x"} {
		if _, err := FetchHTTPSArchive(context.Background(), raw, false, t.TempDir(), "demo"); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(makeSkillArchive(t, "SKILL.md", []byte("x")))
	}))
	defer server.Close()
	old, oldLookup, oldDial := skillHTTPSClient, skillHTTPSLookupIP, skillHTTPSNetDial
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	skillHTTPSClient = &http.Client{Transport: transport}
	skillHTTPSLookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.1")}}, nil
	}
	skillHTTPSNetDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer func() { skillHTTPSClient, skillHTTPSLookupIP, skillHTTPSNetDial = old, oldLookup, oldDial }()
	if _, err := FetchHTTPSArchive(context.Background(), "https://example.test/skill", true, t.TempDir(), "demo"); err != nil {
		t.Fatalf("insecure fetch: %v", err)
	}
}

func TestFetchHTTPSArchiveDisablesConfiguredProxy(t *testing.T) {
	archive := makeSkillArchive(t, "SKILL.md", []byte("x"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	oldClient, oldLookup, oldDial := skillHTTPSClient, skillHTTPSLookupIP, skillHTTPSNetDial
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = func(*http.Request) (*url.URL, error) {
		return nil, fmt.Errorf("configured proxy must not be used")
	}
	skillHTTPSClient = &http.Client{Transport: transport}
	skillHTTPSLookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.1")}}, nil
	}
	skillHTTPSNetDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer func() { skillHTTPSClient, skillHTTPSLookupIP, skillHTTPSNetDial = oldClient, oldLookup, oldDial }()
	if _, err := FetchHTTPSArchive(context.Background(), "https://example.test/skill", true, t.TempDir(), "demo"); err != nil {
		t.Fatalf("fetch with configured proxy: %v", err)
	}
}

func TestFetchHTTPSArchiveRejectsPrivateDestinations(t *testing.T) {
	for _, raw := range []string{"https://127.0.0.1/skill", "https://[::1]/skill", "https://100.64.0.1/skill", "https://198.18.0.1/skill"} {
		if _, err := FetchHTTPSArchive(context.Background(), raw, false, t.TempDir(), "demo"); err == nil {
			t.Fatalf("accepted private URL %q", raw)
		}
	}
	old := skillHTTPSLookupIP
	skillHTTPSLookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
	}
	defer func() { skillHTTPSLookupIP = old }()
	if _, err := FetchHTTPSArchive(context.Background(), "https://internal.example/skill", false, t.TempDir(), "demo"); err == nil {
		t.Fatal("accepted hostname resolving to private address")
	}
}

func TestHTTPSPolicyRejectsReservedNonPublicRangesAtDial(t *testing.T) {
	for _, raw := range []string{"100.64.0.1:443", "198.18.0.1:443"} {
		if _, err := skillHTTPSDialContext(context.Background(), "tcp", raw); err == nil {
			t.Fatalf("accepted reserved address %q", raw)
		}
	}
}

func TestHTTPSRedirectRejectsPrivateDestination(t *testing.T) {
	old := skillHTTPSLookupIP
	skillHTTPSLookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("192.168.1.10")}}, nil
	}
	defer func() { skillHTTPSLookupIP = old }()
	req, err := http.NewRequest(http.MethodGet, "https://internal.example/skill", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := httpsRedirect(context.Background(), req, nil); err == nil {
		t.Fatal("accepted redirect to private address")
	}
}

func TestHTTPSDialRevalidatesDNSAtDialTime(t *testing.T) {
	oldLookup, oldDial := skillHTTPSLookupIP, skillHTTPSNetDial
	defer func() { skillHTTPSLookupIP, skillHTTPSNetDial = oldLookup, oldDial }()
	lookups := 0
	skillHTTPSLookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	dialed := false
	skillHTTPSNetDial = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	}
	if err := validatePublicHTTPSDestination(context.Background(), mustHTTPSURL(t, "https://rebind.example/skill")); err != nil {
		t.Fatal(err)
	}
	if _, err := skillHTTPSDialContext(context.Background(), "tcp", "rebind.example:443"); err == nil {
		t.Fatal("accepted private address returned during dial")
	}
	if dialed {
		t.Fatal("dialed an address before the dial-time policy check")
	}
}

func TestHTTPSDialUsesValidatedAddress(t *testing.T) {
	oldLookup, oldDial := skillHTTPSLookupIP, skillHTTPSNetDial
	defer func() { skillHTTPSLookupIP, skillHTTPSNetDial = oldLookup, oldDial }()
	skillHTTPSLookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.7")}}, nil
	}
	var got string
	skillHTTPSNetDial = func(_ context.Context, _, address string) (net.Conn, error) {
		got = address
		return nil, io.ErrClosedPipe
	}
	if _, err := skillHTTPSDialContext(context.Background(), "tcp", "example.test:443"); err == nil {
		t.Fatal("expected dial failure")
	}
	if got != "203.0.113.7:443" {
		t.Fatalf("dial address = %q", got)
	}
}

func mustHTTPSURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestFetchHTTPSArchiveRejectsTraversal(t *testing.T) {
	archive := makeSkillArchive(t, "../SKILL.md", []byte("x"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	defer server.Close()
	old := skillHTTPSClient
	skillHTTPSClient = server.Client()
	defer func() { skillHTTPSClient = old }()
	// The URL itself is HTTPS; the test client routes it to the local HTTP server.
	_, err := FetchHTTPSArchive(context.Background(), "https://example.test/skill", false, t.TempDir(), "demo")
	if err == nil {
		t.Fatal("accepted traversal archive")
	}
}
