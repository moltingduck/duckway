package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/models"
)

// Persistent contention while creating a scoped approval answers 503 with
// Retry-After (retryable), never 500.
func TestHoldForApprovalPersistentContentionIs503WithRetryAfter(t *testing.T) {
	h := &ProxyHandler{scopedOpsWrap: func(scopedApprovalOps) scopedApprovalOps {
		return scopedApprovalOps{
			pending: func() (string, error) { return "", sql.ErrNoRows },
			valid:   func() error { return sql.ErrNoRows },
			create: func() (string, error) {
				return "", errors.New("UNIQUE constraint failed: approvals.placeholder_id, approvals.scope")
			},
		}
	}}
	rec := httptest.NewRecorder()
	h.holdForApproval(rec, httptest.NewRequest("POST", "/x", nil), &models.Client{Name: "c"}, "svc", "ph", "POST", "/x", "", "", "rule:abc", "r")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("code=%d retry-after=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}
