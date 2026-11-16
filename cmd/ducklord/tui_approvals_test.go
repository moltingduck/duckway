package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func sampleApprovals() []ducklord.ApproverApproval {
	exp := "2026-11-16T12:00:00Z"
	return []ducklord.ApproverApproval{
		{ID: "approval-aaaa", Status: "pending", ClientName: "agent-1", Service: "github", Method: "POST", Path: "/repos/x/y/issues",
			Query: "token=REDACTED", BodyHash: "abc123", Rule: "writes", HeldReason: "An approve rule matched this request: writes",
			CreatedAt: "2026-11-16T11:55:00Z", ExpiresAt: &exp, DefaultMinutes: 30},
		{ID: "approval-bbbb", Status: "pending", ClientName: "agent-2", Service: "slack", Method: "DELETE", Path: "/chat.delete",
			HeldReason: "The phantom token requires approval before requests are forwarded.", CreatedAt: "2026-11-16T10:00:00Z", DefaultMinutes: 30},
	}
}

var approvalsTestNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func advanceApprovals(d time.Duration) { approvalsTestNow = approvalsTestNow.Add(d) }

func setApprovalsTestClock() {
	approvalsTestNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	approvalsClock = func() time.Time { return approvalsTestNow }
}

func approvalsTestState() *tuiState {
	setApprovalsTestClock()
	s := &tuiState{cfg: &ducklord.Config{}}
	s.approvals = approvalsState{open: true, loggedIn: true, server: "https://duck.example.com", conn: "live", items: sampleApprovals()}
	s.approvals.pendingCount, s.approvals.countKnown = 2, true
	return s
}

func linesText(lines []modalRenderLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.text
	}
	return strings.Join(parts, "\n")
}

func TestApprovalsPanelWideShowsListAndDetailSideBySide(t *testing.T) {
	s := approvalsTestState()
	text := linesText(s.approvalsLines(130, 30))
	var row string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "› pending") {
			row = l
		}
	}
	if row == "" || !strings.Contains(row, "│") || !strings.Contains(row, "Status:  pending") {
		t.Fatalf("wide layout should put the selected row and detail on one line:\n%s", text)
	}
	if !strings.Contains(text, "2 pending") || !strings.Contains(text, "live updates: live") {
		t.Fatalf("status line missing:\n%s", text)
	}
}

func TestApprovalsPanelNarrowStacksDetailBelowList(t *testing.T) {
	s := approvalsTestState()
	lines := s.approvalsLines(80, 30)
	listRow, detailRow := -1, -1
	for i, l := range lines {
		if strings.Contains(l.text, "› pending") {
			listRow = i
		}
		if strings.Contains(l.text, "Request: POST /repos/x/y/issues") {
			detailRow = i
		}
		if strings.Contains(l.text, "│") {
			t.Fatalf("narrow layout must not use columns: %q", l.text)
		}
	}
	if listRow < 0 || detailRow <= listRow {
		t.Fatalf("detail must be below the list (list %d, detail %d):\n%s", listRow, detailRow, linesText(lines))
	}
	var out strings.Builder
	s.renderApprovalsModal(&out, 80, 30)
	plain := frameStylePattern.ReplaceAllString(out.String(), "")
	if !strings.Contains(plain, "Request: POST /repos/x/y/issues") || !strings.Contains(plain, "Esc") {
		t.Fatalf("80-column render dropped content:\n%s", plain)
	}
}

func TestApprovalsPanelReadableWithoutColor(t *testing.T) {
	for _, env := range []map[string]string{{"NO_COLOR": "1"}, {"TERM": "dumb"}} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		s := approvalsTestState()
		s.approvals.items[1].Status = "approved"
		var out strings.Builder
		s.renderApprovalsModal(&out, 100, 30)
		plain := frameStylePattern.ReplaceAllString(out.String(), "")
		for _, want := range []string{"› pending", "approved", "Approvals", "y approve", "n reject"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%v: %q missing without colour:\n%s", env, want, plain)
			}
		}
	}
}

