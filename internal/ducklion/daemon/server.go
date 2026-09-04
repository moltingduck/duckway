package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
	"github.com/hackerduck/duckway/internal/ducklion/service"
	"github.com/hackerduck/duckway/internal/ducklion/store"
	"golang.org/x/sys/unix"
)

var ErrAlreadyRunning = errors.New("ducklion daemon is already running")

type Options struct {
	Root       string
	SocketPath string
}

type Server struct {
	root        string
	socketPath  string
	lockFile    *os.File
	listener    *net.UnixListener
	state       *store.SQLite
	service     *service.Service
	registry    *duckruntime.Registry
	instanceID  model.InstanceID
	closeOnce   sync.Once
	done        chan struct{}
	connMu      sync.Mutex
	connections map[*net.UnixConn]struct{}
	handlers    sync.WaitGroup
	lifecycleMu sync.Mutex
	closing     bool
	outputMu    sync.Mutex
	outputs     map[model.SessionID]registeredOutput
	controlMu   sync.Mutex
	controls    map[model.SessionID]*controlPeer
	sequenceMu  sync.Mutex
	sequences   map[model.SessionID]runtimeSequence
}

type runtimeSequence struct {
	identity duckruntime.RuntimeIdentity
	value    uint64
}

type registeredOutput struct {
	identity duckruntime.RuntimeIdentity
	hub      *duckruntime.OutputHub
}

type controlCall struct {
	request protocol.Request
	result  chan protocol.Response
}

type controlPeer struct {
	identity duckruntime.RuntimeIdentity
	conn     *net.UnixConn
	calls    chan controlCall
	done     chan struct{}
	stopOnce sync.Once
}

func (p *controlPeer) stop() {
	p.stopOnce.Do(func() {
		close(p.done)
		_ = p.conn.Close()
	})
}

func DefaultRoot() string { return filepath.Dir(store.DefaultPath()) }

func Open(ctx context.Context, options Options) (*Server, error) {
	root := options.Root
	if root == "" {
		root = DefaultRoot()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := secureRoot(root); err != nil {
		return nil, err
	}
	lockFile, err := openSecureLock(filepath.Join(root, "daemon.lock"))
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, ErrAlreadyRunning
	}
	fail := func(err error) (*Server, error) {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
		return nil, err
	}
	state, err := store.Open(ctx, filepath.Join(root, "ducklion.db"))
	if err != nil {
		return fail(err)
	}
	instanceID, err := state.InstanceID(ctx)
	if err != nil {
		_ = state.Close()
		return fail(err)
	}
	socketPath := options.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(root, "ducklion.sock")
	}
	if err := prepareSocketPath(root, socketPath); err != nil {
		_ = state.Close()
		return fail(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		_ = state.Close()
		return fail(fmt.Errorf("listen on Ducklion socket: %w", err))
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		_ = state.Close()
		return fail(err)
	}
	return &Server{root: root, socketPath: socketPath, lockFile: lockFile, listener: listener, state: state,
		service: service.New(state), registry: duckruntime.NewRegistry(instanceID, state), instanceID: instanceID, done: make(chan struct{}),
		connections: make(map[*net.UnixConn]struct{}), outputs: make(map[model.SessionID]registeredOutput), controls: make(map[model.SessionID]*controlPeer),
		sequences: make(map[model.SessionID]runtimeSequence)}, nil
}

func (s *Server) SocketPath() string           { return s.socketPath }
func (s *Server) InstanceID() model.InstanceID { return s.instanceID }

func (s *Server) Serve() error {
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				return err
			}
		}
		s.lifecycleMu.Lock()
		if s.closing {
			s.lifecycleMu.Unlock()
			_ = conn.Close()
			continue
		}
		s.connMu.Lock()
		s.connections[conn] = struct{}{}
		s.connMu.Unlock()
		s.handlers.Add(1)
		s.lifecycleMu.Unlock()
		go s.handle(conn)
	}
}

