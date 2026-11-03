package handlers

import (
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func TestValidateGatewayURLRejectsShellAndURLParts(t *testing.T) {
	good := []string{"", "http://gw.example:8080", "https://gw.example/base", "http://10.0.0.5:8080"}
	for _, v := range good {
		if msg := ValidateSetting(queries.SettingGatewayURL, v); msg != "" {
			t.Errorf("%q rejected: %s", v, msg)
		}
	}
	bad := []string{
		"http://user:pw@gw.example", "http://user@gw.example", "http://gw.example/?a=b", "http://gw.example/#x",
		"http://gw.example/a b", "http://gw.example/a\tb", "http://gw.example/a\nb", "http://gw.example/\x01",
		"http://gw.example/$(id)", "http://gw.example/`id`", "http://gw.example/a'b", `http://gw.example/a"b`,
		`http://gw.example/a\b`, "http://gw.example/a;b", "http://gw.example/a|b", "http://gw.example/a&b",
		"http://gw.example/a<b", "http://gw.example/a>b", "http://gw.example/(a)", "http://gw.example/{a}", "http://gw.example/*",
	}
	for _, v := range bad {
		if msg := ValidateSetting(queries.SettingGatewayURL, v); msg == "" {
			t.Errorf("%q should be rejected", v)
		}
	}
}

func TestValidateUnknownSettingBounded(t *testing.T) {
	if ValidateSetting("other", strings.Repeat("x", MaxSettingValueBytes)) != "" {
		t.Fatal("value at the limit must be accepted")
	}
	if ValidateSetting("other", strings.Repeat("x", MaxSettingValueBytes+1)) == "" {
		t.Fatal("value over the limit must be rejected")
	}
}

func TestSetManyIsAtomic(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	q := queries.NewSettingsQueries(db)
	if err := q.SetMany(map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatal(err)
	}
	if q.Get("a") != "1" || q.Get("b") != "2" {
		t.Fatal("SetMany did not write")
	}
	// Force a failure on the second statement: the first must roll back.
	if _, err := db.Exec(`CREATE TRIGGER fail_b BEFORE UPDATE ON settings WHEN NEW.key = 'b' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Skip("trigger unsupported: ", err)
	}
	if err := q.SetMany(map[string]string{"a": "changed", "b": "changed"}); err == nil {
		t.Fatal("expected failure")
	}
	if q.Get("a") != "1" {
		t.Fatalf("a=%q, want rollback to 1", q.Get("a"))
	}
}

func TestRequestLogRetentionSettingValidation(t *testing.T) {
	// 7-3650 days, or empty (unset: keep everything).
	for _, ok := range []string{"7", "90", "3650", " 30 ", ""} {
		if msg := ValidateSetting(queries.SettingRequestLogRetentionDays, ok); msg != "" {
			t.Errorf("%q rejected: %s", ok, msg)
		}
	}
	for _, bad := range []string{"0", "1", "6", "-5", "3651", "abc", "1.5", "9999999999999999999"} {
		if ValidateSetting(queries.SettingRequestLogRetentionDays, bad) == "" {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if queries.SuggestedRequestLogRetentionDays != 90 {
		t.Fatal("the suggested retention is 90 days")
	}
}
