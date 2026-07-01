package handlers

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type KeySuiteHandler struct {
	suites       *queries.KeySuiteQueries
	services     *queries.ServiceQueries
	placeholders *queries.PlaceholderQueries
	apiKeys      *queries.APIKeyQueries
	crypto       *svc.Crypto
}

func NewKeySuiteHandler(
	suites *queries.KeySuiteQueries,
	services *queries.ServiceQueries,
	placeholders *queries.PlaceholderQueries,
	apiKeys *queries.APIKeyQueries,
	crypto *svc.Crypto,
) *KeySuiteHandler {
	return &KeySuiteHandler{
		suites:       suites,
		services:     services,
		placeholders: placeholders,
		apiKeys:      apiKeys,
		crypto:       crypto,
	}
}

// GET /api/key-suites
func (h *KeySuiteHandler) List(w http.ResponseWriter, r *http.Request) {
	suites, err := h.suites.List()
	if err != nil {
		jsonError(w, "failed to list suites", http.StatusInternalServerError)
		return
	}
	if suites == nil {
		suites = []models.KeySuite{}
	}
	// Attach entry counts + bound-client counts
	type suiteRow struct {
		models.KeySuite
		EntryCount  int `json:"entry_count"`
		ClientCount int `json:"client_count"`
	}
	result := make([]suiteRow, 0, len(suites))
	for _, s := range suites {
		entries, _ := h.suites.ListEntries(s.ID)
		bound, _ := h.suites.CountBoundClients(s.ID)
		result = append(result, suiteRow{KeySuite: s, EntryCount: len(entries), ClientCount: bound})
	}
	jsonResponse(w, result)
}

