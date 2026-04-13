package handlers

import (
	"encoding/json"
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
		secret = "duckway-internal-default"
	}
	return &InternalHandler{resolver: resolver, secret: secret}
}

// Resolve handles POST /internal/resolve from the mitmproxy addon.
func (h *InternalHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	// Verify internal secret
	if r.Header.Get("X-Internal-Secret") != h.secret {
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
