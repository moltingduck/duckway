package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct {
	users *queries.AdminUserQueries
	auth  *middleware.AdminAuth
}

func NewAuthHandler(users *queries.AdminUserQueries, auth *middleware.AdminAuth) *AuthHandler {
	return &AuthHandler{users: users, auth: auth}
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	user, err := h.users.GetByUsername(req.Username)
	if err != nil {
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	cookie := h.auth.CreateSession(user.Username)
	http.SetCookie(w, cookie)
	jsonResponse(w, map[string]string{"status": "ok", "username": user.Username})
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:   "duckway_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	jsonResponse(w, map[string]string{"status": "ok"})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
