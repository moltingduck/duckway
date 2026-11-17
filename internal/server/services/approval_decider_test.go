package services

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

type decisionRecord struct{ id, by, via, status string }

func approvalsFixture(t *testing.T) (*sql.DB, *queries.ApprovalQueries, *ApprovalDecider, *[]decisionRecord) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('s1','svc','Svc','https://e.example','e.example')`,
		`INSERT INTO clients (id, name, token_hash) VALUES ('c1','client','h1')`,
		`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k1','s1','key','enc')`,
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, approval_ttl_minutes) VALUES ('p1','ENV','ph','s1','k1','c1', 90)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	aq := queries.NewApprovalQueries(db)
	d := NewApprovalDecider(aq)
	var mu sync.Mutex
	var recs []decisionRecord
	d.AddObserver(func(_ context.Context, id, by, via, status string) {
		mu.Lock()
		defer mu.Unlock()
		recs = append(recs, decisionRecord{id, by, via, status})
	})
	return db, aq, d, &recs
}

func addPending(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	// one pending token-wide approval per token: scope distinguishes the rows
	if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id, status, scope) VALUES (?, 'p1', 'pending', ?)`, id, "rule:"+id); err != nil {
		t.Fatal(err)
	}
}

func TestDeciderAttributesAndNotifiesEveryDecision(t *testing.T) {
	db, aq, d, recs := approvalsFixture(t)
	for _, id := range []string{"a", "b", "c"} {
		addPending(t, db, id)
	}
	ctx := context.Background()
	if err := d.Approve(ctx, "a", 0, "admin:alice", ViaWeb); err != nil {
		t.Fatal(err)
	}
	if err := d.Reject(ctx, "b", "approver:ci", ViaTUI); err != nil {
		t.Fatal(err)
	}
	if err := d.NewChatDecision()("c", true, 60, ChatActor{Kind: "discord", UserID: "42"}); err != nil {
		t.Fatal(err)
	}
	want := []decisionRecord{{"a", "admin:alice", "web", "approved"}, {"b", "approver:ci", "tui", "rejected"}, {"c", "discord:42", "discord", "approved"}}
	if len(*recs) != 3 {
		t.Fatalf("hook calls: %+v", *recs)
	}
	for i, w := range want {
		if (*recs)[i] != w {
			t.Fatalf("hook %d = %+v, want %+v", i, (*recs)[i], w)
		}
	}
	if dec, _ := aq.GetDecision("c"); dec.DecidedBy != "discord:42" || dec.DecidedVia != "discord" {
		t.Fatalf("stored decision: %+v", dec)
	}
}

func TestDeciderDefaultDurationFollowsTokenTTL(t *testing.T) {
	db, aq, d, _ := approvalsFixture(t)
	addPending(t, db, "a")
	if err := d.Approve(context.Background(), "a", 0, "admin:alice", ViaWeb); err != nil {
		t.Fatal(err)
	}
	a, _ := aq.GetByID("a")
	if a.ExpiresAt == nil {
		t.Fatal("no expiry")
	}
	// token approval_ttl_minutes = 90
	var mins float64
	_ = db.QueryRow(`SELECT (strftime('%s', expires_at) - strftime('%s','now')) / 60.0 FROM approvals WHERE id='a'`).Scan(&mins)
	if mins < 88 || mins > 91 {
		t.Fatalf("expiry in %.1f min, want ~90", mins)
	}
}

func TestDeciderLostRaceReportsExistingDecider(t *testing.T) {
	db, _, d, recs := approvalsFixture(t)
	addPending(t, db, "a")
	ctx := context.Background()
	if err := d.Approve(ctx, "a", 0, "admin:alice", ViaWeb); err != nil {
		t.Fatal(err)
	}
	err := d.Reject(ctx, "a", "discord:9", ViaDiscord)
	var ad *AlreadyDecidedError
	if !errors.As(err, &ad) || ad.Decision.Status != "approved" || ad.Decision.DecidedBy != "admin:alice" || ad.Decision.DecidedVia != "web" {
		t.Fatalf("err = %#v", err)
	}
	if !errors.Is(err, queries.ErrApprovalNotPending) {
		t.Fatal("must unwrap to ErrApprovalNotPending")
	}
	if decisionFailure(err) != "Already decided (approved) by admin:alice" {
		t.Fatalf("chat wording: %q", decisionFailure(err))
	}
	if len(*recs) != 1 {
		t.Fatalf("a lost race must not notify observers: %+v", *recs)
	}
	if err := d.Approve(ctx, "missing", 0, "x", ViaWeb); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestDeciderConcurrentDecisionsHaveOneWinner(t *testing.T) {
	db, aq, d, recs := approvalsFixture(t)
	addPending(t, db, "a")
	var wins, losses atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				err = d.Approve(context.Background(), "a", 0, "admin:u", ViaWeb)
			} else {
				err = d.Reject(context.Background(), "a", "approver:r", ViaTUI)
			}
			var ad *AlreadyDecidedError
			switch {
			case err == nil:
				wins.Add(1)
			case errors.As(err, &ad) && ad.Decision.DecidedBy != "":
				losses.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != 7 || len(*recs) != 1 {
		t.Fatalf("wins=%d losses=%d hooks=%d", wins.Load(), losses.Load(), len(*recs))
	}
	if dec, _ := aq.GetDecision("a"); dec.DecidedBy == "" {
		t.Fatalf("winner not recorded: %+v", dec)
	}
}

func TestDeciderExpireAttributesSweeperAndNotifies(t *testing.T) {
	db, aq, d, recs := approvalsFixture(t)
	addPending(t, db, "old")
	addPending(t, db, "new")
	_, _ = db.Exec(`UPDATE approvals SET created_at = datetime('now','-3 hours') WHERE id='old'`)
	n, err := d.Expire(context.Background(), 60)
	if err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if len(*recs) != 1 || (*recs)[0] != (decisionRecord{"old", "system:sweeper", "sweeper", "ignored"}) {
		t.Fatalf("hook: %+v", *recs)
	}
	if dec, _ := aq.GetDecision("old"); dec.DecidedBy != "system:sweeper" || dec.DecidedVia != "sweeper" || dec.Status != "ignored" {
		t.Fatalf("decision: %+v", dec)
	}
	if dec, _ := aq.GetDecision("new"); dec.Status != "pending" {
		t.Fatalf("fresh approval touched: %+v", dec)
	}
}
