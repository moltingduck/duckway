package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
	"github.com/hackerduck/duckway/internal/ducklion/service"
	"github.com/hackerduck/duckway/internal/ducklion/store"
	"github.com/hackerduck/duckway/internal/ducklion/supervisor"
	"golang.org/x/sys/unix"
)

func (s *Server) routeAgentTaskSubmit(request protocol.Request, principal string) protocol.Response {
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil || request.InstanceID != string(s.instanceID) || request.OwnershipEpoch == nil || request.RuntimeGeneration == nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session identity and fences are required"}}
	}
	var submit protocol.AgentTaskSubmit
	if decodeStrict(request.Body, &submit) != nil || request.ID != submit.TaskID || !protocol.ValidTaskID(submit.TaskID) || len(submit.Prompt) == 0 || len(submit.Prompt) > protocol.MaxAgentPromptBytes || sha256.Sum256(submit.Prompt) != submit.PromptDigest {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid agent task"}}
	}
	operation := s.sessionOperation(sessionID)
	operation.Lock()
	defer operation.Unlock()
	owner := model.Owner{Kind: model.OwnerCC, ID: principal}
	epoch, generation := *request.OwnershipEpoch, *request.RuntimeGeneration
	metadata := store.ManagedTask{SessionID: sessionID, TaskID: submit.TaskID, PromptDigest: submit.PromptDigest, Owner: owner,
		OwnershipEpoch: epoch, RuntimeGeneration: generation}
	if existing, getErr := s.service.GetManagedTask(context.Background(), sessionID, submit.TaskID); getErr == nil {
		if existing.PromptDigest != submit.PromptDigest || existing.Owner != owner || existing.OwnershipEpoch != epoch || existing.RuntimeGeneration != generation {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrIdempotencyConflict, Message: "task id was already used with different metadata"}}
		}
		switch existing.Status {
		case store.ManagedTaskFailed:
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "agent task previously failed: " + existing.ErrorCategory}}
		case store.ManagedTaskRunning, store.ManagedTaskReplying, store.ManagedTaskCompleted:
			result, _ := json.Marshal(protocol.AgentTaskState{SessionID: string(sessionID), TaskID: existing.TaskID, Status: string(existing.Status),
				OwnershipEpoch: epoch, RuntimeGeneration: generation, Writer: owner, OutputStart: existing.OutputStart})
			return protocol.Response{ID: request.ID, Result: result}
		}
	} else if !errors.Is(getErr, store.ErrNotFound) {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not inspect managed task", Retryable: true}}
	}
	if protocolError := s.service.ValidateManagedTask(context.Background(), metadata); protocolError != nil {
		return protocol.Response{ID: request.ID, Error: protocolError}
	}
	prepareBody, _ := json.Marshal(protocol.SupervisorAgentPrepare{TaskID: submit.TaskID, Prompt: submit.Prompt, PromptDigest: submit.PromptDigest, Owner: owner})
	prepare := request
	prepare.Type, prepare.Body = "supervisor.agent_prepare", prepareBody
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	prepareResponse := s.callControl(ctx, sessionID, prepare)
	cancel()
	if prepareResponse.Error != nil {
		prepareResponse.ID = request.ID
		return prepareResponse
	}
	s.outputMu.Lock()
	registered, outputReady := s.outputs[sessionID]
	s.outputMu.Unlock()
	if !outputReady || registered.identity.Generation != generation {
		s.abortPreparedAgentTask(request, submit.TaskID)
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "runtime output is unavailable", Retryable: true}}
	}
	_, outputStart := registered.hub.Bounds()
	metadata.OutputStart = outputStart
	task, _, protocolError, admitErr := s.service.AdmitManagedTask(context.Background(), "cc:"+principal, metadata)
	if admitErr != nil || protocolError != nil {
		s.abortPreparedAgentTask(request, submit.TaskID)
		if protocolError == nil {
			protocolError = &protocol.Error{Code: protocol.ErrInternal, Message: "could not admit managed task", Retryable: true}
		}
		return protocol.Response{ID: request.ID, Error: protocolError}
	}
	commitBody, _ := json.Marshal(protocol.SupervisorAgentCommit{TaskID: submit.TaskID, PromptDigest: submit.PromptDigest, Owner: owner})
	commit := request
	commit.Type, commit.Body = "supervisor.agent_commit", commitBody
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	commitResponse := s.callControl(ctx, sessionID, commit)
	cancel()
	if commitResponse.Error != nil {
		status, statusErr := s.agentTaskRuntimeStatus(request, submit.TaskID, submit.PromptDigest)
		if statusErr == nil && status == "committed" {
			commitResponse.Error = nil
		} else if statusErr == nil && status == "absent" || statusErr == nil && status == "prepared" && !commitResponse.Error.Retryable {
			s.abortPreparedAgentTask(request, submit.TaskID)
			_ = s.service.FailManagedTask(context.Background(), sessionID, submit.TaskID, string(commitResponse.Error.Code), generation)
			// The adapter cannot prove that zero bytes reached the native PTY.
			// Stop this runtime rather than advertise it as healthy for another task.
			terminate := request
			terminate.Type, terminate.Body = "supervisor.terminate", nil
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.callControl(ctx, sessionID, terminate)
			cancel()
			commitResponse.ID = request.ID
			return commitResponse
		} else {
			commitResponse.ID = request.ID
			commitResponse.Error.Retryable = true
			commitResponse.Error.Message = "agent task commit outcome is unknown; retry the same task id"
			return commitResponse
		}
	}
	task, err = s.service.MarkManagedTaskRunning(context.Background(), sessionID, submit.TaskID, generation)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "agent task was committed but its state could not be finalized", Retryable: true}}
	}
	result, _ := json.Marshal(protocol.AgentTaskState{SessionID: string(sessionID), TaskID: task.TaskID, Status: string(task.Status),
		OwnershipEpoch: epoch, RuntimeGeneration: generation, Writer: owner, OutputStart: task.OutputStart})
	return protocol.Response{ID: request.ID, Result: result}
}