func (s *Server) handle(conn *net.UnixConn) {
	defer func() {
		_ = conn.Close()
		s.connMu.Lock()
		delete(s.connections, conn)
		s.connMu.Unlock()
		s.handlers.Done()
	}()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	uid, err := peerUID(conn)
	if err != nil || uid != uint32(os.Geteuid()) {
		return
	}
	codec := bridge.NewCodec(conn, conn, bridge.DefaultMaxFrame)
	var remote protocol.Handshake
	if err := codec.Read(&remote); err != nil {
		return
	}
	if remote.Role == protocol.RoleSupervisor {
		s.handleSupervisor(conn, codec, remote)
		return
	}
	if remote.Role == protocol.RoleSupervisorControl {
		s.handleSupervisorControl(conn, codec, remote)
		return
	}
	if remote.Role != protocol.RoleDucklord {
		_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "peer role is not available on this endpoint"}})
		return
	}
	terminalOwner := model.Owner{Kind: model.OwnerTerminal, ID: remote.Principal}
	if err := terminalOwner.Validate(); err != nil || len(remote.Principal) > 128 {
		_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid Ducklord principal"}})
		return
	}
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: []string{"status", "sessions_list", "output_subscribe", "session_input", "session_resize"}}
	negotiated, protocolError := protocol.Negotiate(local, remote)
	if protocolError != nil {
		_ = codec.Write(protocol.HandshakeResponse{Error: protocolError})
		return
	}
	if err := codec.Write(protocol.HandshakeResponse{Handshake: &negotiated}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	for {
		var request protocol.Request
		if err := codec.Read(&request); err != nil {
			return
		}
		if err := request.Validate(); err != nil {
			if request.ID == "" {
				return
			}
			_ = codec.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}})
			continue
		}
		if request.Type == "session.output_subscribe" {
			if !hasCapability(negotiated.Capabilities, "output_subscribe") {
				writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "output subscription capability was not negotiated")
				continue
			}
			s.handleOutputSubscription(codec, request)
			return
		}
		response := s.route(request, negotiated.Capabilities, negotiated.Principal)
		if err := codec.Write(response); err != nil {
			return
		}
	}
}

type registrationConnection struct {
	mu             sync.Mutex
	conn           *net.UnixConn
	armed          bool
	closeRequested bool
}

func (c *registrationConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		c.closeRequested = true
		return nil
	}
	return c.conn.Close()
}

func (c *registrationConnection) arm() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
	if c.closeRequested {
		return c.conn.Close()
	}
	return nil
}