func TestApprovalsPanelNotLoggedInExplainsLogin(t *testing.T) {
	s := approvalsTestState()
	s.approvals.loggedIn = false
	text := linesText(s.approvalsLines(80, 24))
	if !strings.Contains(text, "ducklord approver login --server") || !strings.Contains(text, "Not logged in") {
		t.Fatalf("login guidance missing:\n%s", text)
	}
	if cmd := s.handleApprovalsInput([]byte("r")); cmd.kind != "reload" {
		t.Fatalf("r = %+v", cmd)
	}
	if cmd := s.handleApprovalsInput([]byte("y")); cmd.kind != "" {
		t.Fatalf("y while logged out = %+v", cmd)
	}
	if cmd := s.handleApprovalsInput([]byte("\x1b")); cmd.kind != "close" {
		t.Fatalf("esc = %+v", cmd)
	}
	if s.approvalsHeaderLabel() != "" {
		t.Fatal("logged-out header label must be empty")
	}
}

func TestApprovalsDetailAndHistoryViews(t *testing.T) {
	s := approvalsTestState()
	if text := linesText(s.approvalsLines(130, 30)); strings.Contains(text, "sha256") {
		t.Fatal("summary view must not show the full request")
	}
	s.handleApprovalsInput([]byte("\r"))
	text := linesText(s.approvalsLines(130, 30))
	for _, want := range []string{"Query:   token=REDACTED (redacted)", "Body:    sha256 abc123", "Rule:    writes", "never the body"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail missing %q:\n%s", want, text)
		}
	}
	s.handleApprovalsInput([]byte("\x1b"))
	if s.approvals.detail || !s.approvals.open {
		t.Fatal("esc should leave the detail view first")
	}
	if cmd := s.handleApprovalsInput([]byte("h")); cmd.kind != "refresh" || !s.approvals.showHistory {
		t.Fatalf("h = %+v", cmd)
	}
	s.approvals.setHistory([]ducklord.ApproverApproval{{ID: "h1", Status: "approved", ClientName: "c", Service: "s", Method: "GET", Path: "/p", DecidedBy: "approver:me", DecidedVia: "tui", CreatedAt: "2026-11-16T11:00:00Z"}})
	text = linesText(s.approvalsLines(130, 30))
	if !strings.Contains(text, "history") || !strings.Contains(text, "Decided: by approver:me via tui") {
		t.Fatalf("history view:\n%s", text)
	}
	if cmd := s.handleApprovalsInput([]byte("y")); cmd.kind != "" || !strings.Contains(s.approvals.message, "read-only") {
		t.Fatalf("history decision = %+v %q", cmd, s.approvals.message)
	}
	s.handleApprovalsInput([]byte("\x1b"))
	if s.approvals.showHistory || !s.approvals.open {
		t.Fatal("esc leaves history")
	}
	if cmd := s.handleApprovalsInput([]byte("\x1b")); cmd.kind != "close" {
		t.Fatalf("final esc = %+v", cmd)
	}
}

