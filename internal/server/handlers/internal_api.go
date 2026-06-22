package handlers

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hackerduck/duckway/internal/server/services"
)

type InternalHandler struct {
	resolver *services.KeyResolver
	secret   string
}

// NewInternalHandler creates the handler for the mitmproxy internal API.
// Secret resolution order:
//  1. DUCKWAY_INTERNAL_SECRET env var
//  2. <dataDir>/internal-secret file (auto-created on first run)
func NewInternalHandler(resolver *services.KeyResolver, dataDir string) *InternalHandler {
	secret := os.Getenv("DUCKWAY_INTERNAL_SECRET")
	if secret == "" {
		secret = loadOrCreateInternalSecret(dataDir)
	}
	return &InternalHandler{resolver: resolver, secret: secret}
}

func loadOrCreateInternalSecret(dataDir string) string {
	path := filepath.Join(dataDir, "internal-secret")
	if data, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(data))
		if s != "" {
			return s
		}
	}
	// Generate a new 32-byte random secret and persist it.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		log.Fatalf("[internal-api] failed to generate secret: %v", err)
	}
	secret := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
		log.Printf("[internal-api] warning: could not persist secret to %s: %v", path, err)
	} else {
		log.Printf("[internal-api] generated internal secret, saved to %s", path)
	}
	return secret
}

// Resolve handles POST /internal/resolve from the mitmproxy addon.
func (h *InternalHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	// Constant-time comparison to prevent timing side-channel.
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Secret")), []byte(h.secret)) != 1 {
		jsonError(w, "invalid internal secret", http.StatusUnauthorized)
		return
	}

	var req struct {
		Placeholder string `json:"placeholder"`
		ClientID    string `json:"client_id"`
		TargetHost  string `json:"target_host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	result, err := h.resolver.Resolve(req.Placeholder, req.ClientID)
	if err != nil {
		jsonError(w, "resolve error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]interface{}{
		"permitted":     result.Permitted,
		"need_approval": result.NeedApproval,
	}

	if result.Permitted {
		resp["real_key"] = result.RealKey
	}
	if result.Error != "" {
		resp["error"] = result.Error
	}

	jsonResponse(w, resp)
}
