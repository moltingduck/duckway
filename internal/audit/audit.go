// Package audit records who changed what in the admin console (fx1116).
//
// The recorder is small on purpose:
//
//	audit.Record(ctx, audit.Entry{Actor: "discord:123", Via: "discord",
//	    Action: "approval.approve", ResourceType: "approval", ResourceID: id, Summary: "..."})
//
// Record never returns an error and never panics: a failed audit write is
// logged and the caller's action carries on. Before/After are redacted before
// they are stored; secrets never reach the table.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// Entry is one audit event. Before and After may be any JSON-marshalable value
// (or an already-encoded JSON string); they are redacted on the way in.
type Entry struct {
	Actor        string // admin:<user>, approver:<name>, discord:<id>, telegram:<id>, system:<x>, anonymous
	Via          string // web, api, tui, discord, telegram, system
	RemoteAddr   string
	Action       string // e.g. placeholder.update
	ResourceType string
	ResourceID   string
	ResourceName string
	Summary      string
	Before       any
	After        any
	Status       string // ok (default) or failed
}

// Recorder writes entries to admin_audit_log.
type Recorder struct {
	q   *queries.AuditQueries
	now func() time.Time

	mu      sync.Mutex
	signins map[string]*signinBurst
}

type signinBurst struct {
	minute int64
	id     int64
	count  int
}

// New returns a Recorder backed by db.
func New(db *sql.DB) *Recorder {
	return &Recorder{q: queries.NewAuditQueries(db), now: time.Now, signins: map[string]*signinBurst{}}
}

// SetClock replaces the recorder's time source (tests; safe at any time).
func (r *Recorder) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	r.now = now
}

func (r *Recorder) clock() time.Time {
	r.mu.Lock()
	f := r.now
	r.mu.Unlock()
	return f()
}

// Queries exposes the read side (the /api/audit handler uses it).
func (r *Recorder) Queries() *queries.AuditQueries { return r.q }

type ctxKey int

const (
	recorderKey ctxKey = iota
	identityKey
)

var defaultRecorder atomic.Pointer[Recorder]

// SetDefault installs the process-wide recorder used by Record when the
// context carries none (chat and TUI approval paths run outside an HTTP request).
func SetDefault(r *Recorder) { defaultRecorder.Store(r) }

// WithRecorder returns ctx carrying r.
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey, r)
}

// Identity is the actor of a request, stored in the context so deeper code can
// call Record without knowing the request.
type Identity struct{ Actor, Via, RemoteAddr string }

// IdentityFrom returns the identity stored in ctx (zero when none).
func IdentityFrom(ctx context.Context) Identity {
	if ctx == nil {
		return Identity{}
	}
	id, _ := ctx.Value(identityKey).(Identity)
	return id
}

// WithIdentity returns ctx carrying the actor.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// RemoteHost returns the host part of the request's remote address.
func RemoteHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Record stores e using the recorder in ctx (or the default). It fills Actor,
// Via and RemoteAddr from the context identity when e leaves them empty.
func Record(ctx context.Context, e Entry) {
	r, _ := ctx.Value(recorderKey).(*Recorder)
	if r == nil {
		r = defaultRecorder.Load()
	}
	if r == nil {
		return
	}
	r.Record(ctx, e)
}

// Record stores e. It reports nothing to the caller: failures are logged.
func (r *Recorder) Record(ctx context.Context, e Entry) {
	_ = r.record(ctx, e)
}

func (r *Recorder) record(ctx context.Context, e Entry) (id int64) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("audit: record panic: %v", p)
			id = 0
		}
	}()
	if ctx != nil {
		if ident, ok := ctx.Value(identityKey).(Identity); ok {
			if e.Actor == "" {
				e.Actor = ident.Actor
			}
			if e.Via == "" {
				e.Via = ident.Via
			}
			if e.RemoteAddr == "" {
				e.RemoteAddr = ident.RemoteAddr
			}
		}
	}
	if e.Actor == "" {
		e.Actor = "anonymous"
	}
	before, after := RedactPair(e.Before, e.After)
	row := queries.AuditRow{
		CreatedAt: queries.UTCNow(r.clock()), Actor: clip(e.Actor, 200), Via: clip(e.Via, 32), RemoteAddr: clip(e.RemoteAddr, 100),
		Action: clip(e.Action, 100), ResourceType: clip(e.ResourceType, 64), ResourceID: clip(e.ResourceID, 200),
		ResourceName: clip(e.ResourceName, 300), Summary: clip(e.Summary, 1000),
		Before: before, After: after, Status: e.Status,
	}
	n, err := r.q.Insert(row)
	if err != nil {
		log.Printf("audit: write %s failed: %v", e.Action, err)
		return 0
	}
	return n
}

// truncUTF8 cuts s to at most n bytes without splitting a rune (Postgres
// rejects invalid UTF-8, which would lose the whole row).
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return truncUTF8(strings.ToValidUTF8(s, "\uFFFD"), n)
}

// UnknownUser is stored instead of a username that matches no admin user.
const UnknownUser = "(unknown user)"

