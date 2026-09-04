package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklion/store"
)

type Service struct{ state *store.SQLite }

func New(state *store.SQLite) *Service { return &Service{state: state} }

type Outcome struct {
	Decision       model.YieldDecision `json:"decision,omitempty"`
	SessionID      model.SessionID     `json:"session_id,omitempty"`
	OwnershipEpoch uint64              `json:"ownership_epoch,omitempty"`
	TaskState      model.TaskState     `json:"task_state,omitempty"`
	Writer         *model.Owner        `json:"writer,omitempty"`
	Error          *protocol.Error     `json:"error,omitempty"`
}

func (s *Service) CreateSession(ctx context.Context, principal, requestID string, session model.Session) (Outcome, bool, error) {
	if err := session.Validate(); err != nil {
		return Outcome{}, false, err
	}
	if session.Kind == model.KindAgent && principal != principalFor(*session.Writer) {
		return Outcome{}, false, fmt.Errorf("initial writer does not match authenticated principal")
	}
	if session.Kind == model.KindShell && !strings.HasPrefix(principal, string(model.OwnerTerminal)+":") {
		return Outcome{}, false, fmt.Errorf("only a terminal principal may create a shell session")
	}
	payload, _ := json.Marshal(session)
	key := store.MutationKey{Principal: principal, RequestID: requestID, Operation: "create_session", SessionID: session.ID}
	key.Fingerprint = store.Fingerprint(key.Operation, key.SessionID, payload)
	result, err := s.state.RunMutation(ctx, key, func(tx *sql.Tx) (json.RawMessage, error) {
		if err := s.state.InsertSessionTx(ctx, tx, session); err != nil {
			return nil, err
		}
		if err := s.audit(ctx, tx, principal, "create_session", "ok", session); err != nil {
			return nil, err
		}
		return json.Marshal(Outcome{SessionID: session.ID, OwnershipEpoch: session.OwnershipEpoch})
	})
	if err != nil {
		return Outcome{}, false, err
	}
	var outcome Outcome
	if err := json.Unmarshal(result.JSON, &outcome); err != nil {
		return Outcome{}, false, err
	}
	return outcome, result.Replayed, nil
}

func (s *Service) RequestYield(ctx context.Context, principal, requestID string, sessionID model.SessionID, requester model.Owner, wait bool, expectedEpoch, expectedGeneration uint64) (Outcome, bool, error) {
	return s.RequestYieldWithHook(ctx, principal, requestID, sessionID, requester, wait, expectedEpoch, expectedGeneration, nil)
}

