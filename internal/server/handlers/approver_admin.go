package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// ApproverAdminHandler manages approver tokens (admin session auth).
type ApproverAdminHandler struct{ tokens *queries.ApproverTokenQueries }

func NewApproverAdminHandler(t *queries.ApproverTokenQueries) *ApproverAdminHandler {
	return &ApproverAdminHandler{tokens: t}
}

// approverNamePattern keeps names usable inside "approver:<name>" and in logs.
var approverNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

// GET /api/approvers: names, creation, last use and revocation. Never a token or hash.
func (h *ApproverAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	list, err := h.tokens.List()
	if err != nil {
		jsonError(w, "failed to list approver tokens", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, list)
}

// POST /api/approvers {name} -> {id, name, token}. The token is returned this
// once; only its SHA-256 is stored.
func (h *ApproverAdminHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if !approverNamePattern.MatchString(name) {
		jsonError(w, "name must be 1-64 characters: letters, digits, space, dot, dash or underscore, starting with a letter or digit", http.StatusBadRequest)
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		jsonError(w, "failed to generate token", http.StatusInternalServerError)
		return
	}
	token := "dwa_" + hex.EncodeToString(raw)
	id, err := svc.GenerateToken(8)
	if err != nil {
		jsonError(w, "failed to generate id", http.StatusInternalServerError)
		return
	}
	if err := h.tokens.Create(id, name, svc.HashToken(token)); err != nil {
		if errors.Is(err, queries.ErrApproverNameTaken) {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		jsonError(w, "failed to create approver token", http.StatusInternalServerError)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "name": name, "token": token})
}

// DELETE /api/approvers/{id} revokes the token (it keeps its row for history).
func (h *ApproverAdminHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	if err := h.tokens.Revoke(r.PathValue("id")); err != nil {
		if errors.Is(err, queries.ErrApproverNotFound) {
			jsonError(w, "approver token not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to revoke", http.StatusInternalServerError)
		return
	}
	noStore(w)
	jsonResponse(w, map[string]string{"status": "revoked"})
}