func (s *Server) handleSupervisor(conn *net.UnixConn, codec *bridge.Codec, remote protocol.Handshake) {
	sessionID, err := model.ParseSessionID(remote.Principal)
	if err != nil || string(sessionID) != remote.Principal {
		_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "supervisor principal must be the canonical session ID"}})
		return
	}
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: []string{"supervisor_recovery", "output_publish"}}
	negotiated, protocolError := protocol.Negotiate(local, remote)
	if protocolError != nil || !hasCapability(negotiated.Capabilities, "supervisor_recovery") || !hasCapability(negotiated.Capabilities, "output_publish") {
		if protocolError == nil {
			protocolError = &protocol.Error{Code: protocol.ErrIncompatible, Message: "supervisor recovery capability is required"}
		}
		_ = codec.Write(protocol.HandshakeResponse{Error: protocolError})
		return
	}
	if err := codec.Write(protocol.HandshakeResponse{Handshake: &negotiated}); err != nil {
		return
	}
	var beginRequest protocol.Request
	if err := codec.Read(&beginRequest); err != nil || beginRequest.Validate() != nil || beginRequest.Type != "supervisor.register_begin" || beginRequest.SessionID != string(sessionID) || (beginRequest.InstanceID != "" && beginRequest.InstanceID != string(s.instanceID)) {
		writeSupervisorError(codec, beginRequest.ID, protocol.ErrInvalidArgument, "registration must begin with the bound session")
		return
	}
	if len(beginRequest.Body) != 0 || beginRequest.RuntimeGeneration == nil || *beginRequest.RuntimeGeneration == 0 {
		writeSupervisorError(codec, beginRequest.ID, protocol.ErrInvalidArgument, "invalid registration begin request")
		return
	}
	generation := *beginRequest.RuntimeGeneration
	challenge, err := s.registry.BeginRegistration(context.Background(), sessionID, generation)
	if err != nil {
		writeSupervisorError(codec, beginRequest.ID, protocol.ErrAdapterUnhealthy, "runtime is not recoverable")
		return
	}
	challengeJSON, _ := json.Marshal(protocol.SupervisorChallenge{ChallengeID: challenge.ID, Nonce: challenge.Nonce, InstanceID: string(s.instanceID)})
	if err := codec.Write(protocol.Response{ID: beginRequest.ID, Result: challengeJSON}); err != nil {
		return
	}
	var completeRequest protocol.Request
	if err := codec.Read(&completeRequest); err != nil || completeRequest.Validate() != nil || completeRequest.Type != "supervisor.register_complete" || completeRequest.SessionID != string(sessionID) || completeRequest.InstanceID != string(s.instanceID) || completeRequest.RuntimeGeneration == nil || *completeRequest.RuntimeGeneration != generation {
		writeSupervisorError(codec, completeRequest.ID, protocol.ErrInvalidArgument, "registration proof must follow its challenge")
		return
	}
	var complete protocol.SupervisorRegisterComplete
	if err := decodeStrict(completeRequest.Body, &complete); err != nil || complete.ChallengeID != challenge.ID {
		writeSupervisorError(codec, completeRequest.ID, protocol.ErrInvalidArgument, "invalid registration proof request")
		return
	}
	runtimeConn := &registrationConnection{conn: conn}
	identity, err := s.registry.CompleteRegistration(context.Background(), sessionID, generation, complete.ChallengeID, complete.Proof,
		uint16(negotiated.Major), uint16(negotiated.Minor), runtimeConn)
	if err != nil {
		writeSupervisorError(codec, completeRequest.ID, protocol.ErrAdapterUnhealthy, "runtime registration failed")
		return
	}
	if err := s.state.MarkRuntimeConnected(context.Background(), identity.SessionID, identity.Generation); err != nil {
		s.registry.Disconnect(identity)
		writeSupervisorError(codec, completeRequest.ID, protocol.ErrStaleGeneration, "runtime state changed during registration")
		return
	}
	output := s.activateOutput(identity)
	defer func() {
		s.deactivateOutput(identity)
		s.registry.Disconnect(identity)
		_ = s.state.MarkRuntimeDisconnected(context.Background(), identity.SessionID, identity.Generation)
	}()
	if err := runtimeConn.arm(); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	registeredJSON, _ := json.Marshal(protocol.SupervisorRegistered{SessionID: string(identity.SessionID), RuntimeGeneration: identity.Generation, LeaseID: identity.LeaseID})
	if err := codec.Write(protocol.Response{ID: completeRequest.ID, Result: registeredJSON}); err != nil {
		return
	}
	for {
		var request protocol.Request
		if err := codec.Read(&request); err != nil {
			return
		}
		if request.Validate() != nil || request.InstanceID != string(s.instanceID) || request.SessionID != string(identity.SessionID) || request.RuntimeGeneration == nil {
			writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "operation is unavailable to supervisors")
			continue
		}
		if *request.RuntimeGeneration != identity.Generation || !s.registry.IsCurrent(identity) {
			writeSupervisorError(codec, request.ID, protocol.ErrStaleGeneration, "runtime generation changed")
			continue
		}
		var result []byte
		switch request.Type {
		case "supervisor.ping":
			if len(request.Body) != 0 {
				writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "invalid supervisor ping")
				continue
			}
			result, _ = json.Marshal(map[string]bool{"ok": true})
		case "supervisor.output":
			var published protocol.SupervisorOutput
			if err := decodeStrict(request.Body, &published); err != nil || len(published.Data) == 0 || len(published.Data) > 64<<10 || !s.registry.IsCurrent(identity) {
				writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "invalid supervisor output")
				continue
			}
			_, end := output.Bounds()
			if published.Offset != end {
				writeSupervisorError(codec, request.ID, protocol.ErrStaleGeneration, "output offset is not contiguous")
				continue
			}
			frame := output.Publish(published.Data)
			result, _ = json.Marshal(protocol.SupervisorOutputAck{Offset: frame.Offset, Length: uint64(len(frame.Data))})
		default:
			writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "operation is unavailable to supervisors")
			continue
		}
		if err := codec.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
			return
		}
	}
}

