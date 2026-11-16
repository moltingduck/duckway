package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// approvalDurations are the choices behind Y (approve for a chosen duration).
var approvalDurations = []struct {
	label   string
	minutes int
}{{"15 minutes", 15}, {"1 hour", 60}, {"4 hours", 240}, {"24 hours", 1440}}

// approvalsClock is replaceable so rendering tests are deterministic.
var approvalsClock = time.Now

// approvalsState is the local, view-only state of the approvals panel. It does
// no I/O: the run loop (tui_loop_approvals.go) fetches and decides, and this
// state only records the results. The approver credentials are held only by the
// client inside it and never leave this process.
type approvalsState struct {
	open     bool
	loggedIn bool
	loadErr  string
	server   string
	client   *ducklord.ApproverClient

	pendingCount int
	countKnown   bool
	items        []ducklord.ApproverApproval // pending, newest first
	history      []ducklord.ApproverApproval // recently decided
	showHistory  bool
	selected     int
	detail       bool
	picker       bool
	pickerIndex  int
	conn         string // connecting | live | reconnecting | offline | revoked
	message      string // last action result or error
	messageErr   bool
	busy         string // approval ID being decided
	loading      bool
	generation   uint64 // fences list responses
}

// approvalsCmd is what a key press asks the run loop to do.
type approvalsCmd struct {
	kind    string // "", close, refresh, reload, approve, reject
	id      string
	minutes int
}

func (a *approvalsState) visible() []ducklord.ApproverApproval {
	if a.showHistory {
		return a.history
	}
	return a.items
}

func (a *approvalsState) current() (ducklord.ApproverApproval, bool) {
	list := a.visible()
	if a.selected < 0 || a.selected >= len(list) {
		return ducklord.ApproverApproval{}, false
	}
	return list[a.selected], true
}

func (a *approvalsState) clamp() {
	a.selected = min(max(a.selected, 0), max(0, len(a.visible())-1))
}

// setItems replaces the pending list, keeping the selection on the same ID.
func (a *approvalsState) setItems(items []ducklord.ApproverApproval) {
	selectedID := ""
	if cur, ok := a.current(); ok && !a.showHistory {
		selectedID = cur.ID
	}
	a.items = items
	a.pendingCount, a.countKnown = len(items), true
	if !a.showHistory {
		a.selected = 0
		for i, it := range items {
			if it.ID == selectedID {
				a.selected = i
			}
		}
		a.clamp()
	}
}

func (a *approvalsState) setHistory(items []ducklord.ApproverApproval) {
	a.history = items
	if a.showHistory {
		a.clamp()
	}
}

// handleApprovalsInput consumes one key chunk while the panel is open. It
// reports the action for the run loop; every key is consumed.
func (s *tuiState) handleApprovalsInput(b []byte) approvalsCmd {
	a := &s.approvals
	key := string(b)
	if strings.HasPrefix(key, "\x1b[<") { // mouse reports never reach the PTY or lists behind the panel
		return approvalsCmd{}
	}
	if !a.loggedIn {
		switch key {
		case "\x1b", "q", "\x03":
			return approvalsCmd{kind: "close"}
		case "r":
			return approvalsCmd{kind: "reload"}
		}
		return approvalsCmd{}
	}
	if a.picker {
		switch key {
		case "\x1b", "q", "\x03":
			a.picker = false
		case "\x1b[A", "k":
			a.pickerIndex = max(0, a.pickerIndex-1)
		case "\x1b[B", "j":
			a.pickerIndex = min(len(approvalDurations)-1, a.pickerIndex+1)
		case "1", "2", "3", "4":
			a.pickerIndex = int(key[0] - '1')
			return a.decide("approve", approvalDurations[a.pickerIndex].minutes)
		case "\r":
			return a.decide("approve", approvalDurations[a.pickerIndex].minutes)
		}
		return approvalsCmd{}
	}
	switch key {
	case "\x1b", "q", "\x03":
		switch {
		case a.detail:
			a.detail = false
		case a.showHistory:
			a.showHistory, a.selected = false, 0
			a.clamp()
		default:
			return approvalsCmd{kind: "close"}
		}
	case "\x1b[A", "k":
		a.selected--
		a.clamp()
	case "\x1b[B", "j":
		a.selected++
		a.clamp()
	case "\r":
		if _, ok := a.current(); ok {
			a.detail = !a.detail
		}
	case "h":
		a.showHistory, a.detail, a.selected, a.message, a.messageErr = !a.showHistory, false, 0, "", false
		a.clamp()
		return approvalsCmd{kind: "refresh"}
	case "r":
		return approvalsCmd{kind: "refresh"}
	case "y":
		return a.decide("approve", 0)
	case "n":
		return a.decide("reject", 0)
	case "Y":
		if _, ok := a.decidable(); ok {
			a.picker, a.pickerIndex = true, 1
		}
	}
	return approvalsCmd{}
}

