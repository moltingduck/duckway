package handlers

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/hackerduck/duckway/internal/server/services"
)

type InternalHandler struct {
	resolver *services.KeyResolver
	secret   string
}

func NewInternalHandler(resolver *services.KeyResolver) *InternalHandler {
	secret := os.Getenv("DUCKWAY_INTERNAL_SECRET")
	if secret == "" {
		// Refuse to run with no secret: the internal API returns decrypted keys
		// and must not be reachable with a well-known default credential.
		log.Fatal("[internal-api] DUCKWAY_INTERNAL_SECRET is not set. " +
			"Set it to a long random value before starting the server.")
	}
	return &InternalHandler{resolver: resolver, secret: secret}
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