func (s *Server) handleSupervisorControl(conn *net.UnixConn, codec *bridge.Codec, remote protocol.Handshake) {
	sessionID, err := model.ParseSessionID(remote.Principal)
	if err != nil || string(sessionID) != remote.Principal {
		_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "control principal must be the canonical session ID"}})
		return
	}
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: []string{"runtime_control"}}
	negotiated, protocolError := protocol.Negotiate(local, remote)
	if protocolError != nil || !hasCapability(negotiated.Capabilities, "runtime_control") {
		if protocolError == nil {
			protocolError = &protocol.Error{Code: protocol.ErrIncompatible, Message: "runtime control capability is required"}
		}
		_ = codec.Write(protocol.HandshakeResponse{Error: protocolError})
		return
	}
	if err := codec.Write(protocol.HandshakeResponse{Handshake: &negotiated}); err != nil {
		return
	}
	var begin protocol.Request
	if err := codec.Read(&begin); err != nil || begin.Validate() != nil || begin.Type != "supervisor.control_begin" || begin.SessionID != string(sessionID) || begin.RuntimeGeneration == nil || len(begin.Body) != 0 {
		writeSupervisorError(codec, begin.ID, protocol.ErrInvalidArgument, "invalid control authentication request")
		return
	}
	generation := *begin.RuntimeGeneration
	session, err := s.state.GetSession(context.Background(), sessionID)
	if err != nil || session.Status != model.StatusRunning || session.RuntimeGeneration != generation || len(session.RecoveryPublicKey) != ed25519.PublicKeySize {
		writeSupervisorError(codec, begin.ID, protocol.ErrAdapterUnhealthy, "runtime control is unavailable")
		return
	}
	identity, ok := s.registry.Current(sessionID, generation)
	if !ok {
		writeSupervisorError(codec, begin.ID, protocol.ErrAdapterUnhealthy, "runtime control is unavailable")
		return
	}
	nonce, err := model.NewRecoveryNonce()
	if err != nil {
		writeSupervisorError(codec, begin.ID, protocol.ErrInternal, "could not create control challenge")
		return
	}
	challengeID := uuid.NewString()
	challengeJSON, _ := json.Marshal(protocol.SupervisorChallenge{ChallengeID: challengeID, Nonce: nonce, InstanceID: string(s.instanceID)})
	if err := codec.Write(protocol.Response{ID: begin.ID, Result: challengeJSON}); err != nil {
		return
	}
	var complete protocol.Request
	if err := codec.Read(&complete); err != nil || complete.Validate() != nil || complete.Type != "supervisor.control_complete" || complete.SessionID != string(sessionID) ||
		complete.InstanceID != string(s.instanceID) || complete.RuntimeGeneration == nil || *complete.RuntimeGeneration != generation {
		writeSupervisorError(codec, complete.ID, protocol.ErrInvalidArgument, "invalid control proof request")
		return
	}
	var proof protocol.SupervisorRegisterComplete
	if err := decodeStrict(complete.Body, &proof); err != nil || proof.ChallengeID != challengeID ||
		!model.VerifyRecoveryProof(ed25519.PublicKey(session.RecoveryPublicKey), s.instanceID, sessionID, generation, nonce, proof.Proof, uint16(negotiated.Major), uint16(negotiated.Minor)) {
		writeSupervisorError(codec, complete.ID, protocol.ErrAdapterUnhealthy, "runtime control authentication failed")
		return
	}
	if current, currentOK := s.registry.Current(sessionID, generation); !currentOK || current != identity {
		writeSupervisorError(codec, complete.ID, protocol.ErrStaleGeneration, "runtime changed during control authentication")
		return
	}
	ready, _ := json.Marshal(protocol.SupervisorControlReady{SessionID: string(sessionID), RuntimeGeneration: generation})
	if err := codec.Write(protocol.Response{ID: complete.ID, Result: ready}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	peer := &controlPeer{identity: identity, conn: conn, calls: make(chan controlCall), done: make(chan struct{})}
	s.controlMu.Lock()
	old := s.controls[sessionID]
	s.controls[sessionID] = peer
	s.controlMu.Unlock()
	if old != nil {
		old.stop()
	}
	defer func() {
		peer.stop()
		s.controlMu.Lock()
		if s.controls[sessionID] == peer {
			delete(s.controls, sessionID)
		}
		s.controlMu.Unlock()
	}()
	for {
		select {
		case <-s.done:
			return
		case <-peer.done:
			return
		case call := <-peer.calls:
			if !s.registry.IsCurrent(identity) {
				call.result <- protocol.Response{ID: call.request.ID, Error: &protocol.Error{Code: protocol.ErrStaleGeneration, Message: "runtime is no longer current"}}
				continue
			}
			if call.request.Type == "supervisor.input" {
				var input protocol.SupervisorInput
				if err := json.Unmarshal(call.request.Body, &input); err != nil {
					call.result <- protocol.Response{ID: call.request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid runtime input command"}}
					continue
				}
				input.Sequence = s.nextInputSequence(identity)
				call.request.Body, _ = json.Marshal(input)
			}
			if err := codec.Write(call.request); err != nil {
				call.result <- protocol.Response{ID: call.request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "runtime control disconnected", Retryable: true}}
				return
			}
			var response protocol.Response
			if err := codec.Read(&response); err != nil || response.Validate() != nil || response.ID != call.request.ID {
				call.result <- protocol.Response{ID: call.request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "runtime control response failed", Retryable: true}}
				return
			}
			call.result <- response
		}
	}
}