func (s *Server) agentTaskRuntimeStatus(request protocol.Request, taskID string, digest [32]byte) (string, error) {
	body, _ := json.Marshal(protocol.SupervisorAgentStatus{TaskID: taskID, PromptDigest: digest})
	request.Type, request.Body = "supervisor.agent_status", body
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response := s.callControl(ctx, model.SessionID(request.SessionID), request)
	if response.Error != nil {
		return "", errors.New(response.Error.Message)
	}
	var result protocol.SupervisorAgentStatusResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return "", err
	}
	return result.Status, nil
}

func (s *Server) abortPreparedAgentTask(request protocol.Request, taskID string) {
	body, _ := json.Marshal(protocol.SupervisorAgentAbort{TaskID: taskID})
	request.Type, request.Body = "supervisor.agent_abort", body
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.callControl(ctx, model.SessionID(request.SessionID), request)
}

const runtimeSubcommand = "__ducklion_runtime_v1"

type runtimeSpec struct {
	SocketPath        string          `json:"socket_path"`
	SessionID         model.SessionID `json:"session_id"`
	RuntimeGeneration uint64          `json:"runtime_generation"`
	OwnershipEpoch    uint64          `json:"ownership_epoch"`
	CWD               string          `json:"cwd"`
	Command           []string        `json:"command"`
	Rows              uint16          `json:"rows"`
	Cols              uint16          `json:"cols"`
}

