package model

import (
	"errors"
	"testing"
)

func TestSessionIDAndUnicodeHandle(t *testing.T) {
	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseSessionID(string(id)); err != nil || parsed != id {
		t.Fatalf("parse generated ID = %q, %v", parsed, err)
	}
	if got, err := ValidateHandle("  測試 session  "); err != nil || got != "測試 session" {
		t.Fatalf("handle = %q, %v", got, err)
	}
	for _, bad := range []string{"", "bad\nname", "bad\x00name", "IIIIII"} {
		if len(bad) == 6 {
			if _, err := ParseSessionID(bad); err == nil {
				t.Fatalf("invalid ID %q accepted", bad)
			}
			continue
		}
		if _, err := ValidateHandle(bad); err == nil {
			t.Fatalf("invalid handle %q accepted", bad)
		}
	}
}

func agentSession() Session {
	owner := Owner{Kind: OwnerCC, ID: "channel-1"}
	return Session{ID: "ABC123", Handle: "same name", Kind: KindAgent, AgentType: "codex", Status: StatusRunning,
		Writer: &owner, OwnershipEpoch: 2, RuntimeGeneration: 3, TaskState: TaskIdle, AdapterState: AdapterHealthy}
}

func TestAuthorizeAgentInputFencesOwnerEpochAndGeneration(t *testing.T) {
	s := agentSession()
	owner := *s.Writer
	if err := s.AuthorizeAgentInput(owner, 2, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeAgentInput(Owner{Kind: OwnerTerminal, ID: "laptop"}, 2, 3); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("wrong owner error = %v", err)
	}
	if err := s.AuthorizeAgentInput(owner, 1, 3); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale epoch error = %v", err)
	}
	if err := s.AuthorizeAgentInput(owner, 2, 2); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale generation error = %v", err)
	}
}

func TestWaitingYieldIsExclusiveAndTransfersOnlyWhenIdle(t *testing.T) {
	s := agentSession()
	s.TaskState = TaskRunning
	requester := Owner{Kind: OwnerTerminal, ID: "laptop"}
	decision, pending, err := s.RequestYield(requester, true, nil)
	if err != nil || decision != YieldWaiting || pending == nil {
		t.Fatalf("waiting decision = %q pending=%+v err=%v", decision, pending, err)
	}
	if _, _, err := s.RequestYield(Owner{Kind: OwnerTerminal, ID: "desktop"}, true, pending); !errors.Is(err, ErrPendingYield) {
		t.Fatalf("second waiter error = %v", err)
	}
	pending.RequestID = "request-1"
	s.TaskState = TaskIdle
	transferred, err := s.ApplyPendingYield(*pending)
	if err != nil || !transferred || s.Writer == nil || *s.Writer != requester || s.OwnershipEpoch != 3 {
		t.Fatalf("transfer=%v session=%+v err=%v", transferred, s, err)
	}
}

func TestShellSessionRejectsYield(t *testing.T) {
	s := Session{ID: "ABC123", Handle: "shell", Kind: KindShell, Status: StatusRunning, RuntimeGeneration: 1, TaskState: TaskIdle, AdapterState: AdapterUnavailable}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RequestYield(Owner{Kind: OwnerTerminal, ID: "laptop"}, false, nil); !errors.Is(err, ErrYieldUnsupported) {
		t.Fatalf("yield error = %v", err)
	}
}

func TestRecoveryVerifierIsSessionAndGenerationBound(t *testing.T) {
	token, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	verifier := RecoveryVerifier("ABC123", 4, token)
	if string(verifier) == string(token) {
		t.Fatal("verifier exposes bearer token")
	}
	if !VerifyRecoveryToken("ABC123", 4, token, verifier) {
		t.Fatal("valid recovery token rejected")
	}
	if VerifyRecoveryToken("DEF456", 4, token, verifier) || VerifyRecoveryToken("ABC123", 5, token, verifier) {
		t.Fatal("recovery token accepted for wrong runtime")
	}
}