func (s *Server) callControl(ctx context.Context, sessionID model.SessionID, request protocol.Request) protocol.Response {
	s.controlMu.Lock()
	peer := s.controls[sessionID]
	s.controlMu.Unlock()
	if peer == nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "runtime control is unavailable", Retryable: true}}
	}
	call := controlCall{request: request, result: make(chan protocol.Response, 1)}
	select {
	case peer.calls <- call:
	case <-peer.done:
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "runtime control disconnected", Retryable: true}}
	case <-ctx.Done():
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrBusy, Message: "runtime control request was not admitted"}}
	}
	select {
	case response := <-call.result:
		return response
	case <-ctx.Done():
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "runtime control outcome is unknown"}}
	}
}

func (s *Server) nextInputSequence(identity duckruntime.RuntimeIdentity) uint64 {
	s.sequenceMu.Lock()
	defer s.sequenceMu.Unlock()
	current := s.sequences[identity.SessionID]
	if current.identity != identity {
		current = runtimeSequence{identity: identity}
	}
	current.value++
	s.sequences[identity.SessionID] = current
	return current.value
}

func (s *Server) activateOutput(identity duckruntime.RuntimeIdentity) *duckruntime.OutputHub {
	hub := duckruntime.NewOutputHub(4 << 20)
	s.outputMu.Lock()
	previous, replaced := s.outputs[identity.SessionID]
	s.outputs[identity.SessionID] = registeredOutput{identity: identity, hub: hub}
	s.outputMu.Unlock()
	if replaced {
		previous.hub.Close()
	}
	return hub
}

