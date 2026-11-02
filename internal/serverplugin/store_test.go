package serverplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/clientplugin"
)

func fixtureManifest(data []byte) clientplugin.Manifest {
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	return clientplugin.Manifest{APIVersion: 1, Name: "duckllo", Version: "sha256-" + digest, Description: "Duckllo", Artifacts: []clientplugin.Artifact{{OS: "linux", Arch: "amd64", URL: "/bin", SHA256: digest, Size: int64(len(data))}}}
}
func newFixture(t *testing.T) (*Store, *clientplugin.Manifest, *[]byte, *atomic.Int64, string) {
	t.Helper()
	data := []byte("real executable bytes\n")
	manifest := fixtureManifest(data)
	hits := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Duckway-Token") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("upstream received client credentials")
		}
		switch r.URL.Path {
		case "/manifest":
			_ = json.NewEncoder(w).Encode(manifest)
		case "/bin":
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	s, err := New(t.TempDir(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return s, &manifest, &data, hits, server.URL + "/manifest"
}
func publishFixture(s *Store, ctx context.Context, name, source string) (clientplugin.Manifest, error) {
	m, err := s.Import(ctx, name, source)
	if err != nil {
		return m, err
	}
	return s.Publish(ctx, name, m.Version, currentApproval(s))
}
func TestImportDurableCacheImmutableAndDisable(t *testing.T) {
	s, m, data, hits, source := newFixture(t)
	ctx := context.Background()
	approved, err := publishFixture(s, ctx, "duckllo", source)
	if err != nil {
		t.Fatal(err)
	}
	old := approved
	want := "/client/plugins/duckllo/releases/" + m.Version + "/linux/amd64"
	if approved.Artifacts[0].URL != want {
		t.Fatalf("artifact URL=%s", approved.Artifacts[0].URL)
	}
	if hits.Load() != 4 {
		t.Fatalf("upstream requests=%d", hits.Load())
	}
	// A separate Store models the independently running gateway, reading shared durable state.
	gateway, err := New(filepath.Dir(s.dir), "")
	if err != nil {
		t.Fatal(err)
	}
	check := func(version string, want []byte) {
		t.Helper()
		f, _, err := gateway.Artifact(ctx, "duckllo", version, "linux", "amd64")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		got, err := io.ReadAll(f)
		if err != nil || string(got) != string(want) {
			t.Fatalf("artifact=%q err=%v", got, err)
		}
	}
	check(old.Version, *data)
	catalog, err := gateway.Catalog(ctx)
	if err != nil || len(catalog) != 1 {
		t.Fatalf("catalog=%v err=%v", catalog, err)
	}
	if _, err := gateway.Manifest(ctx, "duckllo"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 4 {
		t.Fatal("cache reads contacted publisher")
	}
	// A corrupt next release never replaces the known-good current release.
	oldData := append([]byte(nil), (*data)...)
	*m = fixtureManifest([]byte("new release bytes"))
	*data = []byte("corrupted release")
	if _, err := publishFixture(s, ctx, "duckllo", source); err == nil {
		t.Fatal("corrupt import accepted")
	}
	current, err := gateway.Manifest(ctx, "duckllo")
	if err != nil || current.Version != old.Version {
		t.Fatalf("failed import changed active release: %v %v", current, err)
	}
	*data = []byte("new release bytes")
	if _, err := publishFixture(s, ctx, "duckllo", source); err != nil {
		t.Fatal(err)
	}
	check(old.Version, oldData)
	check(m.Version, *data)
	before := hits.Load()
	if _, err := publishFixture(s, ctx, "duckllo", strings.TrimSuffix(source, "manifest")+"changed"); err == nil {
		t.Fatal("source switch accepted")
	}
	if hits.Load() != before {
		t.Fatal("source switch fetched before rejection")
	}
	if err := s.SetEnabled(ctx, "duckllo", false); err != nil {
		t.Fatal(err)
	}
	catalog, err = gateway.Catalog(ctx)
	if err != nil || len(catalog) != 0 {
		t.Fatalf("disabled catalog %v %v", catalog, err)
	}
	if _, err := gateway.Manifest(ctx, "duckllo"); err != ErrNotFound {
		t.Fatalf("disabled manifest err=%v", err)
	}
	if f, _, err := gateway.Artifact(ctx, "duckllo", old.Version, "linux", "amd64"); err != ErrNotFound {
		if f != nil {
			f.Close()
		}
		t.Fatalf("disabled old artifact err=%v", err)
	}
	if err := s.SetEnabled(ctx, "duckllo", true); err != nil {
		t.Fatal(err)
	}
	check(old.Version, oldData)
}
func TestImportBoundsAndIdentity(t *testing.T) {
	tests := []struct {
		name   string
		change func(*clientplugin.Manifest, *[]byte)
	}{
		{"wrong name", func(m *clientplugin.Manifest, _ *[]byte) { m.Name = "different" }},
		{"oversized artifact", func(m *clientplugin.Manifest, _ *[]byte) { m.Artifacts[0].Size = clientplugin.MaxArtifactSize + 1 }},
		{"aggregate", func(m *clientplugin.Manifest, _ *[]byte) {
			a := m.Artifacts[0]
			a.Size = clientplugin.MaxArtifactSize
			m.Artifacts = []clientplugin.Artifact{a, a, a}
			m.Artifacts[0].Arch = "a"
			m.Artifacts[1].Arch = "b"
			m.Artifacts[2].Arch = "c"
		}},
		{"count", func(m *clientplugin.Manifest, _ *[]byte) {
			a := m.Artifacts[0]
			m.Artifacts = nil
			for i := 0; i < 33; i++ {
				a.Arch = string(rune('a'+i/26)) + string(rune('a'+i%26))
				m.Artifacts = append(m.Artifacts, a)
			}
		}},
		{"size mismatch", func(m *clientplugin.Manifest, _ *[]byte) { m.Artifacts[0].Size-- }},
		{"absolute upstream artifact", func(m *clientplugin.Manifest, _ *[]byte) { m.Artifacts[0].URL = "https://other.example/bin" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, m, data, hits, source := newFixture(t)
			tt.change(m, data)
			if _, err := publishFixture(s, context.Background(), "duckllo", source); err == nil {
				t.Fatal("invalid import accepted")
			}
			if list, err := s.Catalog(context.Background()); err != nil || len(list) != 0 {
				t.Fatalf("failed import published: %v %v", list, err)
			}
			if tt.name != "size mismatch" && hits.Load() != 1 {
				t.Fatalf("validation fetched artifacts: %d requests", hits.Load())
			}
		})
	}
}
func TestNetworkDestinations(t *testing.T) {
	for _, raw := range []string{"169.254.169.254", "127.0.0.1", "10.0.0.1", "192.168.2.1", "::1", "fe80::1", "ff02::1", "0.0.0.0", "224.0.0.1", "100.100.100.200", "fd00:ec2::254", "::ffff:169.254.169.254"} {
		ip := netip.MustParseAddr(raw)
		if permittedIP(ip, false) {
			t.Errorf("default permitted %s", raw)
		}
	}
	for _, raw := range []string{"169.254.169.254", "168.63.129.16", "100.100.100.200", "fd00:ec2::254", "::", "fe80::1", "ff02::1", "224.0.0.1", "::ffff:169.254.169.254"} {
		if permittedIP(netip.MustParseAddr(raw), true) {
			t.Errorf("allowlist permitted forbidden %s", raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "::1", "fd12::1"} {
		if !permittedIP(netip.MustParseAddr(raw), true) {
			t.Errorf("private explicit origin denied %s", raw)
		}
	}
	if !permittedIP(netip.MustParseAddr("8.8.8.8"), false) {
		t.Fatal("public IP rejected")
	}
}
func TestURLAndRedirectPolicy(t *testing.T) {
	s, _, _, hits, source := newFixture(t)
	for _, raw := range []string{"http://169.254.169.254/manifest", "http://127.0.0.1/manifest", "https://127.0.0.1/manifest", source + "?token=secret", source + "#fragment", strings.Replace(source, "://", "://user:password@", 1)} {
		if _, err := s.Import(context.Background(), "duckllo", raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid URL reached publisher")
	}
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	t.Cleanup(redirect.Close)
	p, err := New(t.TempDir(), redirect.URL+","+target.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Import(context.Background(), "duckllo", redirect.URL+"/manifest"); err == nil {
		t.Fatal("redirect accepted")
	}
	if targetHits.Load() != 0 {
		t.Fatal("redirect followed")
	}
	policy, _ := NewNetworkPolicy("http://8.8.8.8")
	u, _ := clientplugin.SourceURL("http://8.8.8.8/manifest")
	client, _ := policy.client(u)
	defer client.CloseIdleConnections()
	if _, err := fetch(context.Background(), client, u.String()); err == nil {
		t.Fatal("public plaintext allowed")
	}
}
func TestCacheRejectsSymlinksAndNonprivatePaths(t *testing.T) {
	t.Run("ancestor", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(base, "link")); err != nil {
			t.Fatal(err)
		}
		s, _ := New(filepath.Join(base, "link"), "")
		if _, err := s.Catalog(context.Background()); err == nil {
			t.Fatal("symlink ancestor accepted")
		}
	})
	t.Run("catalog", func(t *testing.T) {
		s, _, _, _, source := newFixture(t)
		if _, err := publishFixture(s, context.Background(), "duckllo", source); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(s.dir, "catalog.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), filepath.Join(s.dir, "catalog.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Catalog(context.Background()); err == nil {
			t.Fatal("symlink catalog accepted")
		}
	})
	t.Run("mode", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "plugins"), 0755); err != nil {
			t.Fatal(err)
		}
		s, _ := New(dir, "")
		if _, err := s.Catalog(context.Background()); err == nil {
			t.Fatal("nonprivate cache accepted")
		}
	})
}

func TestDNSValidationAndPinnedDial(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(server.Close)
	// The URL hostname does not exist in real DNS. Its single approved lookup
	// resolves to the fixture, proving the actual dial uses that validated IP.
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	u, err := clientplugin.SourceURL("http://publisher.invalid:" + port + "/manifest")
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := NewNetworkPolicy("http://publisher.invalid:" + port)
	var lookups atomic.Int64
	policy.lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	client, err := policy.client(u)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := fetch(context.Background(), client, u.String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if hits.Load() != 1 || lookups.Load() != 1 {
		t.Fatalf("hits=%d lookups=%d", hits.Load(), lookups.Load())
	}
	// Reject all DNS answers when even one candidate is metadata/link-local.
	other, _ := NewNetworkPolicy("http://publisher.invalid:" + port)
	other.lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("169.254.169.254")}, nil
	}
	denied, _ := other.client(u)
	defer denied.CloseIdleConnections()
	if _, err := fetch(context.Background(), denied, u.String()); err == nil {
		t.Fatal("mixed allowed/forbidden DNS accepted")
	}
	if hits.Load() != 1 {
		t.Fatal("mixed DNS reached fixture")
	}
	// Default HTTPS policy rejects a name which resolves privately before dialing.
	publicURL, _ := clientplugin.SourceURL("https://publisher.invalid/manifest")
	strict, _ := NewNetworkPolicy("")
	strict.lookupIP = other.lookupIP
	strictClient, _ := strict.client(publicURL)
	defer strictClient.CloseIdleConnections()
	if _, err := fetch(context.Background(), strictClient, publicURL.String()); err == nil {
		t.Fatal("private DNS accepted by public policy")
	}
}

func TestConcurrentReadersUseFilesystemLock(t *testing.T) {
	s, _, _, _, source := newFixture(t)
	if _, err := publishFixture(s, context.Background(), "duckllo", source); err != nil {
		t.Fatal(err)
	}
	root, done, err := s.locked(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	_ = root
	unlock := sync.OnceFunc(done)
	t.Cleanup(unlock)
	// Separate store instances have independent memory and independent lock descriptors.
	other, err := New(filepath.Dir(s.dir), "")
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := other.Catalog(ctx); err == nil {
		unlock()
		t.Fatal("reader bypassed writer flock")
	}
	unlock()
	if _, err := other.Catalog(context.Background()); err != nil {
		t.Fatalf("reader failed after writer unlock: %v", err)
	}
}

func TestChunkedArtifactCannotExceedDeclaredSize(t *testing.T) {
	m := fixtureManifest([]byte("x"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			_ = json.NewEncoder(w).Encode(m)
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("xy"))
	}))
	t.Cleanup(server.Close)
	s, err := New(t.TempDir(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishFixture(s, context.Background(), "duckllo", server.URL+"/manifest"); err == nil {
		t.Fatal("chunked artifact overrun accepted")
	}
	list, err := s.Catalog(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("overrun activated catalog: %v %v", list, err)
	}
}

func currentApproval(s *Store) string {
	e, err := s.snapshot(context.Background(), "duckllo")
	if err != nil {
		return ""
	}
	return manifestDigest(e.Latest)
}