// RecordSignIn records a sign-in. Failures from one address collapse into one
// row per minute whose summary carries the running count.
func (r *Recorder) RecordSignIn(ctx context.Context, username string, ok bool, remoteAddr, via string) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("audit: signin panic: %v", p)
		}
	}()
	if via == "" {
		via = "web"
	}
	if ok {
		r.record(ctx, Entry{Actor: "admin:" + username, Via: via, RemoteAddr: remoteAddr, Action: "signin.success",
			ResourceType: "signin", ResourceName: username, Summary: "signed in"})
		return
	}
	tried := clip(username, 64)
	minute := r.clock().Unix() / 60
	// The mutex guards only the burst map; DB writes happen outside it so a slow
	// database cannot serialize every failed login.
	r.mu.Lock()
	if len(r.signins) > 2048 {
		for k, b := range r.signins {
			if b.minute != minute {
				delete(r.signins, k)
			}
		}
	}
	if b := r.signins[remoteAddr]; b != nil && b.minute == minute {
		b.count++
		id, count := b.id, b.count
		r.mu.Unlock()
		if id > 0 { // id 0: the first row is still being written; it re-reads the count
			if err := r.q.UpdateSummary(id, fmt.Sprintf("%d failed sign-ins in this minute, latest tried %q", count, tried)); err != nil {
				log.Printf("audit: signin throttle update failed: %v", err)
			}
		}
		return
	}
	b := &signinBurst{minute: minute, count: 1}
	r.signins[remoteAddr] = b
	r.mu.Unlock()

	id := r.record(ctx, Entry{Actor: "anonymous", Via: via, RemoteAddr: remoteAddr, Action: "signin.failed",
		ResourceType: "signin", ResourceName: tried, Summary: fmt.Sprintf("failed sign-in, tried %q", tried), Status: "failed"})
	r.mu.Lock()
	b.id = id
	count := b.count
	r.mu.Unlock()
	if id > 0 && count > 1 {
		if err := r.q.UpdateSummary(id, fmt.Sprintf("%d failed sign-ins in this minute, latest tried %q", count, tried)); err != nil {
			log.Printf("audit: signin throttle update failed: %v", err)
		}
	}
}

/* ---------------- redaction ---------------- */

var secretNamer atomic.Pointer[func(string) bool]

// SetSecretNamer plugs in the console's field-name redaction rules (handlers
// owns them). Names it flags are treated as secret in addition to the built-in list.
func SetSecretNamer(f func(string) bool) { secretNamer.Store(&f) }

var visibleNames = map[string]bool{"token_endpoint": true, "token_type": true, "tokens_remaining": true, "tokens_limit": true, "has_token": true}

func builtinSecret(name string) bool {
	n := strings.ToLower(name)
	if visibleNames[n] {
		return false
	}
	if n == "key" || n == "keys" || strings.HasSuffix(n, "_key") || strings.HasSuffix(n, "-key") {
		return true
	}
	for _, w := range []string{"secret", "password", "passwd", "token", "webhook", "authorization", "credential", "private_key", "api_key", "apikey", "encrypted", "hash", "cookie", "bot_token", "passphrase"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

func isSecret(name string) bool {
	if n := strings.ToLower(name); n == "id" || strings.HasSuffix(n, "_id") || strings.HasSuffix(n, "-id") {
		return false // identifiers (api_key_id, client_id) name a secret's owner, not the secret
	}
	if builtinSecret(name) {
		return true
	}
	if f := secretNamer.Load(); f != nil {
		return (*f)(name)
	}
	return false
}

// Masks stored in place of secret values.
const (
	MaskSet     = "(set)"
	MaskChanged = "(changed)"
)

func normalize(v any) any {
	if v == nil {
		return nil
	}
	var raw []byte
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		raw = b
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func empty(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return s == ""
	}
	return false
}

// redactWalk masks secret-named fields in cur, comparing with the same path in
// other (nil when absent) so an edit shows "(changed)" instead of "(set)".
func redactWalk(cur, other any, isAfter bool) any {
	switch t := cur.(type) {
	case map[string]any:
		om, _ := other.(map[string]any)
		for k, child := range t {
			var oc any
			if om != nil {
				oc = om[k]
			}
			if isSecret(k) {
				switch {
				case empty(child):
					t[k] = ""
				case !isAfter:
					t[k] = MaskSet // the "before" side only says a value existed
				case om != nil && !empty(oc) && !reflect.DeepEqual(child, oc):
					t[k] = MaskChanged
				case om != nil && empty(oc):
					t[k] = MaskChanged
				default:
					t[k] = MaskSet
				}
				continue
			}
			t[k] = redactWalk(child, oc, isAfter)
		}
		return t
	case []any:
		oa, _ := other.([]any)
		for i := range t {
			var oc any
			if i < len(oa) {
				oc = oa[i]
			}
			t[i] = redactWalk(t[i], oc, isAfter)
		}
		return t
	case string:
		if len(t) > maxStringBytes {
			return fmt.Sprintf("%s... (%d bytes)", truncUTF8(t, maxStringBytes), len(t))
		}
		return t
	default:
		return cur
	}
}

// maxStringBytes bounds one string value in a stored snapshot (a statusline script can be 64 KB).
const maxStringBytes = 2000

// RedactPair returns the compact, redacted JSON for a before/after pair ("" when nil).
func RedactPair(before, after any) (string, string) {
	b, a := normalize(before), normalize(after)
	// Comparisons need the unredacted peers, so walk copies.
	bc, ac := normalize(before), normalize(after)
	b = redactWalk(b, ac, false)
	a = redactWalk(a, bc, true)
	return encode(b), encode(a)
}

func encode(v any) string {
	if v == nil {
		return ""
	}
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}