func (s *Server) deactivateOutput(identity duckruntime.RuntimeIdentity) {
	s.outputMu.Lock()
	current, ok := s.outputs[identity.SessionID]
	if ok && current.identity == identity {
		delete(s.outputs, identity.SessionID)
	}
	s.outputMu.Unlock()
	if ok && current.identity == identity {
		current.hub.Close()
		s.sequenceMu.Lock()
		if sequence, exists := s.sequences[identity.SessionID]; exists && sequence.identity == identity {
			delete(s.sequences, identity.SessionID)
		}
		s.sequenceMu.Unlock()
	}
}

func (s *Server) handleOutputSubscription(codec *bridge.Codec, request protocol.Request) {
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil || request.InstanceID != string(s.instanceID) || request.RuntimeGeneration == nil || len(request.Body) == 0 {
		writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "invalid output subscription")
		return
	}
	var subscription protocol.OutputSubscribe
	if err := decodeStrict(request.Body, &subscription); err != nil {
		writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "invalid output subscription")
		return
	}
	s.outputMu.Lock()
	current, ok := s.outputs[sessionID]
	s.outputMu.Unlock()
	if !ok {
		writeSupervisorError(codec, request.ID, protocol.ErrAdapterUnhealthy, "session runtime is not connected")
		return
	}
	if current.identity.Generation != *request.RuntimeGeneration {
		writeSupervisorError(codec, request.ID, protocol.ErrStaleGeneration, "runtime generation changed")
		return
	}
	replay, stream, cancel, err := current.hub.Subscribe(subscription.Offset, 32)
	if err != nil {
		writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, err.Error())
		return
	}
	defer cancel()
	_, end := current.hub.Bounds()
	subscriptionID := uuid.NewString()
	result, _ := json.Marshal(protocol.OutputSubscribeResult{SubscriptionID: subscriptionID, RuntimeID: current.identity.LeaseID, InstanceID: string(s.instanceID), SessionID: string(sessionID),
		RuntimeGeneration: current.identity.Generation, StartOffset: replay.Offset, EndOffset: end, Gap: replay.Gap})
	if err := codec.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
		return
	}
	if err := writeOutputEvents(codec, subscriptionID, s.instanceID, current.identity, replay); err != nil {
		return
	}
	nextOffset := replay.Offset + uint64(len(replay.Data))
	for frame := range stream {
		if err := writeOutputEvents(codec, subscriptionID, s.instanceID, current.identity, frame); err != nil {
			return
		}
		nextOffset = frame.Offset + uint64(len(frame.Data))
	}
	_, finalEnd := current.hub.Bounds()
	reason := "runtime_disconnected"
	if nextOffset < finalEnd {
		reason = "subscriber_lag"
	}
	_ = codec.Write(protocol.OutputEvent{Type: "output_end", SubscriptionID: subscriptionID, RuntimeID: current.identity.LeaseID,
		InstanceID: string(s.instanceID), SessionID: string(current.identity.SessionID), RuntimeGeneration: current.identity.Generation,
		Frame: protocol.OutputFrame{Offset: nextOffset}, Reason: reason})
}

func writeOutputEvents(codec *bridge.Codec, subscriptionID string, instanceID model.InstanceID, identity duckruntime.RuntimeIdentity, frame duckruntime.OutputFrame) error {
	const maxChunk = 64 << 10
	if len(frame.Data) == 0 {
		return nil
	}
	for start := 0; start < len(frame.Data); start += maxChunk {
		end := start + maxChunk
		if end > len(frame.Data) {
			end = len(frame.Data)
		}
		event := protocol.OutputEvent{Type: "output", SubscriptionID: subscriptionID, RuntimeID: identity.LeaseID, InstanceID: string(instanceID), SessionID: string(identity.SessionID),
			RuntimeGeneration: identity.Generation, Frame: protocol.OutputFrame{Offset: frame.Offset + uint64(start), Data: frame.Data[start:end], Gap: frame.Gap && start == 0}}
		if err := codec.Write(event); err != nil {
			return err
		}
	}
	return nil
}

