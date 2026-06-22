package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

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

	cookie := h.auth.CreateSession(user.Username, r)
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

// ChangePassword lets the currently logged-in admin change their own password.
// POST /api/auth/change-password  {"current_password":"...","new_password":"..."}
func (h *AuthHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	username, _ := r.Context().Value(middleware.AdminUserKey).(string)
	if username == "" {
		jsonError(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		jsonError(w, "current_password and new_password are required", http.StatusBadRequest)
		return
	}
	if len(req.NewPassword) < 8 {
		jsonError(w, "new password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	user, err := h.users.GetByUsername(username)
	if err != nil {
		jsonError(w, "user not found", http.StatusInternalServerError)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.CurrentPassword)); err != nil {
		jsonError(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, "failed to hash password", http.StatusInternalServerError)
		return
	}
	if err := h.users.UpdatePassword(username, string(hash)); err != nil {
		jsonError(w, "failed to update password", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

func JsonErrorPublic(w http.ResponseWriter, msg string, code int) { jsonError(w, msg, code) }
func JsonResponsePublic(w http.ResponseWriter, data interface{})  { jsonResponse(w, data) }

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// parseRequest decodes JSON or form-encoded body into dest struct.
// For form data, it uses field names from json tags.
func parseRequest(r *http.Request, dest interface{}) error {
	ct := r.Header.Get("Content-Type")
	if ct == "application/x-www-form-urlencoded" || strings.HasPrefix(ct, "multipart/form-data") {
		r.ParseForm()
		return formToStruct(r.Form, dest)
	}
	return json.NewDecoder(r.Body).Decode(dest)
}

// formToStruct maps form values to struct fields using json tags.
func formToStruct(form map[string][]string, dest interface{}) error {
	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Ptr || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("dest must be a pointer to struct")
	}
	v = v.Elem()
	t := v.Type()

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		tag = strings.Split(tag, ",")[0]

		vals, ok := form[tag]
		if !ok || len(vals) == 0 || vals[0] == "" {
			continue
		}
		val := vals[0]

		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.String:
			fv.SetString(val)
		case reflect.Int, reflect.Int64:
			n, _ := strconv.ParseInt(val, 10, 64)
			fv.SetInt(n)
		case reflect.Bool:
			fv.SetBool(val == "true" || val == "1" || val == "on")
		case reflect.Ptr:
			if fv.Type().Elem().Kind() == reflect.String {
				fv.Set(reflect.ValueOf(&val))
			}
		}
	}
	return nil
}
