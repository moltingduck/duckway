package handlers_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
)

// newApprovalDB opens an in-memory-like SQLite DB with full migrations and
// seeds the minimum rows that approval FK constraints require:
//
//	services → clients → placeholder_keys
//
// Returns the db and the placeholder_id to use when inserting approvals.
func newApprovalDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Insert the prerequisite rows.  Column lists are explicit so that
	// future schema additions don't break these seeding statements.
	_, err = db.Exec(
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		 VALUES ('svc-appr', 'test-svc', 'Test Service', 'https://example.com', 'example.com')`,
	)
	if err != nil {
		t.Fatalf("seed service: %v", err)
	}

	_, err = db.Exec(
		`INSERT INTO clients (id, name, token_hash) VALUES ('cli-appr', 'test-client', 'hash-abc')`,
	)
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}

	// placeholder_keys.CHECK requires exactly one of api_key_id/group_id to be
	// set; we use a direct api_key first, so insert a key row.
	_, err = db.Exec(
		`INSERT INTO api_keys (id, service_id, name, key_encrypted)
		 VALUES ('key-appr', 'svc-appr', 'test-key', 'enc-val')`,
	)
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}

	const phID = "ph-appr-01"
	_, err = db.Exec(
		`INSERT INTO placeholder_keys
		 (id, env_name, placeholder, service_id, api_key_id, client_id)
		 VALUES (?, 'ANTHROPIC_API_KEY', 'placeholder-val', 'svc-appr', 'key-appr', 'cli-appr')`,
		phID,
	)
	if err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}

	return db, phID
}

// newApprovalMux wires ApprovalHandler into a ServeMux whose patterns use
// PathValue — matching the real server's routing.
func newApprovalMux(t *testing.T, db *sql.DB) *http.ServeMux {
	t.Helper()
	approvalQ := queries.NewApprovalQueries(db)
	placeholderQ := queries.NewPlaceholderQueries(db)
	h := handlers.NewApprovalHandler(approvalQ, placeholderQ)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/approvals/{id}/approve", h.Approve)
	mux.HandleFunc("POST /api/approvals/{id}/reject", h.Reject)
	return mux
}

// insertApproval inserts a row directly so tests can set arbitrary statuses.
func insertApproval(t *testing.T, db *sql.DB, id, phID, status string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO approvals (id, placeholder_id, status) VALUES (?, ?, ?)`,
		id, phID, status,
	)
	if err != nil {
		t.Fatalf("insert approval %s: %v", id, err)
	}
}

func doApproval(t *testing.T, mux *http.ServeMux, method, path string) (int, map[string]json.RawMessage) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// ---- Approve tests ----

func TestApprove_NonExistentID_Returns404(t *testing.T) {
	db, _ := newApprovalDB(t)
	mux := newApprovalMux(t, db)

	code, _ := doApproval(t, mux, "POST", "/api/approvals/does-not-exist/approve")
	if code != http.StatusNotFound {
		t.Errorf("approve non-existent: want 404, got %d", code)
	}
}

func TestApprove_PendingApproval_Returns200(t *testing.T) {
	db, phID := newApprovalDB(t)
	insertApproval(t, db, "appr-pending-01", phID, "pending")
	mux := newApprovalMux(t, db)

	code, out := doApproval(t, mux, "POST", "/api/approvals/appr-pending-01/approve")
	if code != http.StatusOK {
		t.Errorf("approve pending: want 200, got %d, body=%s", code, out)
	}
	if string(out["status"]) != `"approved"` {
		t.Errorf("approve pending: body status = %s, want \"approved\"", out["status"])
	}
}

func TestApprove_AlreadyApproved_Returns409(t *testing.T) {
	db, phID := newApprovalDB(t)
	insertApproval(t, db, "appr-already-approved", phID, "approved")
	mux := newApprovalMux(t, db)

	code, _ := doApproval(t, mux, "POST", "/api/approvals/appr-already-approved/approve")
	if code != http.StatusConflict {
		t.Errorf("approve already-approved: want 409, got %d", code)
	}
}

func TestApprove_AlreadyRejected_Returns409(t *testing.T) {
	db, phID := newApprovalDB(t)
	insertApproval(t, db, "appr-already-rejected", phID, "rejected")
	mux := newApprovalMux(t, db)

	code, _ := doApproval(t, mux, "POST", "/api/approvals/appr-already-rejected/approve")
	if code != http.StatusConflict {
		t.Errorf("approve already-rejected: want 409, got %d", code)
	}
}

// ---- Reject tests ----

func TestReject_NonExistentID_Returns404(t *testing.T) {
	db, _ := newApprovalDB(t)
	mux := newApprovalMux(t, db)

	code, _ := doApproval(t, mux, "POST", "/api/approvals/no-such-id/reject")
	if code != http.StatusNotFound {
		t.Errorf("reject non-existent: want 404, got %d", code)
	}
}

func TestReject_PendingApproval_Returns200(t *testing.T) {
	db, phID := newApprovalDB(t)
	insertApproval(t, db, "appr-reject-pending", phID, "pending")
	mux := newApprovalMux(t, db)

	code, out := doApproval(t, mux, "POST", "/api/approvals/appr-reject-pending/reject")
	if code != http.StatusOK {
		t.Errorf("reject pending: want 200, got %d, body=%s", code, out)
	}
	if string(out["status"]) != `"rejected"` {
		t.Errorf("reject pending: body status = %s, want \"rejected\"", out["status"])
	}
}

func TestReject_AlreadyRejected_Returns409(t *testing.T) {
	db, phID := newApprovalDB(t)
	insertApproval(t, db, "appr-reject-again", phID, "rejected")
	mux := newApprovalMux(t, db)

	code, _ := doApproval(t, mux, "POST", "/api/approvals/appr-reject-again/reject")
	if code != http.StatusConflict {
		t.Errorf("reject already-rejected: want 409, got %d", code)
	}
}
