package handlers

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// ClientDraftHandler backs the Add-client wizard's resumable drafts. A draft
// is an inactive client with an unusable token hash; only IssueToken (the
// wizard's Install step) gives it a credential and activates it.
type ClientDraftHandler struct {
	base   *ClientHandler
	drafts *queries.ClientDraftQueries
}

func NewClientDraftHandler(base *ClientHandler, drafts *queries.ClientDraftQueries) *ClientDraftHandler {
	return &ClientDraftHandler{base: base, drafts: drafts}
}

var draftNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validDraftName(name string) bool { return draftNameRE.MatchString(name) }

func (h *ClientDraftHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.drafts.List()
	if err != nil {
		jsonError(w, "failed to list drafts", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, list)
}

func (h *ClientDraftHandler) Get(w http.ResponseWriter, r *http.Request) {
	d, err := h.drafts.Get(r.PathValue("id"))
	if err != nil {
		jsonError(w, "draft not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, d)
}

// Create starts a draft. No token is generated or returned.
func (h *ClientDraftHandler) Create(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		jsonError(w, "cross-origin request denied", http.StatusForbidden)
		return
	}
	var req struct {
		Name         string `json:"name"`
		UpdatePolicy string `json:"update_policy"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if !validDraftName(name) {
		jsonError(w, "name must be lowercase letters, digits and dashes", http.StatusBadRequest)
		return
	}
	if req.UpdatePolicy == "" {
		req.UpdatePolicy = "managed"
	}
	if req.UpdatePolicy != "managed" && req.UpdatePolicy != "manual" {
		jsonError(w, "update_policy must be managed or manual", http.StatusBadRequest)
		return
	}
	if existing, _ := h.base.clients.GetByName(name); existing != nil {
		jsonError(w, "client name '"+name+"' already exists", http.StatusConflict)
		return
	}
	// The hash comes from a random secret that is discarded, so no presented
	// token can ever match it.
	secret, err := svc.GenerateToken(32)
	if err != nil {
		jsonError(w, "failed to create draft", http.StatusInternalServerError)
		return
	}
	id, _ := svc.GenerateToken(16)
	c := &models.Client{
		ID: id, ShortID: svc.GenerateShortID(), Name: name,
		TokenHash: svc.HashToken(secret), CanaryEnabled: true, UpdatePolicy: req.UpdatePolicy,
	}
	if err := h.drafts.Create(c); err != nil {
		if queries.IsUniqueViolation(err) {
			jsonError(w, "client name '"+name+"' already exists", http.StatusConflict)
			return
		}
		jsonError(w, "failed to create draft", http.StatusInternalServerError)
		return
	}
	d, err := h.drafts.Get(id)
	if err != nil {
		jsonError(w, "failed to load draft", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, d)
}

// Update saves wizard progress (name, update policy, step, selected bundle).
func (h *ClientDraftHandler) Update(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		jsonError(w, "cross-origin request denied", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	var req struct {
		Name         *string `json:"name"`
		UpdatePolicy *string `json:"update_policy"`
		Step         *int    `json:"step"`
		SuiteID      *string `json:"suite_id"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !h.drafts.IsDraft(id) {
		jsonError(w, "draft not found", http.StatusNotFound)
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if !validDraftName(n) {
			jsonError(w, "name must be lowercase letters, digits and dashes", http.StatusBadRequest)
			return
		}
		if existing, _ := h.base.clients.GetByName(n); existing != nil && existing.ID != id {
			jsonError(w, "client name '"+n+"' already exists", http.StatusConflict)
			return
		}
		req.Name = &n
	}
	if req.UpdatePolicy != nil && *req.UpdatePolicy != "managed" && *req.UpdatePolicy != "manual" {
		jsonError(w, "update_policy must be managed or manual", http.StatusBadRequest)
		return
	}
	if req.Step != nil && (*req.Step < 1 || *req.Step > 4) {
		jsonError(w, "step must be 1-4", http.StatusBadRequest)
		return
	}
	if err := h.drafts.Update(id, req.Name, req.UpdatePolicy, req.Step, req.SuiteID); err != nil {
		if errors.Is(err, queries.ErrNotDraft) {
			jsonError(w, "draft not found", http.StatusNotFound)
			return
		}
		if queries.IsUniqueViolation(err) {
			jsonError(w, "client name already exists", http.StatusConflict)
			return
		}
		jsonError(w, "failed to save draft", http.StatusInternalServerError)
		return
	}
	d, err := h.drafts.Get(id)
	if err != nil {
		jsonError(w, "failed to load draft", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, d)
}

// IssueToken is the Install step: it generates the client's one real token,
// activates the client and returns the token once.
func (h *ClientDraftHandler) IssueToken(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		jsonError(w, "cross-origin request denied", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	d, err := h.drafts.Get(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, "draft not found (the token was already issued or the draft was discarded)", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to load draft", http.StatusInternalServerError)
		return
	}
	token, err := svc.GenerateToken(32)
	if err != nil {
		jsonError(w, "failed to generate token", http.StatusInternalServerError)
		return
	}
	if err := h.drafts.IssueToken(id, svc.HashToken(token)); err != nil {
		if errors.Is(err, queries.ErrNotDraft) {
			jsonError(w, "draft not found (the token was already issued or the draft was discarded)", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to issue token", http.StatusInternalServerError)
		return
	}
	h.base.assignHeartbeat(id)
	if h.base.canarySvc != nil {
		go func(name, shortID string) {
			if err := h.base.canarySvc.GenerateForClient(id, name, shortID); err != nil {
				log.Printf("Canary token generation failed for %s: %v", name, err)
			}
		}(d.Name, d.ShortID)
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]string{
		"id": id, "short_id": d.ShortID, "name": d.Name, "token": token,
		"issued_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// Delete discards a draft and everything the wizard attached to it.
func (h *ClientDraftHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		jsonError(w, "cross-origin request denied", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if err := h.drafts.Discard(id); err != nil {
		if errors.Is(err, queries.ErrNotDraft) {
			jsonError(w, "draft not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to discard draft", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}
