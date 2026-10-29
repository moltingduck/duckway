package client

import (
	"regexp"
	"strings"
	"testing"
)

func TestDoctorPresentationPrioritizesAndDeduplicatesRepairs(t *testing.T) {
	r := DoctorReport{Lines: []DoctorLine{
		{Name: "HTTP_PROXY", Status: "WARN", Detail: "different proxy", Missing: "launch wrapper"},
		{Name: "config", Status: "ERROR", Detail: "bad config", Missing: "repair config"},
		{Name: "HTTPS_PROXY", Status: "MISSING", Detail: "unset", Missing: "launch wrapper"},
		{Name: "Codex credential source", Status: "INFO", Detail: "file", Missing: "source guidance"},
		{Name: "SSL_CERT_FILE", Status: "OK", Detail: "matches", Missing: "CA guidance"},
		{Name: "observed requests", Status: "UNKNOWN", Detail: "no requests", Missing: "send a request"},
	}}
	got := r.FormatText()
	for _, want := range []string{"Summary: Attention needed — 1 error, 1 missing, 1 warning; 1 inconclusive; 1 OK, 1 info", "1. repair config", "2. launch wrapper", "For: HTTPS_PROXY, HTTP_PROXY", "note: source guidance", "note: CA guidance", "to verify: send a request", "Credentials (configured files; not request proof)", "Routing expectations (doctor environment; child may differ)", "Observed traffic (since proxy startup; not auth success)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "launch wrapper") != 1 {
		t.Errorf("repair was duplicated:\n%s", got)
	}
	actions := strings.Split(got, "Setup and server")[0]
	for _, guidance := range []string{"source guidance", "CA guidance", "send a request"} {
		if strings.Contains(actions, guidance) {
			t.Errorf("non-actionable guidance in repairs: %s", guidance)
		}
	}
}

func TestDoctorPresentationInconclusiveAndFutureChecks(t *testing.T) {
	r := DoctorReport{Lines: []DoctorLine{{Name: "observed requests", Status: "UNKNOWN", Detail: "no request observed"}, {Name: "new check", Status: "FUTURE", Detail: "retained"}}}
	got := r.FormatText()
	for _, want := range []string{"Summary: Checks inconclusive", "0 error, 0 missing, 0 warning; 1 inconclusive", "1 other status", "Additional checks", "[FUTURE] new check: retained"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Next steps") {
		t.Fatal("unknown check offered as repair")
	}
	if !strings.Contains((DoctorReport{Lines: []DoctorLine{{Name: "future", Status: "FUTURE"}}}).FormatText(), "Summary: Checks inconclusive") {
		t.Fatal("future status alone implies checks passed")
	}
	if !strings.Contains((DoctorReport{}).FormatText(), "No checks reported") {
		t.Fatal("empty report implies checks passed")
	}
}

func TestDoctorPresentationEscapesControlsBeforeOptionalColor(t *testing.T) {
	bad := "evil\x1b[31m\n\r\t\x7f\u009b\u202e"
	r := DoctorReport{Version: bad, ConfigDir: bad, Client: bad, ServerURL: bad, Lines: []DoctorLine{{Name: bad, Status: bad, Detail: bad, Missing: bad}, {Name: "config", Status: "ERROR", Detail: bad, Missing: bad}}}
	plain := r.FormatText()
	if strings.ContainsAny(plain, "\x1b\r\t\x7f\u009b\u202e") || strings.Contains(plain, bad) {
		t.Fatalf("unsafe controls in %q", plain)
	}
	if !strings.Contains(plain, `evil\x1b[31m\n\r\t\x7f\u009b\u202e`) {
		t.Fatalf("control data was lost: %q", plain)
	}
	colored := r.FormatTextWithColor(true)
	ansi := regexp.MustCompile("\x1b\\[[0-9;]*m")
	if colored == plain || ansi.ReplaceAllString(colored, "") != plain {
		t.Fatal("color changes report meaning")
	}
	if plain != r.FormatTextWithColor(false) {
		t.Fatal("plain renderer adds styling")
	}
}
