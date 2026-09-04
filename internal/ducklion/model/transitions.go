package model

import "errors"

var (
	ErrNotOwner          = errors.New("not owner")
	ErrStaleEpoch        = errors.New("stale ownership epoch")
	ErrStaleGeneration   = errors.New("stale runtime generation")
	ErrSessionNotRunning = errors.New("session is not running")
	ErrAdapterNotHealthy = errors.New("adapter is not healthy")
	ErrTaskActive        = errors.New("task is active")
	ErrPendingYield      = errors.New("pending yield already exists")
	ErrYieldUnsupported  = errors.New("yield is unsupported")
)

func (s Session) AuthorizeAgentInput(owner Owner, epoch, generation uint64) error {
	if s.Kind != KindAgent || s.Status != StatusRunning {
		return ErrSessionNotRunning
	}
	if generation != s.RuntimeGeneration {
		return ErrStaleGeneration
	}
	if epoch != s.OwnershipEpoch {
		return ErrStaleEpoch
	}
	if s.Writer == nil || *s.Writer != owner {
		return ErrNotOwner
	}
	return nil
}

type YieldDecision string

const (
	YieldTransferred YieldDecision = "transferred"
	YieldWaiting     YieldDecision = "waiting"
	YieldUnchanged   YieldDecision = "unchanged"
)

func (s *Session) RequestYield(requester Owner, wait bool, existing *PendingYield) (YieldDecision, *PendingYield, error) {
	if s.Kind != KindAgent {
		return "", nil, ErrYieldUnsupported
	}
	if s.Status != StatusRunning {
		return "", nil, ErrSessionNotRunning
	}
	if err := requester.Validate(); err != nil {
		return "", nil, err
	}
	if existing != nil {
		return "", nil, ErrPendingYield
	}
	if s.Writer != nil && *s.Writer == requester {
		return YieldUnchanged, nil, nil
	}
	if s.AdapterState != AdapterHealthy {
		return "", nil, ErrAdapterNotHealthy
	}
	if s.TaskState != TaskIdle {
		if !wait {
			return "", nil, ErrTaskActive
		}
		pending := &PendingYield{SessionID: s.ID, Requester: requester, SourceEpoch: s.OwnershipEpoch}
		return YieldWaiting, pending, nil
	}
	s.Writer = &requester
	s.OwnershipEpoch++
	return YieldTransferred, nil, nil
}

func (s *Session) ApplyPendingYield(pending PendingYield) (bool, error) {
	if s.Kind != KindAgent || s.Status != StatusRunning {
		return false, ErrSessionNotRunning
	}
	if pending.SessionID != s.ID || pending.SourceEpoch != s.OwnershipEpoch {
		return false, ErrStaleEpoch
	}
	if s.AdapterState != AdapterHealthy || s.TaskState != TaskIdle {
		return false, nil
	}
	s.Writer = &pending.Requester
	s.OwnershipEpoch++
	return true, nil
}