// RequestYieldWithHook runs before an immediate ownership transfer is made
// durable. Daemon callers use it as the supervisor fencing barrier; returning
// an error rolls the SQLite mutation back.
func (s *Service) RequestYieldWithHook(ctx context.Context, principal, requestID string, sessionID model.SessionID, requester model.Owner, wait bool, expectedEpoch, expectedGeneration uint64, beforeCommit func(model.Session) error) (Outcome, bool, error) {
	if principal != principalFor(requester) {
		return Outcome{}, false, fmt.Errorf("yield requester does not match authenticated principal")
	}
	payload, _ := json.Marshal(struct {
		Requester         model.Owner `json:"requester"`
		Wait              bool        `json:"wait"`
		OwnershipEpoch    uint64      `json:"ownership_epoch"`
		RuntimeGeneration uint64      `json:"runtime_generation"`
	}{requester, wait, expectedEpoch, expectedGeneration})
	key := store.MutationKey{Principal: principal, RequestID: requestID, Operation: "request_yield", SessionID: sessionID}
	key.Fingerprint = store.Fingerprint(key.Operation, key.SessionID, payload)
	result, err := s.state.RunMutation(ctx, key, func(tx *sql.Tx) (json.RawMessage, error) {
		session, err := s.state.GetSessionTx(ctx, tx, sessionID)
		if err != nil {
			return nil, err
		}
		if session.RuntimeGeneration != expectedGeneration {
			if err := s.audit(ctx, tx, principal, "request_yield", string(protocol.ErrStaleGeneration), session); err != nil {
				return nil, err
			}
			return json.Marshal(rejection(protocol.ErrStaleGeneration, "runtime generation changed", session))
		}
		if session.OwnershipEpoch != expectedEpoch {
			if err := s.audit(ctx, tx, principal, "request_yield", string(protocol.ErrStaleEpoch), session); err != nil {
				return nil, err
			}
			return json.Marshal(rejection(protocol.ErrStaleEpoch, "ownership epoch changed", session))
		}
		pending, err := s.state.GetPendingYieldTx(ctx, tx, sessionID)
		if err != nil {
			return nil, err
		}
		oldEpoch, oldGeneration := session.OwnershipEpoch, session.RuntimeGeneration
		decision, newPending, transitionErr := session.RequestYield(requester, wait, pending)
		if transitionErr != nil {
			if err := s.audit(ctx, tx, principal, "request_yield", string(mapError(transitionErr)), session); err != nil {
				return nil, err
			}
			return json.Marshal(rejection(mapError(transitionErr), transitionErr.Error(), session))
		}
		now := time.Now().UTC().UnixMilli()
		if newPending != nil {
			newPending.RequestID = requestID
			newPending.CreatedAtMS = now
			if err := s.state.InsertPendingYieldTx(ctx, tx, *newPending); err != nil {
				return nil, err
			}
		} else if decision == model.YieldTransferred {
			session.UpdatedAtMS = now
			if beforeCommit != nil {
				if err := beforeCommit(session); err != nil {
					return nil, err
				}
			}
			if err := s.state.UpdateSessionTx(ctx, tx, session, oldEpoch, oldGeneration); err != nil {
				return nil, err
			}
		}
		if err := s.audit(ctx, tx, principal, "request_yield", string(decision), session); err != nil {
			return nil, err
		}
		return json.Marshal(Outcome{Decision: decision, SessionID: session.ID, OwnershipEpoch: session.OwnershipEpoch, Writer: session.Writer})
	})
	if err != nil {
		return Outcome{}, false, err
	}
	var outcome Outcome
	if err := json.Unmarshal(result.JSON, &outcome); err != nil {
		return Outcome{}, false, err
	}
	return outcome, result.Replayed, nil
}

func principalFor(owner model.Owner) string { return string(owner.Kind) + ":" + owner.ID }

func (s *Service) BeginTask(ctx context.Context, principal, requestID string, sessionID model.SessionID, owner model.Owner, expectedEpoch, expectedGeneration uint64) (Outcome, bool, error) {
	if principal != principalFor(owner) {
		return Outcome{}, false, fmt.Errorf("task owner does not match authenticated principal")
	}
	payload, _ := json.Marshal(struct {
		Owner      model.Owner `json:"owner"`
		Epoch      uint64      `json:"epoch"`
		Generation uint64      `json:"generation"`
	}{owner, expectedEpoch, expectedGeneration})
	return s.runTaskMutation(ctx, principal, requestID, "begin_task", sessionID, payload, func(session *model.Session, pending *model.PendingYield) *protocol.Error {
		if err := session.AuthorizeAgentInput(owner, expectedEpoch, expectedGeneration); err != nil {
			return &protocol.Error{Code: mapError(err), Message: err.Error()}
		}
		if pending != nil {
			return &protocol.Error{Code: protocol.ErrDraining, Message: "a waiting yield prevents new task admission"}
		}
		if session.TaskState != model.TaskIdle {
			return &protocol.Error{Code: protocol.ErrTaskActive, Message: "another task is active"}
		}
		session.TaskState = model.TaskRunning
		return nil
	})
}

func (s *Service) BeginReply(ctx context.Context, principal, requestID string, sessionID model.SessionID, expectedGeneration uint64) (Outcome, bool, error) {
	return s.runtimeTaskMutation(ctx, principal, requestID, "begin_reply", sessionID, expectedGeneration, func(session *model.Session, _ *model.PendingYield) *protocol.Error {
		if session.TaskState != model.TaskRunning {
			return &protocol.Error{Code: protocol.ErrTaskNotActive, Message: "task is not running"}
		}
		session.TaskState = model.TaskReplying
		return nil
	})
}

func (s *Service) CompleteReply(ctx context.Context, principal, requestID string, sessionID model.SessionID, expectedGeneration uint64) (Outcome, bool, error) {
	return s.CompleteReplyWithHook(ctx, principal, requestID, sessionID, expectedGeneration, nil)
}

