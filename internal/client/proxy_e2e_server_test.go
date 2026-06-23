package client

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

// TestE2E_ClientProxy_Through_ServerProxy wires the client-side MITM proxy
// (httpsProxy) end-to-end through the real server-side ProxyHandler all the
// way to a fake upstream LLM API, and asserts that:
//   - The response status is 200
//   - The fake upstream received Authorization: Bearer sk-ant-real (the real key)
//   - The fake upstream did NOT see any X-Duckway-* headers
//   - The response body contains "hello"
func TestE2E_ClientProxy_Through_ServerProxy(t *testing.T) {
	// -----------------------------------------------------------------------
	// 1. Fake "upstream LLM API" — stands in for api.anthropic.com
	// -----------------------------------------------------------------------
	var (
		upstreamMu         sync.Mutex
		upstreamAuthHeader string
		upstreamDuckwayHdr string
	)

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMu.Lock()
		upstreamAuthHeader = r.Header.Get("Authorization")
		for key := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-duckway-") {
				upstreamDuckwayHdr = key
			}
		}
		upstreamMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"role":"assistant","content":"hello"}`))
	}))
	defer fakeUpstream.Close()

	// -----------------------------------------------------------------------
	// 2. In-memory SQLite DB with migrations; seed service + client + key
	// -----------------------------------------------------------------------
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	defer db.Close()

	// 32-byte AES key for NewCrypto
	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))

	encryptedKey, err := crypto.Encrypt("sk-ant-real")
	if err != nil {
		t.Fatalf("encrypt api key: %v", err)
	}

	svcQ := queries.NewServiceQueries(db)
	clientQ := queries.NewClientQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	placeholderQ := queries.NewPlaceholderQueries(db)
	groupQ := queries.NewGroupQueries(db)
	approvalQ := queries.NewApprovalQueries(db)
	settingsQ := queries.NewSettingsQueries(db)

	// Seed: service
	svc := &models.Service{
		ID:           "svc-e2e-test",
		Name:         "anthropic",
		DisplayName:  "Anthropic (e2e test)",
		UpstreamURL:  fakeUpstream.URL,
		HostPattern:  "api.anthropic.com",
		AuthType:     "bearer",
		AuthHeader:   "Authorization",
		AuthPrefix:   "Bearer ",
		DeliveryMode: "proxy",
		IsActive:     true,
	}
	if err := svcQ.Create(svc); err != nil {
		t.Fatalf("create service: %v", err)
	}

	// Seed: client (token_hash is SHA-256 of "test-token")
	h := sha256.Sum256([]byte("test-token"))
	tokenHash := fmt.Sprintf("%x", h)
	seedClient := &models.Client{
		ID:        "client-e2e-test",
		ShortID:   "e2eabc",
		Name:      "e2e-test-client",
		TokenHash: tokenHash,
		IsActive:  true,
	}
	if err := clientQ.Create(seedClient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	// Seed: api_key
	apiKey := &models.APIKey{
		ID:           "apikey-e2e-test",
		ServiceID:    svc.ID,
		Name:         "e2e real key",
		KeyEncrypted: encryptedKey,
		IsActive:     true,
	}
	if err := apiKeyQ.Create(apiKey); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// Seed: placeholder_key linking client → api_key with delivery_mode proxy
	apiKeyID := apiKey.ID
	placeholder := &models.PlaceholderKey{
		ID:          "ph-e2e-test",
		EnvName:     "ANTHROPIC_API_KEY",
		Placeholder: "sk-ant-phantom-e2etest",
		ServiceID:   svc.ID,
		APIKeyID:    &apiKeyID,
		ClientID:    seedClient.ID,
		IsActive:    true,
	}
	if err := placeholderQ.Create(placeholder); err != nil {
		t.Fatalf("create placeholder key: %v", err)
	}

	// -----------------------------------------------------------------------
	// 3. ProxyHandler backed by real DB / KeyResolver
	// -----------------------------------------------------------------------
	resolver := services.NewKeyResolver(crypto, apiKeyQ, placeholderQ, groupQ, approvalQ)
	proxyH := handlers.NewProxyHandler(svcQ, apiKeyQ, resolver, nil, approvalQ, settingsQ, nil)

	// -----------------------------------------------------------------------
	// 4. httptest server wrapping the ProxyHandler; inject seedClient into
	//    context on every request (bypasses client auth middleware)
	// -----------------------------------------------------------------------
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), middleware.ClientKey, seedClient)
		proxyH.Handle(w, r.WithContext(ctx))
	})
	duckwayServer := httptest.NewServer(mux)
	defer duckwayServer.Close()

	// -----------------------------------------------------------------------
	// 5. Client-side httpsProxy (MITM) pointing at the duckway server
	// -----------------------------------------------------------------------
	ca, err := services.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		t.Fatal("append CA cert to pool")
	}

	proxy := &httpsProxy{
		serverURL: duckwayServer.URL,
		token:     "test-token",
		ca:        ca,
		hostMap: map[string]hostEntry{
			"api.anthropic.com": {
				Service:      "anthropic",
				DeliveryMode: "proxy",
			},
		},
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		loanCache:   make(map[string]*loanedToken),
		auditClient: &http.Client{Timeout: time.Second},
	}

	// Start the client-side (MITM) proxy
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	proxyAddr := strings.TrimPrefix(proxyServer.URL, "http://")

	// -----------------------------------------------------------------------
	// 6. CONNECT api.anthropic.com:443 through the MITM proxy
	// -----------------------------------------------------------------------
	tlsConn := dialMITM(t, proxyAddr, []string{"http/1.1"}, pool)

	// -----------------------------------------------------------------------
	// 7. Send POST /v1/messages through the decrypted MITM tunnel
	// -----------------------------------------------------------------------
	reqBody := `{"model":"claude-3","messages":[]}`
	fmt.Fprintf(tlsConn,
		"POST /v1/messages HTTP/1.1\r\n"+
			"Host: api.anthropic.com\r\n"+
			"Content-Type: application/json\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n"+
			"\r\n"+
			"%s",
		len(reqBody), reqBody,
	)

	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("read response from MITM tunnel: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// -----------------------------------------------------------------------
	// 8. Assertions
	// -----------------------------------------------------------------------

	// 8a. HTTP 200
	if resp.StatusCode != http.StatusOK {
		t.Errorf("response status = %d, want 200; body: %s", resp.StatusCode, string(body))
	}

	upstreamMu.Lock()
	gotAuth := upstreamAuthHeader
	gotDuckway := upstreamDuckwayHdr
	upstreamMu.Unlock()

	// 8b. Upstream received the real key injected by the server proxy
	if gotAuth != "Bearer sk-ant-real" {
		t.Errorf("upstream Authorization = %q, want \"Bearer sk-ant-real\"", gotAuth)
	}

	// 8c. No X-Duckway-* headers leaked through to the upstream
	if gotDuckway != "" {
		t.Errorf("upstream received X-Duckway-* header %q — must not leak upstream", gotDuckway)
	}

	// 8d. Response body contains the upstream's payload
	if !strings.Contains(string(body), "hello") {
		t.Errorf("response body = %q, want it to contain \"hello\"", string(body))
	}
}
