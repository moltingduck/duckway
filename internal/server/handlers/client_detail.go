package handlers

import (
	"net/http"
)

// clientDetailKey and clientDetailGroup are the minimal key/pool descriptors
// the "Add credential" dialog needs. They are JSON-encoded into the page by
// html/template (no secret material: names, ids and capability flags only).
type clientDetailKey struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ServiceID     string `json:"serviceId"`
	ServiceName   string `json:"serviceName"`
	IsRefreshable bool   `json:"isRefreshable"`
	IsMintable    bool   `json:"isMintable"`
}

type clientDetailGroup struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ServiceID   string `json:"serviceId"`
	ServiceName string `json:"serviceName"`
	Strategy    string `json:"strategy"`
	MemberCount int    `json:"memberCount"`
}

// ClientDetailPage serves GET /admin/clients/{id}. An unknown id renders the
// same shell with a 404 "client not found" notice instead of a bare error.
func (h *AdminHandler) ClientDetailPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	client, err := h.clients.GetByID(id)
	if err != nil || client == nil {
		w.WriteHeader(http.StatusNotFound)
		h.render(w, r, "client_detail", pageData{
			Title:  "Client not found",
			Active: "clients",
			Error:  "not_found",
		})
		return
	}

	keys, _ := h.apiKeys.List("")
	keys = h.markMintableKeys(keys)
	keyViews := make([]clientDetailKey, 0, len(keys))
	for _, k := range keys {
		keyViews = append(keyViews, clientDetailKey{
			ID: k.ID, Name: k.Name, ServiceID: k.ServiceID, ServiceName: k.ServiceName,
			IsRefreshable: k.IsRefreshable, IsMintable: k.IsMintable,
		})
	}
	groups, _ := h.groups.List("")
	groupViews := make([]clientDetailGroup, 0, len(groups))
	for _, g := range groups {
		members, _ := h.groups.GetMembers(g.ID)
		groupViews = append(groupViews, clientDetailGroup{
			ID: g.ID, Name: g.Name, ServiceID: g.ServiceID, ServiceName: g.ServiceName,
			Strategy: g.Strategy, MemberCount: len(members),
		})
	}

	h.render(w, r, "client_detail", pageData{
		Title:   client.Name,
		Active:  "clients",
		Clients: clientDetailHead{ID: client.ID, Name: client.Name, ShortID: client.ShortID},
		Keys:    keyViews,
		Groups:  groupViews,
	})
}

// clientDetailHead is the only client data rendered server-side (never the
// token hash); everything else is fetched through the admin JSON APIs.
type clientDetailHead struct {
	ID      string
	Name    string
	ShortID string
}