// POST /api/key-suites
func (h *KeySuiteHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := parseRequest(r, &req); err != nil || req.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	id, _ := svc.GenerateToken(16)
	s := &models.KeySuite{ID: id, Name: req.Name, Description: req.Description}
	if err := h.suites.Create(s); err != nil {
		jsonError(w, "failed to create suite: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Entries = []models.KeySuiteEntry{}
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, s)
}

// GET /api/key-suites/{id}
func (h *KeySuiteHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s, err := h.suites.GetByID(id)
	if err != nil {
		jsonError(w, "suite not found", http.StatusNotFound)
		return
	}
	if err := h.suites.PruneStaleSuitePlaceholders(id); err != nil {
		jsonError(w, "failed to clean stale suite assignments: "+err.Error(), http.StatusInternalServerError)
		return
	}
	bound, err := h.suites.CountBoundClients(id)
	if err != nil {
		jsonError(w, "failed to count assigned clients", http.StatusInternalServerError)
		return
	}
	clients, err := h.suites.ListBoundClients(id)
	if err != nil {
		jsonError(w, "failed to list assigned clients", http.StatusInternalServerError)
		return
	}
	if clients == nil {
		clients = []models.KeySuiteClient{}
	}
	jsonResponse(w, map[string]interface{}{
		"id":           s.ID,
		"name":         s.Name,
		"description":  s.Description,
		"created_at":   s.CreatedAt,
		"entries":      s.Entries,
		"client_count": bound,
		"clients":      clients,
	})
}

// PATCH /api/key-suites/{id}
func (h *KeySuiteHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s, err := h.suites.GetByID(id)
	if err != nil {
		jsonError(w, "suite not found", http.StatusNotFound)
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Name != nil {
		s.Name = *req.Name
	}
	if req.Description != nil {
		s.Description = *req.Description
	}
	if err := h.suites.Update(s); err != nil {
		jsonError(w, "failed to update suite: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, s)
}

// DELETE /api/key-suites/{id}
func (h *KeySuiteHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.suites.Delete(id); err != nil {
		jsonError(w, "failed to delete suite", http.StatusInternalServerError)
		return
	}
	// Placeholders keep existing keys but lose suite tracking (suite_id set null by ON DELETE SET NULL)
	w.WriteHeader(http.StatusOK)
}

// POST /api/key-suites/{id}/entries
func (h *KeySuiteHandler) AddEntry(w http.ResponseWriter, r *http.Request) {
	suiteID := r.PathValue("id")
	if _, err := h.suites.GetByID(suiteID); err != nil {
		jsonError(w, "suite not found", http.StatusNotFound)
		return
	}
	var req struct {
		ServiceID string  `json:"service_id"`
		APIKeyID  *string `json:"api_key_id"`
		GroupID   *string `json:"group_id"`
		EnvName   string  `json:"env_name"`
	}
	if err := parseRequest(r, &req); err != nil || req.ServiceID == "" {
		jsonError(w, "service_id is required", http.StatusBadRequest)
		return
	}
	if req.APIKeyID == nil && req.GroupID == nil {
		jsonError(w, "either api_key_id or group_id is required", http.StatusBadRequest)
		return
	}
	if req.EnvName == "" {
		if service, err := h.services.GetByID(req.ServiceID); err == nil {
			req.EnvName = defaultEnvName(service.Name)
		}
	}
	id, _ := svc.GenerateToken(16)
	entry := &models.KeySuiteEntry{
		ID: id, SuiteID: suiteID,
		ServiceID: req.ServiceID, APIKeyID: req.APIKeyID, GroupID: req.GroupID,
		EnvName: req.EnvName,
	}
	if err := h.suites.AddEntry(entry); err != nil {
		jsonError(w, "failed to add entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	propagated, skipped := h.assignEntryToBoundClients(entry)
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]interface{}{
		"entry":      entry,
		"propagated": propagated,
		"skipped":    skipped,
	})
}

// PATCH /api/key-suites/{id}/entries/{entryId}
// Also propagates the change to all placeholder_keys that came from this suite entry.
func (h *KeySuiteHandler) UpdateEntry(w http.ResponseWriter, r *http.Request) {
	suiteID := r.PathValue("id")
	entryID := r.PathValue("entryId")

	entry, err := h.suites.GetEntry(entryID)
	if err != nil || entry.SuiteID != suiteID {
		jsonError(w, "entry not found", http.StatusNotFound)
		return
	}

	var req struct {
		APIKeyID *string `json:"api_key_id"`
		GroupID  *string `json:"group_id"`
		EnvName  *string `json:"env_name"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	// At least one key source must remain set
	newAPIKey := entry.APIKeyID
	newGroup := entry.GroupID
	if req.APIKeyID != nil {
		newAPIKey = req.APIKeyID
		newGroup = nil
	}
	if req.GroupID != nil {
		newGroup = req.GroupID
		newAPIKey = nil
	}
	if newAPIKey == nil && newGroup == nil {
		jsonError(w, "either api_key_id or group_id is required", http.StatusBadRequest)
		return
	}
	if req.EnvName != nil {
		entry.EnvName = *req.EnvName
	}
	entry.APIKeyID = newAPIKey
	entry.GroupID = newGroup

	if err := h.suites.UpdateEntry(entry); err != nil {
		jsonError(w, "failed to update entry: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Propagate to all placeholders from this suite entry.
	// If the key format changed (e.g. ghp_ → github_pat_), regenerate the placeholder token.
	updatedIDs, err := h.suites.PropagateEntryUpdate(suiteID, entry.ServiceID, newAPIKey, newGroup)
	if err != nil {
		jsonError(w, "failed to propagate entry update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(updatedIDs) > 0 && newAPIKey != nil && h.apiKeys != nil && h.crypto != nil {
		if apiKey, err := h.apiKeys.GetByID(*newAPIKey); err == nil {
			if realKey, err := h.crypto.Decrypt(apiKey.KeyEncrypted); err == nil {
				service, _ := h.services.GetByID(entry.ServiceID)
				prefix, keyLen := "", 64
				if service != nil {
					prefix, keyLen = service.KeyPrefix, service.KeyLength
				}
				prefix, keyLen = svc.DetectKeyFormat(realKey, prefix, keyLen)
				for _, pid := range updatedIDs {
					if newPH, err := svc.GeneratePlaceholder(prefix, keyLen); err == nil {
						h.placeholders.UpdatePlaceholder(pid, newPH)
					}
				}
			}
		}
	}

	jsonResponse(w, entry)
}

// DELETE /api/key-suites/{id}/entries/{entryId}
func (h *KeySuiteHandler) RemoveEntry(w http.ResponseWriter, r *http.Request) {
	suiteID := r.PathValue("id")
	entryID := r.PathValue("entryId")
	entry, err := h.suites.GetEntry(entryID)
	if err != nil || entry.SuiteID != suiteID {
		jsonError(w, "entry not found", http.StatusNotFound)
		return
	}
	if err := h.suites.DeleteSuiteServicePlaceholders(suiteID, entry.ServiceID); err != nil {
		jsonError(w, "failed to unassign suite entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.suites.RemoveEntry(entryID); err != nil {
		jsonError(w, "failed to remove entry", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *KeySuiteHandler) assignEntryToBoundClients(entry *models.KeySuiteEntry) ([]string, []string) {
	clients, err := h.suites.ListBoundClients(entry.SuiteID)
	if err != nil || len(clients) == 0 {
		return []string{}, []string{}
	}

	var assigned, skipped []string
	for _, client := range clients {
		ok, err := h.assignSuiteEntryToClient(entry, client.ID)
		if err != nil {
			log.Printf("[key-suites] failed to propagate entry %s to client %s: %v", entry.ID, client.ID, err)
			skipped = append(skipped, client.Name)
			continue
		}
		if ok {
			assigned = append(assigned, client.Name)
		} else {
			skipped = append(skipped, client.Name)
		}
	}
	if assigned == nil {
		assigned = []string{}
	}
	if skipped == nil {
		skipped = []string{}
	}
	return assigned, skipped
}

func (h *KeySuiteHandler) assignSuiteEntryToClient(entry *models.KeySuiteEntry, clientID string) (bool, error) {
	service, err := h.services.GetByID(entry.ServiceID)
	if err != nil {
		return false, err
	}

	if existing, err := h.placeholders.GetByClientAndService(clientID, entry.ServiceID); err == nil {
		if existing.SuiteID == nil {
			return false, nil
		}
		if err := h.placeholders.Delete(existing.ID); err != nil {
			return false, err
		}
	} else if err != sql.ErrNoRows {
		return false, err
	}

	prefix, keyLen := service.KeyPrefix, service.KeyLength
	if entry.APIKeyID != nil && h.apiKeys != nil && h.crypto != nil {
		if apiKey, err := h.apiKeys.GetByID(*entry.APIKeyID); err == nil {
			if realKey, err := h.crypto.Decrypt(apiKey.KeyEncrypted); err == nil {
				prefix, keyLen = svc.DetectKeyFormat(realKey, prefix, keyLen)
			}
		}
	}
	placeholder, err := svc.GeneratePlaceholder(prefix, keyLen)
	if err != nil {
		return false, err
	}
	envName := entry.EnvName
	if envName == "" {
		envName = defaultEnvName(service.Name)
	}
	pid, _ := svc.GenerateToken(16)
	suiteRef := entry.SuiteID
	pk := &models.PlaceholderKey{
		ID:          pid,
		EnvName:     envName,
		Placeholder: placeholder,
		ServiceID:   entry.ServiceID,
		APIKeyID:    entry.APIKeyID,
		GroupID:     entry.GroupID,
		ClientID:    clientID,
		SuiteID:     &suiteRef,
		IsActive:    true,
	}
	if err := h.placeholders.Create(pk); err != nil {
		return false, err
	}
	return true, nil
}

// POST /api/key-suites/{id}/assign
// Assigns a suite to a client. Returns conflict warnings; individual (non-suite)
// assignments always take priority and are never replaced.
//
// Body: {"client_id": "...", "force": false}
// Response: {"assigned": [...service names], "skipped": [...service names (conflicts)]}
func (h *KeySuiteHandler) AssignToClient(w http.ResponseWriter, r *http.Request) {
	suiteID := r.PathValue("id")
	suite, err := h.suites.GetByID(suiteID)
	if err != nil {
		jsonError(w, "suite not found", http.StatusNotFound)
		return
	}

	var req struct {
		ClientID string `json:"client_id"`
		Force    bool   `json:"force"` // if false and conflicts exist, return 409 with conflict info
	}
	if err := parseRequest(r, &req); err != nil || req.ClientID == "" {
		jsonError(w, "client_id is required", http.StatusBadRequest)
		return
	}

	// Check for conflicts (client already has individual assignment for a service in the suite)
	conflictServiceIDs, err := h.suites.CheckConflicts(suiteID, req.ClientID)
	if err != nil {
		jsonError(w, "conflict check failed", http.StatusInternalServerError)
		return
	}
	conflictSet := make(map[string]bool, len(conflictServiceIDs))
	for _, id := range conflictServiceIDs {
		conflictSet[id] = true
	}

	// Return 409 with conflict details if there are conflicts and force=false
	if len(conflictServiceIDs) > 0 && !req.Force {
		type conflictInfo struct {
			ServiceID   string `json:"service_id"`
			ServiceName string `json:"service_name"`
		}
		var conflicts []conflictInfo
		for _, e := range suite.Entries {
			if conflictSet[e.ServiceID] {
				conflicts = append(conflicts, conflictInfo{ServiceID: e.ServiceID, ServiceName: e.ServiceName})
			}
		}
		w.WriteHeader(http.StatusConflict)
		jsonResponse(w, map[string]interface{}{
			"error":     "conflicts_detected",
			"message":   "Client already has individual key assignments for some services. Individual assignments take priority. Pass force:true to proceed (suite entries for those services will be skipped).",
			"conflicts": conflicts,
		})
		return
	}

	// Create placeholders for non-conflicting entries
	var assigned, skipped []string
	if err := h.suites.AssignClient(suiteID, req.ClientID); err != nil {
		jsonError(w, "failed to record suite assignment: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, entry := range suite.Entries {
		if conflictSet[entry.ServiceID] {
			skipped = append(skipped, entry.ServiceName)
			continue
		}

		service, err := h.services.GetByID(entry.ServiceID)
		if err != nil {
			skipped = append(skipped, entry.ServiceID)
			continue
		}

		prefix, keyLen := service.KeyPrefix, service.KeyLength
		if entry.APIKeyID != nil && h.apiKeys != nil && h.crypto != nil {
			if apiKey, err := h.apiKeys.GetByID(*entry.APIKeyID); err == nil {
				if realKey, err := h.crypto.Decrypt(apiKey.KeyEncrypted); err == nil {
					prefix, keyLen = svc.DetectKeyFormat(realKey, prefix, keyLen)
				}
			}
		}
		placeholder, err := svc.GeneratePlaceholder(prefix, keyLen)
		if err != nil {
			skipped = append(skipped, service.Name)
			continue
		}

		envName := entry.EnvName
		if envName == "" {
			envName = defaultEnvName(service.Name)
		}

		// Remove any existing suite placeholder for this service (from a different suite)
		if existing, err := h.placeholders.GetByClientAndService(req.ClientID, entry.ServiceID); err == nil {
			if existing.SuiteID != nil {
				h.placeholders.Delete(existing.ID)
			}
		} else if err != sql.ErrNoRows {
			log.Printf("[key-suites] unexpected error looking up placeholder for client %s service %s: %v", req.ClientID, entry.ServiceID, err)
		}

		pid, _ := svc.GenerateToken(16)
		suiteRef := suiteID
		pk := &models.PlaceholderKey{
			ID:          pid,
			EnvName:     envName,
			Placeholder: placeholder,
			ServiceID:   entry.ServiceID,
			APIKeyID:    entry.APIKeyID,
			GroupID:     entry.GroupID,
			ClientID:    req.ClientID,
			SuiteID:     &suiteRef,
			IsActive:    true,
		}
		if err := h.placeholders.Create(pk); err != nil {
			skipped = append(skipped, service.Name)
			continue
		}
		assigned = append(assigned, service.Name)
	}

	if assigned == nil {
		assigned = []string{}
	}
	if skipped == nil {
		skipped = []string{}
	}

	jsonResponse(w, map[string]interface{}{
		"assigned": assigned,
		"skipped":  skipped,
	})
}
