package services_test

import (
	"database/sql"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

// resolverFixture holds a live KeyResolver wired to an in-memory SQLite DB
// and a Crypto instance that tests can use to encrypt/decrypt keys.
type resolverFixture struct {
	db           *sql.DB
	resolver     *services.KeyResolver
	crypto       *services.Crypto
	apiKeys      *queries.APIKeyQueries
	placeholders *queries.PlaceholderQueries
	groups       *queries.GroupQueries
	approvals    *queries.ApprovalQueries
}

func newFixture(t *testing.T) *resolverFixture {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// AES-256-GCM requires exactly 32 bytes.
	cryptoKey := make([]byte, 32)
	copy(cryptoKey, []byte("test-aes-key-for-resolver-tests!"))
	cr := services.NewCrypto(cryptoKey)

	apiKeys := queries.NewAPIKeyQueries(db)
	placeholders := queries.NewPlaceholderQueries(db)
	grps := queries.NewGroupQueries(db)
	approvals := queries.NewApprovalQueries(db)

	r := services.NewKeyResolver(cr, apiKeys, placeholders, grps, approvals)

	return &resolverFixture{
		db:           db,
		resolver:     r,
		crypto:       cr,
		apiKeys:      apiKeys,
		placeholders: placeholders,
		groups:       grps,
		approvals:    approvals,
	}
}

// insertService inserts a minimal active service row and returns its ID.
func (f *resolverFixture) insertService(t *testing.T, id, name, deliveryMode string) {
	t.Helper()
	_, err := f.db.Exec(
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern, delivery_mode)
		 VALUES (?, ?, ?, 'https://api.example.com', 'api.example.com', ?)`,
		id, name, name, deliveryMode,
	)
	if err != nil {
		t.Fatalf("insertService %s: %v", id, err)
	}
}

// insertClient inserts a minimal active client row.
func (f *resolverFixture) insertClient(t *testing.T, id, name string) {
	t.Helper()
	_, err := f.db.Exec(
		`INSERT INTO clients (id, name, token_hash) VALUES (?, ?, ?)`,
		id, name, "hash-"+id,
	)
	if err != nil {
		t.Fatalf("insertClient %s: %v", id, err)
	}
}

// insertAPIKey inserts an api_key row with an encrypted real key value.
// Returns the encrypted form (for direct SQL inserts to group members etc).
func (f *resolverFixture) insertAPIKey(t *testing.T, id, serviceID, realKey string) string {
	t.Helper()
	encrypted, err := f.crypto.Encrypt(realKey)
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	k := &models.APIKey{
		ID:           id,
		ServiceID:    serviceID,
		Name:         "key-" + id,
		KeyEncrypted: encrypted,
		IsActive:     true,
	}
	if err := f.apiKeys.Create(k); err != nil {
		t.Fatalf("insertAPIKey %s: %v", id, err)
	}
	return encrypted
}

// insertPlaceholder inserts a placeholder_keys row pointing at a direct api key.
func (f *resolverFixture) insertPlaceholderDirect(t *testing.T, id, placeholder, serviceID, apiKeyID, clientID string, requiresApproval bool) {
	t.Helper()
	keyID := apiKeyID
	p := &models.PlaceholderKey{
		ID:               id,
		EnvName:          "OPENAI_API_KEY",
		Placeholder:      placeholder,
		ServiceID:        serviceID,
		APIKeyID:         &keyID,
		ClientID:         clientID,
		RequiresApproval: requiresApproval,
		IsActive:         true,
	}
	if err := f.placeholders.Create(p); err != nil {
		t.Fatalf("insertPlaceholderDirect %s: %v", id, err)
	}
}

// insertPlaceholderGroup inserts a placeholder_keys row pointing at a group.
func (f *resolverFixture) insertPlaceholderGroup(t *testing.T, id, placeholder, serviceID, groupID, clientID string) {
	t.Helper()
	gid := groupID
	p := &models.PlaceholderKey{
		ID:               id,
		EnvName:          "OPENAI_API_KEY",
		Placeholder:      placeholder,
		ServiceID:        serviceID,
		GroupID:          &gid,
		ClientID:         clientID,
		RequiresApproval: false,
		IsActive:         true,
	}
	if err := f.placeholders.Create(p); err != nil {
		t.Fatalf("insertPlaceholderGroup %s: %v", id, err)
	}
}

// ---- Tests ----

func TestKeyResolver_Resolve_UnknownPlaceholder(t *testing.T) {
	f := newFixture(t)

	result, err := f.resolver.Resolve("dk_nonexistent", "client-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Permitted {
		t.Error("expected permitted=false for unknown placeholder")
	}
	if result.Error == "" {
		t.Error("expected non-empty Error for unknown placeholder")
	}
}

func TestKeyResolver_Resolve_ActivePlaceholderReturnsRealKey(t *testing.T) {
	f := newFixture(t)

	const (
		svcID     = "svc-1"
		keyID     = "key-1"
		clientID  = "client-1"
		phID      = "ph-1"
		phToken   = "dk_placeholder_abc123"
		realKey   = "sk-real-api-key-value"
	)

	f.insertService(t, svcID, "openai", "proxy")
	f.insertClient(t, clientID, "test-client")
	f.insertAPIKey(t, keyID, svcID, realKey)
	f.insertPlaceholderDirect(t, phID, phToken, svcID, keyID, clientID, false)

	result, err := f.resolver.Resolve(phToken, clientID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !result.Permitted {
		t.Errorf("expected permitted=true, got error: %s", result.Error)
	}
	if result.RealKey != realKey {
		t.Errorf("RealKey = %q, want %q", result.RealKey, realKey)
	}
	if result.PlaceholderID != phID {
		t.Errorf("PlaceholderID = %q, want %q", result.PlaceholderID, phID)
	}
	if result.APIKeyID != keyID {
		t.Errorf("APIKeyID = %q, want %q", result.APIKeyID, keyID)
	}
}

func TestKeyResolver_Resolve_WrongClientID(t *testing.T) {
	f := newFixture(t)

	const svcID, keyID, clientID, phToken = "svc-1", "key-1", "client-1", "dk_tok1"

	f.insertService(t, svcID, "openai", "proxy")
	f.insertClient(t, clientID, "test-client")
	f.insertAPIKey(t, keyID, svcID, "sk-real")
	f.insertPlaceholderDirect(t, "ph-1", phToken, svcID, keyID, clientID, false)

	result, err := f.resolver.Resolve(phToken, "wrong-client")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Permitted {
		t.Error("expected permitted=false when client ID does not match")
	}
}

// TestKeyResolver_RoundRobin_IncrementLastIndex verifies the atomic
// IncrementLastIndex helper returns 0, then 1 for consecutive calls on a
// 2-member group, without any TOCTOU race.
func TestKeyResolver_RoundRobin_IncrementLastIndex(t *testing.T) {
	f := newFixture(t)

	const svcID = "svc-rr"
	const groupID = "grp-rr"

	f.insertService(t, svcID, "openai-rr", "proxy")

	_, err := f.db.Exec(
		`INSERT INTO api_key_groups (id, service_id, name, strategy, last_index) VALUES (?, ?, ?, ?, ?)`,
		groupID, svcID, "round-robin-group", "round-robin", 0,
	)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	const memberCount = 2

	idx0, err := f.groups.IncrementLastIndex(groupID, memberCount)
	if err != nil {
		t.Fatalf("first IncrementLastIndex: %v", err)
	}
	if idx0 != 0 {
		t.Errorf("first call: got index %d, want 0", idx0)
	}

	idx1, err := f.groups.IncrementLastIndex(groupID, memberCount)
	if err != nil {
		t.Fatalf("second IncrementLastIndex: %v", err)
	}
	if idx1 != 1 {
		t.Errorf("second call: got index %d, want 1", idx1)
	}

	// Third call wraps back to 0 (last_index=2, 2 % 2 == 0).
	idx2, err := f.groups.IncrementLastIndex(groupID, memberCount)
	if err != nil {
		t.Fatalf("third IncrementLastIndex: %v", err)
	}
	if idx2 != 0 {
		t.Errorf("third call (wrap-around): got index %d, want 0", idx2)
	}
}

// TestKeyResolver_Resolve_RoundRobin verifies that two successive Resolve calls
// on a placeholder backed by a 2-member round-robin group return the two
// different real keys in alternating order.
func TestKeyResolver_Resolve_RoundRobin(t *testing.T) {
	f := newFixture(t)

	const (
		svcID    = "svc-rr2"
		groupID  = "grp-rr2"
		clientID = "client-rr"
		phToken  = "dk_rr_placeholder"
	)

	f.insertService(t, svcID, "openai-rr2", "proxy")
	f.insertClient(t, clientID, "rr-client")

	_, err := f.db.Exec(
		`INSERT INTO api_key_groups (id, service_id, name, strategy, last_index) VALUES (?, ?, ?, ?, ?)`,
		groupID, svcID, "rr-group", "round-robin", 0,
	)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	f.insertAPIKey(t, "key-rr-a", svcID, "sk-real-A")
	f.insertAPIKey(t, "key-rr-b", svcID, "sk-real-B")

	if err := f.groups.AddMember(groupID, "key-rr-a", 0); err != nil {
		t.Fatalf("AddMember A: %v", err)
	}
	if err := f.groups.AddMember(groupID, "key-rr-b", 1); err != nil {
		t.Fatalf("AddMember B: %v", err)
	}

	f.insertPlaceholderGroup(t, "ph-rr", phToken, svcID, groupID, clientID)

	r1, err := f.resolver.Resolve(phToken, clientID)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if !r1.Permitted {
		t.Fatalf("first Resolve not permitted: %s", r1.Error)
	}

	r2, err := f.resolver.Resolve(phToken, clientID)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if !r2.Permitted {
		t.Fatalf("second Resolve not permitted: %s", r2.Error)
	}

	if r1.RealKey == r2.RealKey {
		t.Errorf("round-robin delivered the same key twice in a row: %q", r1.RealKey)
	}
	keys := map[string]bool{"sk-real-A": true, "sk-real-B": true}
	if !keys[r1.RealKey] {
		t.Errorf("first key %q not in expected set", r1.RealKey)
	}
	if !keys[r2.RealKey] {
		t.Errorf("second key %q not in expected set", r2.RealKey)
	}
}

// TestKeyResolver_Resolve_ApprovalRequired_NeedApproval verifies that when a
// placeholder has requires_approval=true and no approved approval exists, the
// resolver returns NeedApproval=true and Permitted=false.
func TestKeyResolver_Resolve_ApprovalRequired_NeedApproval(t *testing.T) {
	f := newFixture(t)

	const (
		svcID    = "svc-appr"
		keyID    = "key-appr"
		clientID = "client-appr"
		phID     = "ph-appr"
		phToken  = "dk_approval_required"
	)

	f.insertService(t, svcID, "openai-appr", "proxy")
	f.insertClient(t, clientID, "appr-client")
	f.insertAPIKey(t, keyID, svcID, "sk-approval-key")
	f.insertPlaceholderDirect(t, phID, phToken, svcID, keyID, clientID, true /* requiresApproval */)

	// No approval row exists.
	result, err := f.resolver.Resolve(phToken, clientID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Permitted {
		t.Error("expected permitted=false when approval is required but none exists")
	}
	if !result.NeedApproval {
		t.Error("expected NeedApproval=true when approval is required but none exists")
	}
	if result.PlaceholderID != phID {
		t.Errorf("PlaceholderID = %q, want %q", result.PlaceholderID, phID)
	}
}

// TestKeyResolver_Resolve_ApprovalGranted verifies that after inserting a valid
// approved approval row, the resolver returns Permitted=true.
func TestKeyResolver_Resolve_ApprovalGranted(t *testing.T) {
	f := newFixture(t)

	const (
		svcID    = "svc-appr2"
		keyID    = "key-appr2"
		clientID = "client-appr2"
		phID     = "ph-appr2"
		phToken  = "dk_approval_granted"
		realKey  = "sk-approved-key-value"
	)

	f.insertService(t, svcID, "openai-appr2", "proxy")
	f.insertClient(t, clientID, "appr2-client")
	f.insertAPIKey(t, keyID, svcID, realKey)
	f.insertPlaceholderDirect(t, phID, phToken, svcID, keyID, clientID, true)

	// Insert an approved approval that does not expire.
	approval := &models.Approval{
		ID:            "appr-1",
		PlaceholderID: phID,
		Status:        "approved",
	}
	if err := f.approvals.Create(approval); err != nil {
		t.Fatalf("create approval: %v", err)
	}
	// Approve with a far-future expiry.
	if err := f.approvals.Approve("appr-1", "2099-01-01 00:00:00"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	result, err := f.resolver.Resolve(phToken, clientID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !result.Permitted {
		t.Errorf("expected permitted=true after approval, got error: %s", result.Error)
	}
	if result.RealKey != realKey {
		t.Errorf("RealKey = %q, want %q", result.RealKey, realKey)
	}
}

// TestKeyResolver_Resolve_InactiveAPIKey verifies that an inactive underlying
// api_key causes permitted=false.
func TestKeyResolver_Resolve_InactiveAPIKey(t *testing.T) {
	f := newFixture(t)

	const (
		svcID    = "svc-inactive"
		keyID    = "key-inactive"
		clientID = "client-inactive"
		phToken  = "dk_inactive_key"
	)

	f.insertService(t, svcID, "openai-inactive", "proxy")
	f.insertClient(t, clientID, "inactive-client")
	f.insertAPIKey(t, keyID, svcID, "sk-inactive")
	f.insertPlaceholderDirect(t, "ph-inactive", phToken, svcID, keyID, clientID, false)

	// Deactivate the underlying API key.
	if err := f.apiKeys.Deactivate(keyID); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	result, err := f.resolver.Resolve(phToken, clientID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Permitted {
		t.Error("expected permitted=false when underlying api_key is inactive")
	}
}
