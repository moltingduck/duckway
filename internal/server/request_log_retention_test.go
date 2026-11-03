package server

import (
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

// Pruning only happens when the admin explicitly set a retention (7-3650).
// Unset or invalid means keep everything, so an upgrade never drops history.
func TestRequestLogRetentionOnlyWhenExplicitlySet(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO clients (id, name, token_hash) VALUES ('c1','c1','h')`); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -200).Format("2006-01-02 15:04:05")
	for i := 0; i < 5; i++ {
		if _, err := db.Exec(`INSERT INTO request_log (client_id, service_name, method, path, status_code, created_at) VALUES ('c1','s','GET','/old',200,?)`, old); err != nil {
			t.Fatal(err)
		}
	}
	settings := queries.NewSettingsQueries(db)
	rl := queries.NewRequestLogQueries(db)
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&n)
		return n
	}
	for _, v := range []string{"", "abc", "3", "6", "3651"} { // unset or invalid: nothing is deleted
		if v != "" {
			if err := settings.Set(queries.SettingRequestLogRetentionDays, v); err != nil {
				t.Fatal(err)
			}
		}
		if n, _, err := pruneRequestLogIfConfigured(settings, rl); err != nil || n != 0 || count() != 5 {
			t.Fatalf("setting %q: removed=%d err=%v rows=%d, want nothing deleted", v, n, err, count())
		}
	}
	if err := settings.Set(queries.SettingRequestLogRetentionDays, "7"); err != nil {
		t.Fatal(err)
	}
	n, days, err := pruneRequestLogIfConfigured(settings, rl)
	if err != nil || n != 5 || days != 7 || count() != 0 {
		t.Fatalf("retention 7: removed=%d days=%d err=%v rows=%d", n, days, err, count())
	}
}