func hasCapability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON value")
	}
	return nil
}

func writeSupervisorError(codec *bridge.Codec, requestID string, code protocol.ErrorCode, message string) {
	if requestID == "" {
		return
	}
	_ = codec.Write(protocol.Response{ID: requestID, Error: &protocol.Error{Code: code, Message: message}})
}

func (s *Server) route(request protocol.Request, capabilities []string, principal string) protocol.Response {
	if request.InstanceID != "" && request.InstanceID != string(s.instanceID) {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrNotFound, Message: "Ducklion instance does not match"}}
	}
	switch request.Type {
	case "status":
		if !hasCapability(capabilities, "status") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "status capability was not negotiated"}}
		}
		result, _ := json.Marshal(map[string]any{"instance_id": s.instanceID, "protocol_major": protocol.Major, "protocol_minor": protocol.Minor})
		return protocol.Response{ID: request.ID, Result: result}
	case "sessions.list":
		if !hasCapability(capabilities, "sessions_list") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session list capability was not negotiated"}}
		}
		sessions, err := s.state.ListSessions(context.Background())
		if err != nil {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not list sessions", Retryable: true}}
		}
		summaries := make([]protocol.SessionSummary, 0, len(sessions))
		for _, session := range sessions {
			summaries = append(summaries, protocol.SessionSummary{SessionID: string(session.ID), Handle: session.Handle, Kind: session.Kind,
				AgentType: session.AgentType, CWD: session.CWD, Status: session.Status, Writer: session.Writer, OwnershipEpoch: session.OwnershipEpoch,
				RuntimeGeneration: session.RuntimeGeneration, TaskState: session.TaskState, AdapterState: session.AdapterState})
		}
		result, _ := json.Marshal(summaries)
		return protocol.Response{ID: request.ID, Result: result}
	case "session.input":
		if !hasCapability(capabilities, "session_input") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session input capability was not negotiated"}}
		}
		return s.routeSessionInput(request, principal)
	case "session.resize":
		if !hasCapability(capabilities, "session_resize") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session resize capability was not negotiated"}}
		}
		return s.routeSessionResize(request, principal)
	default:
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "unsupported operation"}}
	}
}

func (s *Server) authorizeTerminalControl(request protocol.Request, principal string) (model.Session, model.Owner, *protocol.Error) {
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil || request.InstanceID != string(s.instanceID) || request.OwnershipEpoch == nil || request.RuntimeGeneration == nil {
		return model.Session{}, model.Owner{}, &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session identity and fences are required"}
	}
	session, err := s.state.GetSession(context.Background(), sessionID)
	if err != nil {
		return model.Session{}, model.Owner{}, &protocol.Error{Code: protocol.ErrNotFound, Message: "session not found"}
	}
	if session.RuntimeGeneration != *request.RuntimeGeneration {
		generation := session.RuntimeGeneration
		return model.Session{}, model.Owner{}, &protocol.Error{Code: protocol.ErrStaleGeneration, Message: "runtime generation changed", RuntimeGeneration: &generation}
	}
	owner := model.Owner{Kind: model.OwnerTerminal, ID: principal}
	if session.Kind == model.KindAgent {
		if err := session.AuthorizeAgentInput(owner, *request.OwnershipEpoch, *request.RuntimeGeneration); err != nil {
			epoch, generation := session.OwnershipEpoch, session.RuntimeGeneration
			return model.Session{}, model.Owner{}, &protocol.Error{Code: serviceMapError(err), Message: err.Error(), OwnershipEpoch: &epoch, RuntimeGeneration: &generation}
		}
	} else if session.Status != model.StatusRunning || session.AdapterState != model.AdapterUnavailable {
		return model.Session{}, model.Owner{}, &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "shell runtime is unavailable"}
	}
	return session, owner, nil
}