// decidable reports the selected approval if it can still be decided here.
func (a *approvalsState) decidable() (ducklord.ApproverApproval, bool) {
	it, ok := a.current()
	switch {
	case !ok:
		a.message = "nothing selected"
	case a.showHistory || it.Status != "pending":
		a.message = "history is read-only: " + it.Status
		return it, false
	case a.busy != "":
		a.message = "a decision is already in flight"
		return it, false
	}
	return it, ok && !a.showHistory && it.Status == "pending" && a.busy == ""
}

func (a *approvalsState) decide(kind string, minutes int) approvalsCmd {
	it, ok := a.decidable()
	if !ok {
		return approvalsCmd{}
	}
	a.picker, a.busy, a.message = false, it.ID, ""
	return approvalsCmd{kind: kind, id: it.ID, minutes: minutes}
}

// approvalsHeaderLabel is the status-bar fragment: state as words, no color.
func (s *tuiState) approvalsHeaderLabel() string {
	a := &s.approvals
	switch {
	case !a.loggedIn:
		return ""
	case a.conn == "revoked":
		return "  approvals: token rejected"
	case !a.countKnown:
		return "  approvals: loading"
	case a.conn == "reconnecting" || a.conn == "offline":
		return fmt.Sprintf("  approvals: %d pending (offline)", a.pendingCount)
	}
	return fmt.Sprintf("  approvals: %d pending", a.pendingCount)
}

func approvalAge(created string) string {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, created); err == nil {
			d := approvalsClock().Sub(t)
			switch {
			case d < time.Minute:
				return "now"
			case d < time.Hour:
				return fmt.Sprintf("%dm", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("%dh", int(d.Hours()))
			}
			return fmt.Sprintf("%dd", int(d.Hours()/24))
		}
	}
	return created
}

func approvalRow(it ducklord.ApproverApproval) string {
	return fmt.Sprintf("%-8s %4s  %s · %s %s %s", it.Status, approvalAge(it.CreatedAt), displayField(it.ClientName), displayField(it.Service),
		displayField(it.Method), displayField(it.Path))
}

func approvalDetailLines(it ducklord.ApproverApproval, full bool) []string {
	lines := []string{
		"Status:  " + displayField(it.Status),
		"Client:  " + displayField(it.ClientName),
		"Service: " + displayField(it.Service),
		"Request: " + displayField(it.Method) + " " + displayField(it.Path),
		"Held:    " + displayField(it.HeldReason),
	}
	if it.DecidedBy != "" {
		via := ""
		if it.DecidedVia != "" {
			via = " via " + displayField(it.DecidedVia)
		}
		lines = append(lines, "Decided: by "+displayField(it.DecidedBy)+via)
	}
	if !full {
		return append(lines, "(Enter shows the full request summary)")
	}
	if it.Query != "" {
		lines = append(lines, "Query:   "+displayField(it.Query)+" (redacted)")
	} else {
		lines = append(lines, "Query:   (none)")
	}
	if it.BodyHash != "" {
		lines = append(lines, "Body:    sha256 "+displayField(it.BodyHash), "         (approvers see the hash, never the body)")
	} else {
		lines = append(lines, "Body:    (none)")
	}
	if it.Rule != "" {
		lines = append(lines, "Rule:    "+displayField(it.Rule))
	}
	lines = append(lines, "Created: "+displayField(it.CreatedAt))
	if it.ExpiresAt != nil {
		lines = append(lines, "Expires: "+displayField(*it.ExpiresAt))
	}
	if it.DefaultMinutes > 0 {
		lines = append(lines, fmt.Sprintf("Default: approve lasts %d min (Y chooses another)", it.DefaultMinutes))
	}
	return lines
}

// approvalsWindow returns the slice bounds that keep the selection visible.
func approvalsWindow(total, selected, height int) (int, int) {
	if height >= total {
		return 0, total
	}
	start := min(max(0, selected-height/2), total-height)
	return start, start + height
}