func (s *Server) routeSessionCreate(request protocol.Request, principal string) protocol.Response {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	if request.InstanceID != string(s.instanceID) || request.SessionID != "" || request.OwnershipEpoch != nil || request.RuntimeGeneration != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid session create envelope"}}
	}
	var create protocol.SessionCreate
	if err := decodeStrict(request.Body, &create); err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid session create request"}}
	}
	handle, err := model.ValidateHandle(create.Handle)
	if err != nil || (create.Kind != model.KindAgent && create.Kind != model.KindShell) || len(create.Command) == 0 || len(create.Command) > 64 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "handle, kind, and command are required"}}
	}
	if !filepath.IsAbs(create.CWD) {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "working directory must be absolute"}}
	}
	info, err := os.Stat(create.CWD)
	if err != nil || !info.IsDir() {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "working directory is not accessible"}}
	}
	for _, part := range create.Command {
		if part == "" || strings.ContainsRune(part, 0) || len(part) > 32<<10 {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "command contains an invalid argument"}}
		}
	}
	if create.Rows == 0 {
		create.Rows = 40
	}
	if create.Cols == 0 {
		create.Cols = 120
	}
	if create.Rows < 5 || create.Rows > 200 || create.Cols < 20 || create.Cols > 500 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "PTY size is outside supported bounds"}}
	}
	if create.Kind == model.KindAgent && strings.TrimSpace(create.AgentType) == "" {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "agent type is required"}}
	}
	if create.Kind == model.KindShell && create.AgentType != "" {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "shell sessions cannot have an agent type"}}
	}
	fingerprint := store.Fingerprint("create_session", "", request.Body)
	mutationKey := store.MutationKey{Principal: "terminal:" + principal, RequestID: request.ID, Operation: "create_session", Fingerprint: fingerprint}
	if replay, found, replayErr := s.state.ReplayMutation(context.Background(), mutationKey); found {
		if replayErr != nil {
			code := protocol.ErrInternal
			if errors.Is(replayErr, store.ErrIdempotencyConflict) {
				code = protocol.ErrIdempotencyConflict
			}
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: code, Message: replayErr.Error()}}
		}
		var stored struct {
			SessionID model.SessionID `json:"session_id"`
		}
		if json.Unmarshal(replay.JSON, &stored) != nil {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "invalid stored create outcome"}}
		}
		current, getErr := s.state.GetSession(context.Background(), stored.SessionID)
		if getErr != nil {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not replay created session"}}
		}
		if current.Status == model.StatusRunning || current.Status == model.StatusStopped {
			result, _ := json.Marshal(summaryFor(current))
			return protocol.Response{ID: request.ID, Result: result}
		}
		return s.waitForSessionOutcome(request.ID, stored.SessionID)
	}
	existing, err := s.state.ListSessions(context.Background())
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not inspect session capacity", Retryable: true}}
	}
	active := 0
	for _, item := range existing {
		if item.Status == model.StatusRunning || item.Status == model.StatusRecovering || item.Status == model.StatusProvisioning {
			active++
		}
	}
	if active >= 64 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrBusy, Message: "Ducklion session capacity reached"}}
	}
	id, publicKey, privateKey, err := newRuntimeIdentity(s.state, s.root)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not allocate session"}}
	}
	now := time.Now().UTC().UnixMilli()
	session := model.Session{ID: id, Handle: handle, Kind: create.Kind, AgentType: strings.TrimSpace(create.AgentType), CWD: create.CWD,
		Status: model.StatusRecovering, OwnershipEpoch: 1, RuntimeGeneration: 1, TaskState: model.TaskIdle,
		AdapterState: model.AdapterRecovering, RecoveryPublicKey: publicKey, CreatedAtMS: now, UpdatedAtMS: now}
	if create.Kind == model.KindAgent {
		session.Writer = &model.Owner{Kind: model.OwnerTerminal, ID: principal}
	} else {
		session.AdapterState = model.AdapterUnavailable
	}
	createdID, replayed, err := s.state.CreateSessionIdempotent(context.Background(), "terminal:"+principal, request.ID, fingerprint, session)
	if err != nil {
		code := protocol.ErrInternal
		if errors.Is(err, store.ErrIdempotencyConflict) {
			code = protocol.ErrIdempotencyConflict
		}
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: code, Message: err.Error()}}
	}
	if replayed {
		current, getErr := s.state.GetSession(context.Background(), createdID)
		if getErr != nil {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not replay created session"}}
		}
		if current.Status == model.StatusStopped {
			result, _ := json.Marshal(summaryFor(current))
			return protocol.Response{ID: request.ID, Result: result}
		}
		if current.Status == model.StatusRunning {
			result, _ := json.Marshal(summaryFor(current))
			return protocol.Response{ID: request.ID, Result: result}
		}
		id = createdID
	}
	if !replayed {
		keyPath, err := s.writeRuntimeFiles(session, privateKey, runtimeSpec{SocketPath: s.socketPath, SessionID: id, RuntimeGeneration: 1,
			OwnershipEpoch: 1, CWD: create.CWD, Command: create.Command, Rows: create.Rows, Cols: create.Cols})
		if err != nil {
			_ = s.state.MarkRuntimeExited(context.Background(), id, 1, false, "could not prepare session supervisor")
			_ = os.RemoveAll(filepath.Join(s.root, "sessions", string(id)))
			current, _ := s.state.GetSession(context.Background(), id)
			result, _ := json.Marshal(summaryFor(current))
			return protocol.Response{ID: request.ID, Result: result}
		}
		if err := s.runtimeLauncher(keyPath); err != nil {
			_ = s.state.MarkRuntimeExited(context.Background(), id, 1, false, "could not launch session supervisor")
			_ = os.RemoveAll(filepath.Join(s.root, "sessions", string(id)))
			current, _ := s.state.GetSession(context.Background(), id)
			result, _ := json.Marshal(summaryFor(current))
			return protocol.Response{ID: request.ID, Result: result}
		}
	}
	return s.waitForSessionOutcome(request.ID, id)
}

