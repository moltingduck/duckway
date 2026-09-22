package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestHostSkillsDualPaneRouteOwnership is intentionally a state-router contract:
// the Host settings -> Skills route must open with the repository pane owning
// input, while the agent tree remains visible and can be selected explicitly.
func TestHostSkillsDualPaneRouteOwnership(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep = "actions"
	s.hostMenuIndex = 4
	s.handleHostMenuInput([]byte("\r"))
	if s.hostMenuStep != "skills-dashboard" {
		t.Fatalf("Host settings -> Skills step=%q", s.hostMenuStep)
	}

	var out bytes.Buffer
	s.renderHostSkillsModal(&out, 120, 32)
	screen := out.String()
	for _, label := range []string{"Repository", "Agent", "Ducklord"} {
		if !strings.Contains(screen, label) {
			t.Errorf("dual-pane Skills view omits %q: %q", label, screen)
		}
	}

	// The repository owns the initial cursor. Tab and pane arrows transfer
	// ownership without changing the selected agent target.
	initialTarget := s.hostSkillsTargetIndex
	s.handleHostSkillsInput([]byte("\t"))
	s.handleHostSkillsInput([]byte("\x1b[C"))
	if s.hostSkillsTargetIndex != initialTarget {
		t.Fatalf("pane navigation changed agent selection: before=%d after=%d", initialTarget, s.hostSkillsTargetIndex)
	}
	s.handleHostSkillsInput([]byte("\x1b[D"))
	if s.hostSkillsTargetIndex != initialTarget {
		t.Fatalf("returning to repository changed agent selection: before=%d after=%d", initialTarget, s.hostSkillsTargetIndex)
	}

	// Esc and Ctrl-C must restore the Host settings route and close the whole
	// route respectively, including any pending agent expansion or async work.
	s.handleHostSkillsInput([]byte("\x1b"))
	if s.hostMenuStep != "actions" || s.hostMenuIndex != 4 {
		t.Fatalf("Esc did not restore Host settings: step=%q index=%d", s.hostMenuStep, s.hostMenuIndex)
	}
	s.beginHostSkills()
	s.hostSkillsBusy = true
	s.handleHostSkillsInput([]byte("\x03"))
	if s.hostMenuMode || s.hostSkillsBusy {
		t.Fatalf("Ctrl-C did not close/clean route: mode=%v busy=%v", s.hostMenuMode, s.hostSkillsBusy)
	}
}