func (s *Server) routeSessionInput(request protocol.Request, principal string) protocol.Response {
	var input protocol.SessionInput
	if err := decodeStrict(request.Body, &input); err != nil || len(input.Data) == 0 || len(input.Data) > 64<<10 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "input must contain 1 to 65536 bytes"}}
	}
	session, owner, protocolError := s.authorizeTerminalControl(request, principal)
	if protocolError != nil {
		return protocol.Response{ID: request.ID, Error: protocolError}
	}
	s.controlMu.Lock()
	peer := s.controls[session.ID]
	s.controlMu.Unlock()
	if peer == nil || peer.identity.Generation != session.RuntimeGeneration {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "runtime control is unavailable", Retryable: true}}
	}
	body, _ := json.Marshal(protocol.SupervisorInput{Owner: owner, Data: input.Data})
	forwarded := request
	forwarded.Type = "supervisor.input"
	forwarded.Body = body
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.callControl(ctx, session.ID, forwarded)
}

func (s *Server) routeSessionResize(request protocol.Request, principal string) protocol.Response {
	var resize protocol.SessionResize
	if err := decodeStrict(request.Body, &resize); err != nil || resize.Rows < 5 || resize.Rows > 200 || resize.Cols < 20 || resize.Cols > 500 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "resize is outside supported bounds"}}
	}
	session, _, protocolError := s.authorizeTerminalControl(request, principal)
	if protocolError != nil {
		return protocol.Response{ID: request.ID, Error: protocolError}
	}
	body, _ := json.Marshal(protocol.SupervisorResize{Rows: resize.Rows, Cols: resize.Cols})
	forwarded := request
	forwarded.Type = "supervisor.resize"
	forwarded.Body = body
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.callControl(ctx, session.ID, forwarded)
}

func serviceMapError(err error) protocol.ErrorCode {
	switch {
	case errors.Is(err, model.ErrNotOwner):
		return protocol.ErrNotOwner
	case errors.Is(err, model.ErrStaleEpoch):
		return protocol.ErrStaleEpoch
	case errors.Is(err, model.ErrStaleGeneration):
		return protocol.ErrStaleGeneration
	default:
		return protocol.ErrAdapterUnhealthy
	}
}

func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closing = true
		close(s.done)
		s.registry.CloseAll()
		closeErr = s.listener.Close()
		s.lifecycleMu.Unlock()
		s.connMu.Lock()
		for conn := range s.connections {
			_ = conn.Close()
		}
		s.connMu.Unlock()
		s.handlers.Wait()
		_ = os.Remove(s.socketPath)
		_ = s.state.Close()
		_ = unix.Flock(int(s.lockFile.Fd()), unix.LOCK_UN)
		_ = s.lockFile.Close()
	})
	return closeErr
}

func Run(ctx context.Context, options Options) error {
	server, err := Open(ctx, options)
	if err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	select {
	case err := <-serveErr:
		_ = server.Close()
		return err
	case <-ctx.Done():
		_ = server.Close()
		if err := <-serveErr; err != nil {
			return err
		}
		return nil
	}
}

func secureRoot(root string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("ducklion root is not a real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("ducklion root is not owned by the current user")
	}
	return os.Chmod(root, 0700)
}

func openSecureLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, fmt.Errorf("daemon lock is not a private regular file")
	}
	if err := unix.Fchmod(fd, 0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func prepareSocketPath(root, socketPath string) error {
	abs, err := filepath.Abs(socketPath)
	if err != nil {
		return err
	}
	if filepath.Dir(abs) != root {
		return fmt.Errorf("ducklion socket must be directly inside its private root")
	}
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket Ducklion path")
	}
	return os.Remove(abs)
}