func TestApprovalsKeyHandling(t *testing.T) {
	s := approvalsTestState()
	a := &s.approvals
	s.handleApprovalsInput([]byte("\x1b[B"))
	if a.selected != 1 {
		t.Fatalf("down: selected %d", a.selected)
	}
	s.handleApprovalsInput([]byte("\x1b[B"))
	if a.selected != 1 {
		t.Fatal("down clamps at the end")
	}
	s.handleApprovalsInput([]byte("\x1b[A"))
	if cmd := s.handleApprovalsInput([]byte("y")); cmd != (approvalsCmd{kind: "approve", id: "approval-aaaa"}) {
		t.Fatalf("y = %+v", cmd)
	}
	if cmd := s.handleApprovalsInput([]byte("n")); cmd.kind != "" {
		t.Fatalf("second decision while one is in flight = %+v", cmd)
	}
	a.busy = ""
	if cmd := s.handleApprovalsInput([]byte("n")); cmd != (approvalsCmd{kind: "reject", id: "approval-aaaa"}) {
		t.Fatalf("n = %+v", cmd)
	}
	a.busy = ""
	// Y opens the picker; digits and enter pick a duration.
	s.handleApprovalsInput([]byte("Y"))
	if !a.picker || a.pickerIndex != 1 {
		t.Fatalf("picker = %v/%d", a.picker, a.pickerIndex)
	}
	if text := linesText(s.approvalsLines(100, 30)); !strings.Contains(text, "Approve for how long?") || !strings.Contains(text, "› 2  1 hour") {
		t.Fatalf("picker not rendered:\n%s", text)
	}
	s.handleApprovalsInput([]byte("\x1b[B"))
	if cmd := s.handleApprovalsInput([]byte("\r")); cmd != (approvalsCmd{kind: "approve", id: "approval-aaaa", minutes: 240}) {
		t.Fatalf("picker enter = %+v", cmd)
	}
	a.busy = ""
	s.handleApprovalsInput([]byte("Y"))
	if cmd := s.handleApprovalsInput([]byte("4")); cmd.minutes != 1440 {
		t.Fatalf("picker 4 = %+v", cmd)
	}
	a.busy = ""
	s.handleApprovalsInput([]byte("Y"))
	s.handleApprovalsInput([]byte("\x1b"))
	if a.picker || !a.open {
		t.Fatal("esc cancels the picker only")
	}
	// Mouse reports are swallowed.
	if cmd := s.handleApprovalsInput([]byte("\x1b[<0;5;5M")); cmd.kind != "" {
		t.Fatalf("mouse = %+v", cmd)
	}
}

func TestApprovalsSetItemsKeepsSelectionAndCount(t *testing.T) {
	s := approvalsTestState()
	s.approvals.selected = 1
	s.approvals.setItems([]ducklord.ApproverApproval{sampleApprovals()[1]})
	if s.approvals.selected != 0 || s.approvals.pendingCount != 1 {
		t.Fatalf("selected=%d count=%d", s.approvals.selected, s.approvals.pendingCount)
	}
	s.approvals.setItems(nil)
	if s.approvals.pendingCount != 0 || !strings.Contains(linesText(s.approvalsLines(100, 24)), "No pending approvals.") {
		t.Fatal("empty list")
	}
	if got := s.approvalsHeaderLabel(); got != "  approvals: 0 pending" {
		t.Fatalf("header = %q", got)
	}
	s.approvals.conn = "reconnecting"
	if got := s.approvalsHeaderLabel(); !strings.Contains(got, "offline") {
		t.Fatalf("header = %q", got)
	}
}

func TestApprovalsInputStageOrder(t *testing.T) {
	index := func(name string) int {
		for i, stage := range inputStages {
			if strings.HasSuffix(runtime.FuncForPC(reflect.ValueOf(stage).Pointer()).Name(), "."+name) {
				return i
			}
		}
		t.Fatalf("stage %s not found", name)
		return -1
	}
	approvals := index("inputApprovals")
	for _, earlier := range []string{"inputTerminalTools", "inputCentralModalCancel", "inputCommandPalette", "inputWorkspacePaneModal", "inputHelpMode"} {
		if index(earlier) > approvals {
			t.Fatalf("%s must run before the approvals panel so that modal wins", earlier)
		}
	}
	for _, later := range []string{"inputMouse", "inputFocusedTerminal", "inputSearch", "inputHostMenu", "inputDetailed", "inputDispatchAction"} {
		if index(later) < approvals {
			t.Fatalf("%s must run after the approvals panel", later)
		}
	}
}