func (s *Server) waitForSessionOutcome(requestID string, id model.SessionID) protocol.Response {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.controlMu.Lock()
		control := s.controls[id]
		s.controlMu.Unlock()
		s.outputMu.Lock()
		output, outputReady := s.outputs[id]
		s.outputMu.Unlock()
		if control != nil && outputReady && control.identity == output.identity && s.registry.IsCurrent(output.identity) {
			current, getErr := s.state.GetSession(context.Background(), id)
			if getErr == nil {
				result, _ := json.Marshal(summaryFor(current))
				return protocol.Response{ID: requestID, Result: result}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	current, err := s.state.GetSession(context.Background(), id)
	if err != nil {
		return protocol.Response{ID: requestID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not read provisioning session", Retryable: true}}
	}
	result, _ := json.Marshal(summaryFor(current))
	return protocol.Response{ID: requestID, Result: result}
}

func (s *Server) routeSessionStop(request protocol.Request, principal string) protocol.Response {
	if len(request.Body) != 0 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session stop has no body"}}
	}
	sessionID, parseErr := model.ParseSessionID(request.SessionID)
	if parseErr == nil && request.InstanceID == string(s.instanceID) && request.OwnershipEpoch != nil && request.RuntimeGeneration != nil {
		current, getErr := s.state.GetSession(context.Background(), sessionID)
		ownerMatches := current.Kind == model.KindShell || current.Writer != nil && current.Writer.Kind == model.OwnerTerminal && current.Writer.ID == principal
		if getErr == nil && current.Status == model.StatusStopped && ownerMatches && current.OwnershipEpoch == *request.OwnershipEpoch && current.RuntimeGeneration == *request.RuntimeGeneration {
			result, _ := json.Marshal(summaryFor(current))
			return protocol.Response{ID: request.ID, Result: result}
		}
	}
	session, _, protocolError := s.authorizeTerminalControl(request, principal)
	if protocolError != nil {
		return protocol.Response{ID: request.ID, Error: protocolError}
	}
	forwarded := request
	forwarded.Type = "supervisor.terminate"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response := s.callControl(ctx, session.ID, forwarded)
	if response.Error != nil {
		return response
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := s.state.GetSession(context.Background(), session.ID)
		if err == nil && current.Status == model.StatusStopped {
			result, _ := json.Marshal(summaryFor(current))
			return protocol.Response{ID: request.ID, Result: result}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrBusy, Message: "session is still stopping", Retryable: true}}
}

func (s *Server) routeSessionYield(request protocol.Request, role protocol.PeerRole, principal string) protocol.Response {
	var body protocol.SessionYield
	if request.InstanceID != string(s.instanceID) || request.OwnershipEpoch == nil || request.RuntimeGeneration == nil || decodeStrict(request.Body, &body) != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session identity, fences, and yield body are required"}}
	}
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}}
	}
	ownerKind := model.OwnerTerminal
	if role == protocol.RoleDuckwayCC {
		ownerKind = model.OwnerCC
	}
	requester := model.Owner{Kind: ownerKind, ID: principal}
	operation := s.sessionOperation(sessionID)
	operation.Lock()
	defer operation.Unlock()
	previous, previousErr := s.state.GetSession(context.Background(), sessionID)
	fencePrepared := false
	outcome, _, err := s.service.RequestYieldWithHook(context.Background(), string(ownerKind)+":"+principal, request.ID, sessionID, requester, body.Wait,
		*request.OwnershipEpoch, *request.RuntimeGeneration, func(next model.Session) error {
			response := s.syncRuntimeOwnership(next)
			if response.Error != nil {
				return fmt.Errorf("runtime rejected ownership fence: %s", response.Error.Message)
			}
			fencePrepared = true
			return nil
		})
	if err != nil {
		if fencePrepared && previousErr == nil {
			s.restoreOwnershipOrQuarantine(previous)
		}
		code := protocol.ErrInternal
		retryable := true
		if errors.Is(err, store.ErrIdempotencyConflict) {
			code = protocol.ErrIdempotencyConflict
			retryable = false
		}
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: code, Message: "could not apply yield", Retryable: retryable}}
	}
	if outcome.Error != nil {
		return protocol.Response{ID: request.ID, Error: outcome.Error}
	}
	result, _ := json.Marshal(protocol.SessionYieldResult{Decision: outcome.Decision, SessionID: string(sessionID), OwnershipEpoch: outcome.OwnershipEpoch, Writer: outcome.Writer})
	return protocol.Response{ID: request.ID, Result: result}
}

