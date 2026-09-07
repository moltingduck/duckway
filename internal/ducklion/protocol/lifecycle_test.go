package protocol

import "testing"

func TestSessionLifecycleDTOValidation(t *testing.T) {
	for _, operation := range []SessionLifecycleOperation{SessionLifecycleEnd, SessionLifecycleDestroy, SessionLifecycleRestart} {
		for _, mode := range []SessionLifecycleMode{SessionLifecycleImmediate, SessionLifecycleWait, SessionLifecycleForce} {
			request := SessionLifecycleRequest{Operation: operation, Mode: mode}
			if err := request.Validate(); err != nil {
				t.Fatalf("valid request %+v: %v", request, err)
			}
			result := SessionLifecycleResult{SessionID: "ABC123", Operation: operation, Mode: mode, State: SessionLifecycleWaiting,
				OwnershipEpoch: 1, RuntimeGeneration: 1}
			if err := result.Validate(); err != nil {
				t.Fatalf("valid result %+v: %v", result, err)
			}
		}
	}
	invalidRequests := []SessionLifecycleRequest{
		{},
		{Operation: "restore", Mode: SessionLifecycleImmediate},
		{Operation: SessionLifecycleEnd, Mode: "later"},
	}
	for _, request := range invalidRequests {
		if err := request.Validate(); err == nil {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
	invalidResults := []SessionLifecycleResult{
		{Operation: SessionLifecycleEnd, Mode: SessionLifecycleImmediate, State: SessionLifecycleCompleted, OwnershipEpoch: 1, RuntimeGeneration: 1},
		{SessionID: "ABC123", Operation: SessionLifecycleEnd, Mode: SessionLifecycleImmediate, State: "unknown", OwnershipEpoch: 1, RuntimeGeneration: 1},
		{SessionID: "ABC123", Operation: SessionLifecycleEnd, Mode: SessionLifecycleImmediate, State: SessionLifecycleCompleted},
	}
	for _, result := range invalidResults {
		if err := result.Validate(); err == nil {
			t.Fatalf("invalid result accepted: %+v", result)
		}
	}
}