func (s *tuiState) approvalsLines(cols, rows int) []modalRenderLine {
	a := &s.approvals
	boxInner := min(120, max(8, cols-2)) - 2
	title := "  Approvals"
	if a.showHistory {
		title += " · history (recent decided)"
	} else {
		title += " · pending"
	}
	lines := []modalRenderLine{{modalTitle, title}}
	if !a.loggedIn {
		lines = append(lines, modalRenderLine{modalStatus, "  Not logged in as an approver."},
			modalRenderLine{"", ""},
			modalRenderLine{"", "  1. In the Duckway console, create an approver token (Approvers)."},
			modalRenderLine{"", "  2. Run:  ducklord approver login --server https://your-duckway"},
			modalRenderLine{"", "  3. Press r here, or reopen this panel."})
		if a.loadErr != "" {
			lines = append(lines, modalRenderLine{"", ""}, modalRenderLine{modalDanger, "  " + a.loadErr})
		}
		return append(lines, modalRenderLine{"", ""}, modalRenderLine{modalMuted, "  r check again · Esc close"})
	}
	conn := a.conn
	if conn == "" {
		conn = "connecting"
	}
	status := fmt.Sprintf("  %s · live updates: %s", displayField(a.server), conn)
	if a.countKnown {
		status += fmt.Sprintf(" · %d pending", a.pendingCount)
	}
	if a.loading {
		status += " · loading"
	}
	lines = append(lines, modalRenderLine{modalStatus, status})
	if a.message != "" {
		style := modalStatus
		if a.messageErr {
			style = modalDanger
		}
		lines = append(lines, modalRenderLine{style, "  " + a.message})
	}

	list := a.visible()
	var left []string
	bodyH := max(4, rows-2-len(lines)-2) // minus the hint rows
	wide := cols >= 100
	listH := bodyH
	if !wide {
		listH = max(2, min(len(list), bodyH/2))
	}
	if len(list) == 0 {
		empty := "  No pending approvals."
		if a.showHistory {
			empty = "  No decided approvals yet."
		}
		left = []string{empty}
	} else {
		start, end := approvalsWindow(len(list), a.selected, listH)
		for i := start; i < end; i++ {
			prefix := "  "
			if i == a.selected {
				prefix = "› "
			}
			left = append(left, prefix+approvalRow(list[i]))
		}
	}

	var right []string
	switch {
	case a.picker:
		if it, ok := a.current(); ok {
			right = append(right, "Approve for how long?  ("+displayField(it.Method)+" "+displayField(it.Path)+")")
		}
		for i, d := range approvalDurations {
			right = append(right, choiceLine(i == a.pickerIndex, fmt.Sprintf("%d  %s", i+1, d.label)))
		}
	default:
		if it, ok := a.current(); ok {
			right = approvalDetailLines(it, a.detail)
		}
	}

	if wide {
		leftW := min(58, boxInner/2)
		rightW := boxInner - leftW - 3
		n := max(len(left), len(right))
		for i := 0; i < min(n, bodyH); i++ {
			l, r := "", ""
			if i < len(left) {
				l = left[i]
			}
			if i < len(right) {
				r = right[i]
			}
			lines = append(lines, modalRenderLine{"", modalCellPad(modalCellTruncate(modalDisplayText(l), leftW), leftW) + " │ " + modalCellTruncate(modalDisplayText(r), rightW)})
		}
	} else {
		for _, l := range left {
			lines = append(lines, modalRenderLine{"", l})
		}
		if len(right) > 0 {
			lines = append(lines, modalRenderLine{modalBorder, strings.Repeat("─", boxInner)})
			room := max(0, rows-2-len(lines)-2)
			if len(right) > room {
				right = right[:room]
			}
			for _, r := range right {
				lines = append(lines, modalRenderLine{"", "  " + r})
			}
		}
	}

	hint := "  ↑/↓ · y approve · Y duration · n reject · Enter detail · h history · Esc"
	if a.picker {
		hint = "  ↑/↓ or 1-4 choose · Enter approve · Esc cancel"
	} else if a.showHistory {
		hint = "  ↑/↓ select · Enter detail · h pending · Esc back"
	} else if a.detail {
		hint = "  y approve · Y duration · n reject · Enter summary · Esc back"
	}
	return append(lines, modalRenderLine{"", ""}, modalRenderLine{modalMuted, hint})
}

func (s *tuiState) renderApprovalsModal(out io.Writer, cols, rows int) {
	if !s.approvals.open {
		return
	}
	s.resetModalMouse()
	renderModalBoxWidth(out, cols, rows, 120, s.approvalsLines(cols, rows))
}