func (s *Server) routeSessionTask(request protocol.Request, principal string) protocol.Response {
	if len(request.Body) != 0 || request.InstanceID != string(s.instanceID) || request.OwnershipEpoch == nil || request.RuntimeGeneration == nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session identity and fences are required"}}
	}
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}}
	}
	operation := s.sessionOperation(sessionID)
	operation.Lock()
	defer operation.Unlock()
	owner := model.Owner{Kind: model.OwnerCC, ID: principal}
	var outcome service.Outcome
	if request.Type == "session.task_begin" {
		outcome, _, err = s.service.BeginTask(context.Background(), "cc:"+principal, request.ID, sessionID, owner, *request.OwnershipEpoch, *request.RuntimeGeneration)
	} else {
		previous, previousErr := s.state.GetSession(context.Background(), sessionID)
		fencePrepared := false
		outcome, _, err = s.service.CompleteOwnerReplyWithHook(context.Background(), "cc:"+principal, request.ID, sessionID, owner, *request.OwnershipEpoch, *request.RuntimeGeneration, func(next model.Session) error {
			response := s.syncRuntimeOwnership(next)
			if response.Error != nil {
				return fmt.Errorf("runtime rejected ownership fence: %s", response.Error.Message)
			}
			fencePrepared = true
			return nil
		})
		if err != nil && fencePrepared && previousErr == nil {
			s.restoreOwnershipOrQuarantine(previous)
		}
	}
	if err != nil {
		code := protocol.ErrInternal
		if errors.Is(err, store.ErrIdempotencyConflict) {
			code = protocol.ErrIdempotencyConflict
		}
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: code, Message: "could not update task lifecycle", Retryable: code == protocol.ErrInternal}}
	}
	if outcome.Error != nil {
		return protocol.Response{ID: request.ID, Error: outcome.Error}
	}
	result, _ := json.Marshal(protocol.SessionTaskResult{SessionID: string(outcome.SessionID), OwnershipEpoch: outcome.OwnershipEpoch, TaskState: outcome.TaskState, Writer: outcome.Writer})
	return protocol.Response{ID: request.ID, Result: result}
}

func (s *Server) routeSessionBind(request protocol.Request, principal string) protocol.Response {
	if request.InstanceID != string(s.instanceID) || request.OwnershipEpoch != nil || request.RuntimeGeneration != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "binding requires instance and session identity only"}}
	}
	var body protocol.SessionBind
	if decodeStrict(request.Body, &body) != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "channel handle is required"}}
	}
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}}
	}
	outcome, _, err := s.service.BindDiscord(context.Background(), "cc:"+principal, request.ID, sessionID, body.ChannelHandle)
	if err != nil {
		code := protocol.ErrInternal
		if errors.Is(err, store.ErrIdempotencyConflict) {
			code = protocol.ErrIdempotencyConflict
		}
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: code, Message: "could not bind Discord channel", Retryable: code == protocol.ErrInternal}}
	}
	if outcome.Error != nil {
		return protocol.Response{ID: request.ID, Error: outcome.Error}
	}
	result, _ := json.Marshal(protocol.SessionBinding{SessionID: string(outcome.Binding.SessionID), ChannelHandle: outcome.Binding.ChannelHandle, ManagementHandle: outcome.Binding.ManagementHandle})
	return protocol.Response{ID: request.ID, Result: result}
}

func (s *Server) routeCurrentBinding(request protocol.Request, principal string) protocol.Response {
	if request.InstanceID != string(s.instanceID) || request.SessionID != "" || request.OwnershipEpoch != nil || request.RuntimeGeneration != nil || len(request.Body) != 0 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid binding lookup envelope"}}
	}
	binding, err := s.state.GetBindingByChannel(context.Background(), principal)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrNotFound, Message: "Discord channel is not bound"}}
	}
	result, _ := json.Marshal(protocol.SessionBinding{SessionID: string(binding.SessionID), ChannelHandle: binding.ChannelHandle, ManagementHandle: binding.ManagementHandle})
	return protocol.Response{ID: request.ID, Result: result}
}