func (s *Service) CompleteReplyWithHook(ctx context.Context, principal, requestID string, sessionID model.SessionID, expectedGeneration uint64, beforeCommit func(model.Session) error) (Outcome, bool, error) {
	return s.runtimeTaskMutationWithHook(ctx, principal, requestID, "complete_reply", sessionID, expectedGeneration, func(session *model.Session, pending *model.PendingYield) *protocol.Error {
		if session.TaskState != model.TaskReplying && session.TaskState != model.TaskRunning {
			return &protocol.Error{Code: protocol.ErrTaskNotActive, Message: "task has no active completion"}
		}
		session.TaskState = model.TaskIdle
		if pending != nil {
			if _, err := session.ApplyPendingYield(*pending); err != nil && !errors.Is(err, model.ErrStaleEpoch) {
				return &protocol.Error{Code: mapError(err), Message: err.Error()}
			}
		}
		return nil
	}, beforeCommit)
}

// CompleteOwnerReplyWithHook makes caller ownership part of the idempotent
// mutation. Replays therefore return the original completion even when that
// completion transferred ownership to a waiting requester.
func (s *Service) CompleteOwnerReplyWithHook(ctx context.Context, principal, requestID string, sessionID model.SessionID, owner model.Owner, expectedEpoch, expectedGeneration uint64, beforeCommit func(model.Session) error) (Outcome, bool, error) {
	if principal != principalFor(owner) {
		return Outcome{}, false, fmt.Errorf("task owner does not match authenticated principal")
	}
	payload, _ := json.Marshal(struct {
		Owner      model.Owner `json:"owner"`
		Epoch      uint64      `json:"epoch"`
		Generation uint64      `json:"generation"`
	}{owner, expectedEpoch, expectedGeneration})
	return s.runTaskMutationWithHook(ctx, principal, requestID, "complete_reply", sessionID, payload, func(session *model.Session, pending *model.PendingYield) *protocol.Error {
		if err := session.AuthorizeAgentInput(owner, expectedEpoch, expectedGeneration); err != nil {
			return &protocol.Error{Code: mapError(err), Message: err.Error()}
		}
		if session.TaskState != model.TaskReplying && session.TaskState != model.TaskRunning {
			return &protocol.Error{Code: protocol.ErrTaskNotActive, Message: "task has no active completion"}
		}
		session.TaskState = model.TaskIdle
		if pending != nil {
			if _, err := session.ApplyPendingYield(*pending); err != nil && !errors.Is(err, model.ErrStaleEpoch) {
				return &protocol.Error{Code: mapError(err), Message: err.Error()}
			}
		}
		return nil
	}, beforeCommit)
}

type taskTransition func(*model.Session, *model.PendingYield) *protocol.Error

func (s *Service) runtimeTaskMutation(ctx context.Context, principal, requestID, operation string, sessionID model.SessionID, generation uint64, transition taskTransition) (Outcome, bool, error) {
	return s.runtimeTaskMutationWithHook(ctx, principal, requestID, operation, sessionID, generation, transition, nil)
}

func (s *Service) runtimeTaskMutationWithHook(ctx context.Context, principal, requestID, operation string, sessionID model.SessionID, generation uint64, transition taskTransition, beforeCommit func(model.Session) error) (Outcome, bool, error) {
	payload, _ := json.Marshal(struct {
		Generation uint64 `json:"generation"`
	}{generation})
	return s.runTaskMutationWithHook(ctx, principal, requestID, operation, sessionID, payload, func(session *model.Session, pending *model.PendingYield) *protocol.Error {
		if session.RuntimeGeneration != generation {
			return &protocol.Error{Code: protocol.ErrStaleGeneration, Message: "runtime generation changed"}
		}
		return transition(session, pending)
	}, beforeCommit)
}

func (s *Service) runTaskMutation(ctx context.Context, principal, requestID, operation string, sessionID model.SessionID, payload []byte, transition taskTransition) (Outcome, bool, error) {
	return s.runTaskMutationWithHook(ctx, principal, requestID, operation, sessionID, payload, transition, nil)
}

