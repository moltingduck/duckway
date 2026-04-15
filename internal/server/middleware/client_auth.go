package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

const ClientKey contextKey = "client"

type ClientAuth struct {
	clients *queries.ClientQueries
}

func NewClientAuth(clients *queries.ClientQueries) *ClientAuth {
	return &ClientAuth{clients: clients}
}

func (a *ClientAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Duckway-Token")
		if token == "" {
			// Also check Authorization header
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				token = strings.TrimPrefix(auth, "Bearer ")
			}
		}

		if token == "" {
			http.Error(w, `{"error":"client token required"}`, http.StatusUnauthorized)
			return
		}

		hash := services.HashToken(token)
		client, err := a.clients.GetByTokenHash(hash)
		if err != nil {
			http.Error(w, `{"error":"invalid client token"}`, http.StatusUnauthorized)
			return
		}

		// Update last seen
		a.clients.UpdateLastSeen(client.ID)

		ctx := context.WithValue(r.Context(), ClientKey, client)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func GetClient(r *http.Request) *models.Client {
	client, _ := r.Context().Value(ClientKey).(*models.Client)
	return client
}