func (s *Server) routeBindingBySession(request protocol.Request) protocol.Response {
	if request.InstanceID != string(s.instanceID) || request.SessionID == "" || request.OwnershipEpoch != nil || request.RuntimeGeneration != nil || len(request.Body) != 0 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid binding lookup envelope"}}
	}
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}}
	}
	binding, err := s.state.GetBindingBySession(context.Background(), sessionID)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrNotFound, Message: "Ducklion session is not bound"}}
	}
	result, _ := json.Marshal(protocol.SessionBinding{SessionID: string(binding.SessionID), ChannelHandle: binding.ChannelHandle, ManagementHandle: binding.ManagementHandle})
	return protocol.Response{ID: request.ID, Result: result}
}

func (s *Server) restoreOwnershipOrQuarantine(previous model.Session) {
	if response := s.syncRuntimeOwnership(previous); response.Error == nil {
		return
	}
	// A failed compensation leaves the supervisor fence uncertain. Make the
	// durable session unavailable so neither owner can send more input; runtime
	// recovery/restart is then the only path back to a writable session.
	if err := s.state.MarkRuntimeExited(context.Background(), previous.ID, previous.RuntimeGeneration, false, "ownership fence reconciliation failed"); err == nil {
		return
	}
	// Persistent storage may be unavailable too. Revoke the live lease and
	// close its control channel so the uncertain supervisor cannot receive any
	// more input even if durable quarantine could not be recorded.
	s.controlMu.Lock()
	peer := s.controls[previous.ID]
	if peer != nil {
		delete(s.controls, previous.ID)
	}
	s.controlMu.Unlock()
	if peer != nil {
		s.registry.Disconnect(peer.identity)
		peer.stop()
	}
}

func summaryFor(session model.Session) protocol.SessionSummary {
	return protocol.SessionSummary{SessionID: string(session.ID), Handle: session.Handle, Kind: session.Kind, AgentType: session.AgentType, CWD: session.CWD,
		Status: session.Status, Writer: session.Writer, OwnershipEpoch: session.OwnershipEpoch, RuntimeGeneration: session.RuntimeGeneration,
		TaskState: session.TaskState, AdapterState: session.AdapterState, ExitSuccess: session.ExitSuccess, ExitReason: session.ExitReason}
}

func newRuntimeIdentity(state *store.SQLite, root string) (model.SessionID, ed25519.PublicKey, ed25519.PrivateKey, error) {
	for range 32 {
		id, err := model.NewSessionID()
		if err != nil {
			return "", nil, nil, err
		}
		if _, err = state.GetSession(context.Background(), id); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", nil, nil, err
		}
		if _, err := os.Lstat(filepath.Join(root, "sessions", string(id))); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", nil, nil, err
		}
		publicKey, privateKey, err := model.NewRecoveryKey()
		return id, publicKey, privateKey, err
	}
	return "", nil, nil, fmt.Errorf("could not allocate unique session id")
}