func (s *Service) runTaskMutationWithHook(ctx context.Context, principal, requestID, operation string, sessionID model.SessionID, payload []byte, transition taskTransition, beforeCommit func(model.Session) error) (Outcome, bool, error) {
	key := store.MutationKey{Principal: principal, RequestID: requestID, Operation: operation, SessionID: sessionID}
	key.Fingerprint = store.Fingerprint(operation, sessionID, payload)
	result, err := s.state.RunMutation(ctx, key, func(tx *sql.Tx) (json.RawMessage, error) {
		session, err := s.state.GetSessionTx(ctx, tx, sessionID)
		if err != nil {
			return nil, err
		}
		pending, err := s.state.GetPendingYieldTx(ctx, tx, sessionID)
		if err != nil {
			return nil, err
		}
		oldEpoch, oldGeneration := session.OwnershipEpoch, session.RuntimeGeneration
		if protocolError := transition(&session, pending); protocolError != nil {
			epoch, generation := session.OwnershipEpoch, session.RuntimeGeneration
			protocolError.OwnershipEpoch, protocolError.RuntimeGeneration = &epoch, &generation
			if err := s.audit(ctx, tx, principal, operation, string(protocolError.Code), session); err != nil {
				return nil, err
			}
			return json.Marshal(Outcome{SessionID: session.ID, OwnershipEpoch: epoch, TaskState: session.TaskState, Writer: session.Writer, Error: protocolError})
		}
		session.UpdatedAtMS = time.Now().UTC().UnixMilli()
		if beforeCommit != nil && session.OwnershipEpoch != oldEpoch {
			if err := beforeCommit(session); err != nil {
				return nil, err
			}
		}
		if err := s.state.UpdateSessionTx(ctx, tx, session, oldEpoch, oldGeneration); err != nil {
			return nil, err
		}
		if pending != nil && session.TaskState == model.TaskIdle && (session.OwnershipEpoch != oldEpoch || pending.SourceEpoch != session.OwnershipEpoch) {
			if err := s.state.DeletePendingYieldTx(ctx, tx, sessionID); err != nil {
				return nil, err
			}
		}
		if err := s.audit(ctx, tx, principal, operation, "ok", session); err != nil {
			return nil, err
		}
		return json.Marshal(Outcome{SessionID: session.ID, OwnershipEpoch: session.OwnershipEpoch, TaskState: session.TaskState, Writer: session.Writer})
	})
	if err != nil {
		return Outcome{}, false, err
	}
	var outcome Outcome
	if err := json.Unmarshal(result.JSON, &outcome); err != nil {
		return Outcome{}, false, err
	}
	return outcome, result.Replayed, nil
}

func (s *Service) audit(ctx context.Context, tx *sql.Tx, principal, eventType, outcome string, session model.Session) error {
	return s.state.InsertAuditTx(ctx, tx, store.AuditEvent{SessionID: session.ID, Principal: principal, EventType: eventType,
		OwnershipEpoch: session.OwnershipEpoch, RuntimeGeneration: session.RuntimeGeneration, OutcomeCode: outcome, CreatedAtMS: time.Now().UTC().UnixMilli()})
}

func rejection(code protocol.ErrorCode, message string, session model.Session) Outcome {
	epoch, generation := session.OwnershipEpoch, session.RuntimeGeneration
	return Outcome{SessionID: session.ID, OwnershipEpoch: epoch, Error: &protocol.Error{Code: code, Message: message, OwnershipEpoch: &epoch, RuntimeGeneration: &generation}}
}

func mapError(err error) protocol.ErrorCode {
	switch {
	case errors.Is(err, model.ErrNotOwner):
		return protocol.ErrNotOwner
	case errors.Is(err, model.ErrStaleEpoch):
		return protocol.ErrStaleEpoch
	case errors.Is(err, model.ErrStaleGeneration):
		return protocol.ErrStaleGeneration
	case errors.Is(err, model.ErrTaskActive):
		return protocol.ErrTaskActive
	case errors.Is(err, model.ErrPendingYield):
		return protocol.ErrPendingYield
	case errors.Is(err, model.ErrAdapterNotHealthy):
		return protocol.ErrAdapterUnhealthy
	default:
		return protocol.ErrInvalidArgument
	}
}
