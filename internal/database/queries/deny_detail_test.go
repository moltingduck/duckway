package queries_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/database/queries"
)

func TestDenyDetailMarshalCaps(t *testing.T) {
	refs := make([]queries.DenyRef, 0, 50)
	for i := 0; i < 50; i++ {
		refs = append(refs, queries.DenyRef{Ref: fmt.Sprintf("refs/heads/b%02d", i), Verdict: "refused", Pattern: "**", Role: "read", Reason: "no"})
	}
	s := queries.DenyDetail{Kind: "branch_rules", Layer: "placeholder", Rule: "**", Reason: "r", Refs: refs, Sees: "x"}.Marshal()
	var got queries.DenyDetail
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(got.Refs) != queries.MaxDenyDetailRefs || got.More != 30 {
		t.Fatalf("refs %d more %d", len(got.Refs), got.More)
	}
	if !strings.HasPrefix(s, `{"kind":"branch_rules",`) {
		t.Fatalf("kind must come first for the filter: %s", s)
	}

	// Hostile strings: control characters, invalid UTF-8, multi-byte cut, HTML.
	long := strings.Repeat("é", 5000)
	hostile := queries.DenyDetail{
		Kind: "branch_rules", Reason: "bad\x00\x1b[31m\nline" + long, Rule: "a b", Sees: "keep\nnewline\tbut\x07not" + long + "\xff",
		Refs: []queries.DenyRef{{Ref: "refs/heads/x\ny\x00" + long, Reason: "<script>&"}},
	}.Marshal()
	if len(hostile) > queries.MaxDenyDetailBytes {
		t.Fatalf("detail is %d bytes", len(hostile))
	}
	var h queries.DenyDetail
	if err := json.Unmarshal([]byte(hostile), &h); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{h.Reason, h.Rule, h.Refs[0].Ref} {
		if strings.ContainsAny(f, "\x00\x1b\n ") || !utf8.ValidString(f) {
			t.Errorf("unsafe field %q", f)
		}
	}
	if !strings.Contains(h.Sees, "keep\nnewline") || strings.ContainsAny(h.Sees, "\t\x07") || !utf8.ValidString(h.Sees) {
		t.Errorf("sees %q", h.Sees)
	}
	if !strings.Contains(hostile, "<script>&") {
		t.Errorf("HTML must not be escaped into 6-byte sequences: %s", hostile)
	}

	// Many large refs still fit the byte cap by dropping refs into "more".
	fat := make([]queries.DenyRef, 20)
	for i := range fat {
		fat[i] = queries.DenyRef{Ref: strings.Repeat("r", 300), Reason: strings.Repeat("q", 512), Pattern: strings.Repeat("p", 512)}
	}
	capped := queries.DenyDetail{Kind: "branch_rules", Reason: strings.Repeat("z", 600), Refs: fat, Sees: strings.Repeat("s", 3000)}.Marshal()
	if len(capped) > queries.MaxDenyDetailBytes {
		t.Fatalf("detail is %d bytes", len(capped))
	}
	var c queries.DenyDetail
	if err := json.Unmarshal([]byte(capped), &c); err != nil || len(c.Refs)+c.More != 20 {
		t.Fatalf("refs %d more %d err %v", len(c.Refs), c.More, err)
	}
}

func TestACLDeniedStoresDetailAndCountsRepeats(t *testing.T) {
	db, q := logDB(t)
	detail := queries.DenyDetail{Kind: "token_acl", Layer: "placeholder", Rule: "r", Reason: "nope", Sees: "{}"}.Marshal()
	for i := 0; i < 5; i++ {
		if err := q.LogACLDenied("c1", "", "s", "POST", "/v1/admin", detail); err != nil {
			t.Fatal(err)
		}
	}
	var stored string
	var repeat int
	if err := db.QueryRow(`SELECT deny_detail, repeat_count FROM request_log WHERE decision = 'acl_denied'`).Scan(&stored, &repeat); err != nil {
		t.Fatal(err)
	}
	if stored != detail || repeat != 5 {
		t.Fatalf("detail %q repeat %d", stored, repeat)
	}
	// A different reason on the same path is its own row.
	other := queries.DenyDetail{Kind: "branch_rules", Reason: "other"}.Marshal()
	if err := q.LogACLDenied("c1", "", "s", "POST", "/v1/admin", other); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, db, `decision = 'acl_denied'`); n != 2 {
		t.Fatalf("rows %d, want 2", n)
	}
	// Repeats do not consume the per-client row cap.
	for i := 0; i < 3*queries.MaxACLDeniedRowsPerMinute; i++ {
		_ = q.LogACLDenied("c1", "", "s", "POST", "/v1/admin", detail)
	}
	if n := countWhere(t, db, `decision = 'acl_denied' AND path = '/v1/admin'`); n != 2 {
		t.Fatalf("repeats created rows: %d", n)
	}
}

func TestACLDeniedCapStillHoldsWithDetail(t *testing.T) {
	db, q := logDB(t)
	for i := 0; i < 3*queries.MaxACLDeniedRowsPerMinute; i++ {
		d := queries.DenyDetail{Kind: "branch_rules", Reason: fmt.Sprint(i)}.Marshal()
		_ = q.LogACLDenied("c1", "", "s", "POST", "/probe", d)
	}
	n := countWhere(t, db, `decision = 'acl_denied' AND path <> '`+queries.DeniedSuppressedPath+`'`)
	if n != queries.MaxACLDeniedRowsPerMinute {
		t.Fatalf("stored %d denial rows, cap is %d", n, queries.MaxACLDeniedRowsPerMinute)
	}
	if m := countWhere(t, db, `path = '`+queries.DeniedSuppressedPath+`'`); m != 1 {
		t.Fatalf("suppressed marker rows: %d", m)
	}
}

func TestSearchFiltersByDenyKindAndReturnsDetail(t *testing.T) {
	_, q := logDB(t)
	for i, kind := range []string{"branch_rules", "token_acl", "branch_rules", "uncheckable"} {
		d := queries.DenyDetail{Kind: kind, Reason: "r"}.Marshal()
		_ = q.LogACLDenied("c1", "", "s", "POST", fmt.Sprintf("/p/%d", i), d)
	}
	_ = q.LogACLDenied("c1", "", "s", "POST", "/legacy", "")
	page, err := q.Search(queries.RequestLogFilter{DenyKind: "branch_rules", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("branch_rules rows: %d", len(page.Items))
	}
	for _, it := range page.Items {
		var d queries.DenyDetail
		if err := json.Unmarshal(it.DenyDetail, &d); err != nil || d.Kind != "branch_rules" || it.RepeatCount != 1 {
			t.Fatalf("item %+v %v", it, err)
		}
	}
	// "_" in a kind is not a wildcard.
	if page, _ = q.Search(queries.RequestLogFilter{DenyKind: "branch%rules", Limit: 10}); len(page.Items) != 0 {
		t.Fatalf("wildcard leaked: %d", len(page.Items))
	}
	all, _ := q.Search(queries.RequestLogFilter{Limit: 50})
	legacy := 0
	for _, it := range all.Items {
		if it.Path == "/legacy" {
			legacy++
			if it.DenyDetail != nil || it.RepeatCount != 1 {
				t.Errorf("legacy row: %+v", it)
			}
		}
	}
	if legacy != 1 {
		t.Fatal("legacy row missing")
	}
	if !queries.ValidDenyKind("capability") || queries.ValidDenyKind("nope") {
		t.Error("ValidDenyKind")
	}
}