func (s *Server) writeRuntimeFiles(session model.Session, privateKey ed25519.PrivateKey, spec runtimeSpec) (string, error) {
	dir := filepath.Join(s.root, "sessions", string(session.ID))
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	keyPath, specPath := filepath.Join(dir, "recovery.key"), filepath.Join(dir, "runtime.json")
	if err := writeExclusiveFile(keyPath, []byte(base64.RawStdEncoding.EncodeToString(privateKey))); err != nil {
		return "", err
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	if err := writeExclusiveFile(specPath, data); err != nil {
		return "", err
	}
	return specPath, nil
}

func writeExclusiveFile(path string, data []byte) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (s *Server) spawnRuntime(specPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(specPath+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, runtimeSubcommand, specPath)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	_ = logFile.Close()
	return cmd.Process.Release()
}

func RunManagedSupervisor(ctx context.Context, specPath string) error {
	data, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	var spec runtimeSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	keyData, err := os.ReadFile(filepath.Join(filepath.Dir(specPath), "recovery.key"))
	if err != nil {
		return err
	}
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(keyData)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return fmt.Errorf("invalid recovery key")
	}
	ptySession, err := supervisor.Start(supervisor.Options{SessionID: spec.SessionID, RuntimeGeneration: spec.RuntimeGeneration, OwnershipEpoch: spec.OwnershipEpoch,
		CWD: spec.CWD, Command: spec.Command, Rows: spec.Rows, Cols: spec.Cols, OutputCapacity: 1 << 20})
	if err != nil {
		return err
	}
	wait := make(chan error, 1)
	go func() { wait <- ptySession.Wait() }()
	for {
		select {
		case err := <-wait:
			return reportExitedRuntime(ctx, specPath, spec, ed25519.PrivateKey(decoded), ptySession.Output(), err)
		case <-ctx.Done():
			_ = ptySession.Terminate(false)
			select {
			case <-wait:
			case <-time.After(2 * time.Second):
				_ = ptySession.Terminate(true)
				select {
				case <-wait:
				case <-time.After(2 * time.Second):
				}
			}
			return ctx.Err()
		default:
		}
		client, err := RegisterSupervisor(spec.SocketPath, spec.SessionID, spec.RuntimeGeneration, ed25519.PrivateKey(decoded))
		if err != nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		connectionCtx, cancel := context.WithCancel(ctx)
		forwardDone := make(chan error, 1)
		controlDone := make(chan error, 1)
		go func() { forwardDone <- client.ForwardOutput(connectionCtx, ptySession.Output()) }()
		go func() { controlDone <- client.ServeControl(connectionCtx, ptySession) }()
		select {
		case err := <-wait:
			// Wait closes OutputHub only after PTY capture has drained. Let the
			// forwarding side publish those final bytes before disconnecting.
			select {
			case <-forwardDone:
			case <-time.After(2 * time.Second):
			}
			reason := ""
			if err != nil {
				reason = err.Error()
			}
			reportErr := client.ReportExit(err == nil, reason)
			cancel()
			_ = client.Close()
			select {
			case <-controlDone:
			case <-time.After(time.Second):
			}
			if reportErr != nil {
				return reportExitedRuntime(ctx, specPath, spec, ed25519.PrivateKey(decoded), ptySession.Output(), err)
			}
			removeRuntimeCredentials(specPath)
			return err
		case <-ctx.Done():
			_ = ptySession.Terminate(false)
			cancel()
			_ = client.Close()
			select {
			case <-forwardDone:
			case <-time.After(time.Second):
			}
			select {
			case <-controlDone:
			case <-time.After(time.Second):
			}
			select {
			case <-wait:
			case <-time.After(2 * time.Second):
				_ = ptySession.Terminate(true)
				select {
				case <-wait:
				case <-time.After(2 * time.Second):
				}
			}
			return ctx.Err()
		case <-forwardDone:
			select {
			case waitErr := <-wait:
				reason := ""
				if waitErr != nil {
					reason = waitErr.Error()
				}
				reportErr := client.ReportExit(waitErr == nil, reason)
				cancel()
				_ = client.Close()
				select {
				case <-controlDone:
				case <-time.After(time.Second):
				}
				if reportErr != nil {
					return reportExitedRuntime(ctx, specPath, spec, ed25519.PrivateKey(decoded), ptySession.Output(), waitErr)
				}
				removeRuntimeCredentials(specPath)
				return waitErr
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
			_ = client.Close()
			select {
			case <-controlDone:
			case <-time.After(time.Second):
			}
		case <-controlDone:
			cancel()
			_ = client.Close()
			select {
			case <-forwardDone:
			case <-time.After(time.Second):
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func reportExitedRuntime(ctx context.Context, specPath string, spec runtimeSpec, privateKey ed25519.PrivateKey, output *duckruntime.OutputHub, processErr error) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		client, err := RegisterSupervisor(spec.SocketPath, spec.SessionID, spec.RuntimeGeneration, privateKey)
		if err == nil {
			replay := output.Snapshot()
			forwardErr := client.PublishSnapshot(replay)
			if forwardErr == nil {
				reason := ""
				if processErr != nil {
					reason = processErr.Error()
				}
				err = client.ReportExit(processErr == nil, reason)
			}
			_ = client.Close()
			if err == nil && forwardErr == nil {
				removeRuntimeCredentials(specPath)
				return processErr
			}
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func removeRuntimeCredentials(specPath string) {
	_ = os.Remove(filepath.Join(filepath.Dir(specPath), "recovery.key"))
	_ = os.Remove(specPath)
}

func IsRuntimeSubcommand(args []string) (string, bool) {
	if len(args) == 2 && args[0] == runtimeSubcommand {
		return args[1], true
	}
	return "", false
}
