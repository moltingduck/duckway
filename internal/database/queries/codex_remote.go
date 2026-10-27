package queries

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"strings"
	"sync"
)

// CodexRemoteEnrollment contains gateway-only remote host credentials. The
// dedicated bearer is encrypted before reaching this repository.
type CodexRemoteEnrollment struct {
	CapabilityHash, ClientID, Placeholder, APIKeyID, AccountHash string
	ServerID, InstallationID, TokenEncrypted                     string
	ExpiresAt                                                    int64
}

type CodexRemoteQueries struct {
	db        *sql.DB
	hostLocks [64]sync.Mutex
}

// LockHost serializes enrollment/rotation for an installation with bounded memory.
func (q *CodexRemoteQueries) LockHost(clientID, installation string) func() {
	hash := fnv.New64a()
	hash.Write([]byte(clientID + "\x00" + strings.TrimSpace(installation)))
	lock := &q.hostLocks[hash.Sum64()%64]
	lock.Lock()
	return lock.Unlock
}

func (q *CodexRemoteQueries) ClientActive(id string) bool {
	var active bool
	return q.db.QueryRow("SELECT is_active FROM clients WHERE id=?", id).Scan(&active) == nil && active
}

func NewCodexRemoteQueries(db *sql.DB) *CodexRemoteQueries { return &CodexRemoteQueries{db: db} }

const codexRemoteColumns = `capability_hash,client_id,placeholder,api_key_id,account_hash,server_id,installation_id,token_encrypted,expires_at`

func scanCodexRemote(row *sql.Row) (*CodexRemoteEnrollment, error) {
	v := new(CodexRemoteEnrollment)
	err := row.Scan(&v.CapabilityHash, &v.ClientID, &v.Placeholder, &v.APIKeyID, &v.AccountHash, &v.ServerID, &v.InstallationID, &v.TokenEncrypted, &v.ExpiresAt)
	return v, err
}

func (q *CodexRemoteQueries) Capability(hash, clientID string, now int64) (*CodexRemoteEnrollment, error) {
	return scanCodexRemote(q.db.QueryRow(`SELECT `+codexRemoteColumns+` FROM codex_remote_enrollments WHERE capability_hash=? AND client_id=? AND expires_at>?`, hash, clientID, now))
}

// Host remains queryable after token expiry because native refresh uses OAuth.
func (q *CodexRemoteQueries) Host(clientID, placeholder, serverID, installationID string) (*CodexRemoteEnrollment, error) {
	return scanCodexRemote(q.db.QueryRow(`SELECT `+codexRemoteColumns+` FROM codex_remote_enrollments WHERE client_id=? AND placeholder=? AND server_id=? AND installation_id=?`, clientID, placeholder, serverID, installationID))
}

// Save rotates a host capability atomically and bounds durable storage per
// authenticated client. Expired hosts retain a seven-day refresh grace period.
func (q *CodexRemoteQueries) Save(v *CodexRemoteEnrollment, now int64) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM codex_remote_enrollments WHERE expires_at<?`, now-7*24*60*60); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM codex_remote_enrollments WHERE client_id=? AND NOT (placeholder=? AND server_id=? AND installation_id=?)`, v.ClientID, v.Placeholder, v.ServerID, v.InstallationID).Scan(&count); err != nil {
		return err
	}
	if count >= 64 {
		return errors.New("remote host capacity reached")
	}
	_, err = tx.Exec(`INSERT INTO codex_remote_enrollments (`+codexRemoteColumns+`) VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(client_id,placeholder,server_id,installation_id) DO UPDATE SET capability_hash=excluded.capability_hash,api_key_id=excluded.api_key_id,account_hash=excluded.account_hash,token_encrypted=excluded.token_encrypted,expires_at=excluded.expires_at`, v.CapabilityHash, v.ClientID, v.Placeholder, v.APIKeyID, v.AccountHash, v.ServerID, v.InstallationID, v.TokenEncrypted, v.ExpiresAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RelayAuthorization is a read-only, cancellable authorization snapshot. It never
// rotates group selection or updates usage counters while monitoring a relay.
type CodexRemoteAuthorization struct {
	PlaceholderID, KeyEncrypted, SubscriptionInfo, TokenEndpoint string
	ServiceACL, KeyACL, PlaceholderACL                           string
}

func (q *CodexRemoteQueries) RelayAuthorization(ctx context.Context, binding *CodexRemoteEnrollment, now int64) (*CodexRemoteAuthorization, error) {
	v := new(CodexRemoteAuthorization)
	err := q.db.QueryRowContext(ctx, `SELECT p.id,k.key_encrypted,COALESCE(k.subscription_info,''),COALESCE(k.token_endpoint,''),COALESCE(s.default_acl,''),COALESCE(k.acl,''),COALESCE(p.permission_config,'')
 FROM codex_remote_enrollments e JOIN clients c ON c.id=e.client_id
 JOIN placeholder_keys p ON p.client_id=c.id AND p.placeholder=e.placeholder
 JOIN api_keys k ON k.id=e.api_key_id JOIN services s ON s.id=p.service_id AND s.id=k.service_id
 WHERE e.capability_hash=? AND e.client_id=? AND e.api_key_id=? AND e.account_hash=? AND e.expires_at>?
 AND c.is_active=1 AND p.is_active=1 AND k.is_active=1 AND s.is_active=1 AND s.name='openai'
 AND k.refresh_token<>'' AND (p.api_key_id=k.id OR (p.api_key_id IS NULL AND EXISTS(SELECT 1 FROM api_key_group_members m WHERE m.group_id=p.group_id AND m.api_key_id=k.id)))
 AND (p.requires_approval=0 OR EXISTS(SELECT 1 FROM approvals a WHERE a.placeholder_id=p.id AND a.status='approved' AND (a.expires_at IS NULL OR a.expires_at>datetime('now'))))`, binding.CapabilityHash, binding.ClientID, binding.APIKeyID, binding.AccountHash, now).Scan(&v.PlaceholderID, &v.KeyEncrypted, &v.SubscriptionInfo, &v.TokenEndpoint, &v.ServiceACL, &v.KeyACL, &v.PlaceholderACL)
	return v, err
}