func TestApprovalsDefaultShortcutDoesNotCollide(t *testing.T) {
	if got := (&ducklord.Config{}).Shortcut("approvals"); got != "I" {
		t.Fatalf("approvals shortcut = %q", got)
	}
	for action, key := range ducklord.DefaultShortcuts {
		if action != "approvals" && key == "I" {
			t.Fatalf("%s already uses I", action)
		}
	}
	s := approvalsTestState()
	s.approvals.open = false
	s.workspacePreview = true
	if got := s.handleInput([]byte("I")); got != "approvals" {
		t.Fatalf("d = %q", got)
	}
	s.hostScoped = true
	if got := s.handleInput([]byte("I")); got == "approvals" {
		t.Fatal("approvals are not offered in the host-scoped TUI")
	}
	s.hostScoped = false
	if got := s.handleInput([]byte("a")); got != "add-client" {
		t.Fatalf("a must keep adding a host, got %q", got)
	}
}

// approverFixture is an httptest approver server whose pending list is mutable.
type approverFixture struct {
	srv      *httptest.Server
	mu       sync.Mutex
	items    []ducklord.ApproverApproval
	conflict bool
	calls    []string
	events   chan string
}

func newApproverFixture(t *testing.T) *approverFixture {
	t.Helper()
	f := &approverFixture{items: sampleApprovals(), events: make(chan string, 8)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dwa_fixture" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/approver/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprint(w, "event: hello\ndata: {\"v\":1}\n\n")
			fl.Flush()
			f.mu.Unlock()
			for {
				select {
				case e := <-f.events:
					fmt.Fprint(w, e)
					fl.Flush()
				case <-r.Context().Done():
					f.mu.Lock()
					return
				}
			}
		case r.URL.Path == "/approver/approvals":
			f.calls = append(f.calls, "list "+r.URL.Query().Get("status"))
			if strings.Contains(r.URL.Query().Get("status"), "approved") {
				_ = json.NewEncoder(w).Encode([]ducklord.ApproverApproval{{ID: "old", Status: "approved", ClientName: "c", Service: "s", Method: "GET", Path: "/old", DecidedBy: "approver:me", DecidedVia: "tui"}})
				return
			}
			_ = json.NewEncoder(w).Encode(f.items)
		case strings.HasSuffix(r.URL.Path, "/approve"), strings.HasSuffix(r.URL.Path, "/reject"):
			var body map[string]int
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.calls = append(f.calls, fmt.Sprintf("%s %v", r.URL.Path, body))
			if f.conflict {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"approval already decided","status":"approved","decided_by":"discord:7","decided_via":"discord"}`))
				return
			}
			f.items = f.items[1:]
			_ = json.NewEncoder(w).Encode(ducklord.ApproverDecision{ID: "x", Status: "approved"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func approvalsTestLoop(t *testing.T, f *approverFixture, login bool) *tuiLoop {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = oldStdout; _ = devnull.Close() })
	setApprovalsTestClock()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if login {
		if err := ducklord.SaveApproverCredentials(approverCredentialsFor(cfgPath), ducklord.ApproverCredentials{Server: f.srv.URL, Token: "dwa_fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := &ducklord.Config{}
	state := &tuiState{cfg: cfg, cfgPath: cfgPath}
	return &tuiLoop{ctx: ctx, stop: cancel, cfg: cfg, state: state, approvalsDone: make(chan approvalsEvent, 16)}
}

func pumpApprovals(t *testing.T, l *tuiLoop, until func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !until() {
		select {
		case ev := <-l.approvalsDone:
			l.onApprovalsEvent(ev)
		case <-deadline:
			t.Fatalf("timed out; approvals state: %+v", l.state.approvals)
		}
	}
}

func TestApprovalsLoopLiveUpdatesApproveAndConflict(t *testing.T) {
	f := newApproverFixture(t)
	l := approvalsTestLoop(t, f, true)
	l.startApprovals()
	a := &l.state.approvals
	pumpApprovals(t, l, func() bool { return a.conn == "live" && a.countKnown && len(a.items) == 2 })
	if got := l.state.approvalsHeaderLabel(); got != "  approvals: 2 pending" {
		t.Fatalf("header = %q", got)
	}

	// A server event with a new count arrives while the panel is closed: the
	// header updates without a fetch; opening the panel refetches.
	f.events <- "event: approvals\ndata: {\"pending\":5,\"total\":9}\n\n"
	pumpApprovals(t, l, func() bool { return a.pendingCount == 5 })

	if exit, err := l.onInput([]byte("I")); exit || err != nil || !a.open {
		t.Fatalf("I should open the panel: exit=%v err=%v open=%v", exit, err, a.open)
	}
	pumpApprovals(t, l, func() bool { return !a.loading && a.pendingCount == 2 })

	// q closes the panel rather than quitting the TUI.
	if exit, err := l.onInput([]byte("q")); exit || err != nil || a.open {
		t.Fatalf("q with panel open: exit=%v err=%v open=%v", exit, err, a.open)
	}
	l.openApprovals()
	pumpApprovals(t, l, func() bool { return !a.loading })

	// Decisions are ignored right after the panel opens; wait out the guard.
	_, _ = l.onInput([]byte("y"))
	if len(f.calls) != 0 && strings.Contains(strings.Join(f.calls, "|"), "approve") {
		t.Fatal("y inside the guard window decided a request")
	}
	advanceApprovals(time.Second)
	// Approve for 4 hours through the key path.
	for _, k := range []string{"Y", "\x1b[B", "\r"} {
		if exit, err := l.onInput([]byte(k)); exit || err != nil {
			t.Fatalf("key %q: exit=%v err=%v", k, exit, err)
		}
	}
	pumpApprovals(t, l, func() bool { return a.busy == "" && len(a.items) == 1 && a.message != "" })
	if !strings.HasPrefix(a.message, "approved") || a.messageErr {
		t.Fatalf("message = %q", a.message)
	}
	f.mu.Lock()
	got := strings.Join(f.calls, "|")
	f.mu.Unlock()
	if !strings.Contains(got, "/approver/approvals/approval-aaaa/approve map[duration_minutes:240]") {
		t.Fatalf("calls = %s", got)
	}

	// A concurrent decision elsewhere: 409 shows who decided.
	f.mu.Lock()
	f.conflict = true
	f.mu.Unlock()
	advanceApprovals(time.Second) // the list shifted after the approval: wait out the guard
	_, _ = l.onInput([]byte("y"))
	pumpApprovals(t, l, func() bool { return a.busy == "" && strings.Contains(a.message, "already decided") })
	if a.message != "already decided by discord:7 via discord" || !a.messageErr {
		t.Fatalf("conflict message = %q", a.message)
	}

	// History loads the decided list.
	_, _ = l.onInput([]byte("h"))
	pumpApprovals(t, l, func() bool { return len(a.history) == 1 })
	if text := linesText(l.state.approvalsLines(120, 30)); !strings.Contains(text, "history") || !strings.Contains(text, "/old") {
		t.Fatalf("history view:\n%s", text)
	}
}

func TestApprovalsLoopRevokedTokenAndNotLoggedIn(t *testing.T) {
	f := newApproverFixture(t)
	l := approvalsTestLoop(t, f, false)
	l.startApprovals()
	if l.state.approvals.loggedIn {
		t.Fatal("logged in without credentials")
	}
	l.openApprovals()
	if !l.state.approvals.open || !strings.Contains(linesText(l.state.approvalsLines(80, 24)), "ducklord approver login") {
		t.Fatal("panel should open and explain how to log in")
	}
	// Log in out of band, then press r.
	if err := ducklord.SaveApproverCredentials(approverCredentialsFor(l.state.cfgPath), ducklord.ApproverCredentials{Server: f.srv.URL, Token: "dwa_wrong"}); err != nil {
		t.Fatal(err)
	}
	_, _ = l.onInput([]byte("r"))
	a := &l.state.approvals
	if !a.loggedIn {
		t.Fatal("r should pick up the new login")
	}
	pumpApprovals(t, l, func() bool { return a.conn == "revoked" })
	if !strings.Contains(a.message, "approver login") || l.state.approvalsHeaderLabel() != "  approvals: token rejected" {
		t.Fatalf("revoked state: %q / %q", a.message, l.state.approvalsHeaderLabel())
	}
}

func TestApprovalRequiredNotificationOnlyHintsNeverOpens(t *testing.T) {
	f := newApproverFixture(t)
	l := approvalsTestLoop(t, f, true)
	l.startApprovals()
	a := &l.state.approvals
	pumpApprovals(t, l, func() bool { return a.countKnown })

	// Free navigation, approvals pending: hint only, the panel stays closed.
	l.state.approvalNotifyPending = true
	l.maybeHintApprovalsFromNotification()
	if a.open || l.state.outputErr != "2 approvals pending — press I" {
		t.Fatalf("open=%v hint=%q", a.open, l.state.outputErr)
	}
	// Nothing pending: no hint.
	l.state.outputErr, a.pendingCount, l.state.approvalNotifyPending = "", 0, true
	l.maybeHintApprovalsFromNotification()
	if a.open || l.state.outputErr != "" {
		t.Fatalf("nothing pending: open=%v hint=%q", a.open, l.state.outputErr)
	}
	// Not logged in: no-op.
	l2 := approvalsTestLoop(t, f, false)
	l2.state.approvalNotifyPending = true
	l2.maybeHintApprovalsFromNotification()
	if l2.state.approvals.open || l2.state.outputErr != "" {
		t.Fatal("logged-out notification must be a no-op")
	}
}

func TestApprovalsDecisionGuardAfterOpenAndSelectionShift(t *testing.T) {
	s := approvalsTestState()
	a := &s.approvals
	a.guardKeys() // as when the panel opens
	for _, k := range []string{"y", "n", "Y"} {
		if cmd := s.handleApprovalsInput([]byte(k)); cmd.kind != "" || a.picker || a.busy != "" {
			t.Fatalf("%s inside the guard = %+v", k, cmd)
		}
	}
	advanceApprovals(approvalsDecisionGuard + time.Millisecond)
	if cmd := s.handleApprovalsInput([]byte("y")); cmd.kind != "approve" {
		t.Fatalf("y after the guard = %+v", cmd)
	}
	a.busy = ""
	advanceApprovals(time.Second)
	// The selected item disappears in a live update: guard again, and an open
	// picker for the old item is void.
	s.handleApprovalsInput([]byte("Y"))
	if !a.picker {
		t.Fatal("picker should open")
	}
	a.setItems(a.items[1:])
	if a.picker || !a.guarded() {
		t.Fatalf("selection shift: picker=%v guarded=%v", a.picker, a.guarded())
	}
	if cmd := s.handleApprovalsInput([]byte("n")); cmd.kind != "" {
		t.Fatalf("n right after the shift = %+v", cmd)
	}
	advanceApprovals(time.Second)
	if cmd := s.handleApprovalsInput([]byte("n")); cmd.kind != "reject" || cmd.id != "approval-bbbb" {
		t.Fatalf("n after the guard = %+v", cmd)
	}
	// An update that keeps the same selection does not guard.
	a.busy = ""
	advanceApprovals(time.Second)
	a.setItems(append([]ducklord.ApproverApproval{}, a.items...))
	if a.guarded() {
		t.Fatal("unchanged selection must not re-arm the guard")
	}
}

func TestApprovalsCommandPaletteEntryOpensPanel(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}}
	s.openCommandPalette()
	s.commandPaletteQuery = "Approvals"
	items := s.commandPaletteItems()
	if len(items) != 1 || items[0].kind != "approvals" {
		t.Fatalf("palette items = %+v", items)
	}
	s.handleCommandPaletteInput([]byte("\r"))
	if s.commandPaletteAction != "approvals" {
		t.Fatalf("palette action = %q", s.commandPaletteAction)
	}
}
