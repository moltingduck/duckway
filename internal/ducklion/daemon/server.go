package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	"github.com/hackerduck/duckway/internal/ducklion/supervisor"
	"golang.org/x/sys/unix"
)

var ErrAlreadyRunning = errors.New("ducklion daemon is already running")

const (
	maxOutputSubscriptionsPerConnection = 101 // configured Ducklord maximum plus one atomic handoff slot
	maxOutputSubscriptionsPerSession    = 32
	maxOutputSubscriptionsGlobal        = 512
	maxSessionEventSubscriptionsGlobal  = 32
	maxSupervisorActivityConnections    = 64
	maxInputSequenceRecoveryAttempts    = 65536
	defaultRetainedOutputTTL            = 7 * 24 * time.Hour
	maxRetainedOutputBytesGlobal        = 512 << 20
)

type Options struct {
	Root              string
	SocketPath        string
	RuntimeLauncher   func(string) error
	SessionCleaner    func(string) error
	RetainedOutputTTL time.Duration
}

type Server struct {
	root                         string
	socketPath                   string
	lockFile                     *os.File
	listener                     *net.UnixListener
	state                        *store.SQLite
	service                      *service.Service
	registry                     *duckruntime.Registry
	instanceID                   model.InstanceID
	closeOnce                    sync.Once
	done                         chan struct{}
	connMu                       sync.Mutex
	connections                  map[*net.UnixConn]struct{}
	ducklords                    map[string]*ducklordLease
	handlers                     sync.WaitGroup
	lifecycleMu                  sync.Mutex
	lifecycleWorkersMu           sync.Mutex
	lifecycleWorkers             map[model.SessionID]struct{}
	lifecycleWorkersWG           sync.WaitGroup
	lifecycleCoordinatorWG       sync.WaitGroup
	retainedOutputWG             sync.WaitGroup
	createMu                     sync.Mutex
	closing                      bool
	outputMu                     sync.Mutex
	outputs                      map[model.SessionID]registeredOutput
	outputSubscriptions          int
	outputSubscriptionsBySession map[model.SessionID]int
	controlMu                    sync.Mutex
	controls                     map[model.SessionID]*controlPeer
	sequenceMu                   sync.Mutex
	sequences                    map[model.SessionID]runtimeSequence
	operationMu                  sync.Mutex
	operations                   map[model.SessionID]*sync.Mutex
	agentEventMu                 sync.Mutex
	agentEvents                  map[string][]protocol.SupervisorAgentEvent
	agentEventCount              int
	agentEventBytes              int
	sessionEventMu               sync.Mutex
	sessionEventSubscriptions    int
	attentionMu                  sync.Mutex
	attentionRates               map[string]attentionRate
	activitySlots                chan struct{}
	runtimeLauncher              func(string) error
	sessionCleaner               func(string) error
	retainedOutputTTL            time.Duration
	retainedOutputSweep          chan struct{}
	retainedOutputCancel         context.CancelFunc
}

type ducklordLease struct {
	processID   string
	control     *net.UnixConn
	connections map[string]*net.UnixConn
}

const maxDucklordObserverConnections = 8

type attentionRate struct {
	offset   uint64
	sequence uint64
	at       time.Time
}

type runtimeSequence struct {
	identity duckruntime.RuntimeIdentity
	value    uint64
}

type registeredOutput struct {
	identity duckruntime.RuntimeIdentity
	hub      *duckruntime.OutputHub
}

type ducklordWire struct {
	codec *bridge.Codec
	conn  *net.UnixConn
	mu    sync.Mutex
}

func (w *ducklordWire) Write(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := w.codec.Write(value)
	_ = w.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		_ = w.conn.Close()
	}
	return err
}

type preparedOutputSubscription struct {
	metadata protocol.OutputSubscribeResult
	identity duckruntime.RuntimeIdentity
	replay   duckruntime.OutputFrame
	stream   <-chan duckruntime.OutputFrame
	cancel   func()
	hub      *duckruntime.OutputHub
}

type preparedSessionSubscription struct {
	metadata protocol.SessionSnapshotSubscribeResult
	cursor   uint64
	ctx      context.Context
	cancel   context.CancelFunc
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
	if err := state.PrepareRuntimeRecovery(ctx); err != nil {
		_ = state.Close()
		return fail(fmt.Errorf("prepare runtime recovery: %w", err))
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
	server := &Server{root: root, socketPath: socketPath, lockFile: lockFile, listener: listener, state: state,
		service: service.New(state), registry: duckruntime.NewRegistry(instanceID, state), instanceID: instanceID, done: make(chan struct{}),
		connections: make(map[*net.UnixConn]struct{}), ducklords: make(map[string]*ducklordLease), outputs: make(map[model.SessionID]registeredOutput), controls: make(map[model.SessionID]*controlPeer),
		outputSubscriptionsBySession: make(map[model.SessionID]int),
		activitySlots:                make(chan struct{}, maxSupervisorActivityConnections),
		sequences:                    make(map[model.SessionID]runtimeSequence), runtimeLauncher: options.RuntimeLauncher, sessionCleaner: options.SessionCleaner,
		retainedOutputTTL: options.RetainedOutputTTL}
	server.retainedOutputSweep = make(chan struct{}, 1)
	server.agentEvents = make(map[string][]protocol.SupervisorAgentEvent)
	server.lifecycleWorkers = make(map[model.SessionID]struct{})
	if server.runtimeLauncher == nil {
		server.runtimeLauncher = server.spawnRuntime
	}
	if server.sessionCleaner == nil {
		server.sessionCleaner = os.RemoveAll
	}
	if server.retainedOutputTTL <= 0 {
		server.retainedOutputTTL = defaultRetainedOutputTTL
	}
	if err := server.cleanupExpiredRetainedOutput(ctx, time.Now()); err != nil {
		log.Printf("[ducklion] retained PTY cleanup: %v", err)
	}
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	server.retainedOutputCancel = cancelSweep
	server.retainedOutputWG.Add(1)
	go server.runRetainedOutputSweeper(sweepCtx)
	server.lifecycleCoordinatorWG.Add(1)
	go server.runLifecycleCoordinator()
	return server, nil
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
	if remote.Role == protocol.RoleSupervisorActivity {
		s.handleSupervisorActivity(conn, codec, remote)
		return
	}
	if remote.Role != protocol.RoleDucklord && remote.Role != protocol.RoleDuckwayCC {
		_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "peer role is not available on this endpoint"}})
		return
	}
	ownerKind := model.OwnerTerminal
	if remote.Role == protocol.RoleDuckwayCC {
		ownerKind = model.OwnerCC
	}
	peerOwner := model.Owner{Kind: ownerKind, ID: remote.Principal}
	if err := peerOwner.Validate(); err != nil || len(remote.Principal) > 128 || remote.Role == protocol.RoleDucklord && !protocol.ValidDucklordPrincipal(remote.Principal) {
		_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid peer principal"}})
		return
	}
	capabilities := []string{"status", "sessions_list", "session_create", "session_stop", "session_destroy", "session_lifecycle", "session_yield", "output_subscribe", "output_unsubscribe", "session_input", "session_resize", "session_resize_barrier", "session_events"}
	if remote.Role == protocol.RoleDucklord {
		if remote.OwnerID != remote.Principal || uuid.Validate(remote.ProcessID) != nil || uuid.Validate(remote.ConnectionID) != nil ||
			(remote.ConnectionRole != protocol.ConnectionControl && remote.ConnectionRole != protocol.ConnectionObserver) {
			_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid Ducklord connection identity"}})
			return
		}
		if remote.ConnectionRole == protocol.ConnectionObserver {
			capabilities = []string{"status", "sessions_list", "output_subscribe", "output_unsubscribe", "session_events"}
		}
	}
	if remote.Role == protocol.RoleDuckwayCC {
		capabilities = []string{"status", "sessions_list", "session_create_agent", "session_stop", "session_destroy", "session_lifecycle", "session_yield", "session_task", "discord_binding", "discord_unbind", "agent_task"}
	}
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: capabilities}
	negotiated, protocolError := protocol.Negotiate(local, remote)
	if protocolError != nil {
		_ = codec.Write(protocol.HandshakeResponse{Error: protocolError})
		return
	}
	if remote.Role == protocol.RoleDucklord {
		s.connMu.Lock()
		lease := s.ducklords[remote.Principal]
		busy := false
		if lease == nil {
			lease = &ducklordLease{processID: remote.ProcessID, connections: make(map[string]*net.UnixConn)}
			s.ducklords[remote.Principal] = lease
		}
		observerCount := len(lease.connections)
		if lease.control != nil {
			observerCount--
		}
		busy = lease.processID != remote.ProcessID || lease.connections[remote.ConnectionID] != nil ||
			remote.ConnectionRole == protocol.ConnectionControl && lease.control != nil ||
			remote.ConnectionRole == protocol.ConnectionObserver && observerCount >= maxDucklordObserverConnections
		if !busy {
			lease.connections[remote.ConnectionID] = conn
			if remote.ConnectionRole == protocol.ConnectionControl {
				lease.control = conn
			}
		}
		s.connMu.Unlock()
		if busy {
			_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrBusy, Message: "Ducklord owner name is already connected by another process or control connection", Retryable: true}})
			return
		}
		defer func() {
			s.connMu.Lock()
			lease := s.ducklords[remote.Principal]
			key := remote.ConnectionID
			if lease != nil && lease.connections[key] == conn {
				delete(lease.connections, key)
				if lease.control == conn {
					lease.control = nil
				}
				if len(lease.connections) == 0 {
					delete(s.ducklords, remote.Principal)
				}
			}
			s.connMu.Unlock()
		}()
	}
	if err := codec.Write(protocol.HandshakeResponse{Handshake: &negotiated}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	wire := &ducklordWire{codec: codec, conn: conn}
	var subscriptionsMu sync.Mutex
	outputSubscriptions := make(map[string]func())
	var sessionSubscriptionCancel func()
	var sessionSubscriptionID string
	var subscriptionHandlers sync.WaitGroup
	routeRequests := make(chan protocol.Request, 32)
	var routeHandler sync.WaitGroup
	routeHandler.Add(1)
	go func() {
		defer routeHandler.Done()
		for request := range routeRequests {
			response := s.route(request, negotiated.Capabilities, negotiated.Role, negotiated.Principal)
			if err := wire.Write(response); err != nil {
				return
			}
		}
	}()
	defer func() {
		subscriptionsMu.Lock()
		for id, cancel := range outputSubscriptions {
			cancel()
			delete(outputSubscriptions, id)
		}
		if sessionSubscriptionCancel != nil {
			sessionSubscriptionCancel()
			sessionSubscriptionCancel = nil
		}
		subscriptionsMu.Unlock()
		subscriptionHandlers.Wait()
		close(routeRequests)
		routeHandler.Wait()
	}()
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
		if request.Type == "sessions.events_subscribe" {
			if negotiated.Role != protocol.RoleDucklord || !hasCapability(negotiated.Capabilities, "session_events") {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session events capability was not negotiated"}})
				continue
			}
			subscriptionsMu.Lock()
			hasSessionSubscription := sessionSubscriptionID != ""
			subscriptionsMu.Unlock()
			if hasSessionSubscription {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrBusy, Message: "session event subscription already exists", Retryable: true}})
				continue
			}
			prepared, protocolError := s.prepareSessionSubscription(request)
			if protocolError != nil {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: protocolError})
				continue
			}
			result, _ := json.Marshal(prepared.metadata)
			if err := wire.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
				prepared.cancel()
				return
			}
			subscriptionsMu.Lock()
			sessionSubscriptionID = prepared.metadata.SubscriptionID
			sessionSubscriptionCancel = prepared.cancel
			subscriptionsMu.Unlock()
			subscriptionHandlers.Add(1)
			go func() {
				defer subscriptionHandlers.Done()
				s.streamSessionSubscription(wire, prepared)
				subscriptionsMu.Lock()
				if sessionSubscriptionID == prepared.metadata.SubscriptionID {
					sessionSubscriptionID = ""
					sessionSubscriptionCancel = nil
				}
				subscriptionsMu.Unlock()
			}()
			continue
		}
		if request.Type == "sessions.events_unsubscribe" {
			var body protocol.SessionEventsUnsubscribe
			if negotiated.Role != protocol.RoleDucklord || !hasCapability(negotiated.Capabilities, "session_events") || request.InstanceID != string(s.instanceID) ||
				len(request.SessionID) != 0 || request.RuntimeGeneration != nil || request.OwnershipEpoch != nil || decodeStrict(request.Body, &body) != nil || body.SubscriptionID == "" {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid session event unsubscribe"}})
				continue
			}
			subscriptionsMu.Lock()
			cancel := sessionSubscriptionCancel
			if cancel != nil && body.SubscriptionID == sessionSubscriptionID {
				sessionSubscriptionID = ""
				sessionSubscriptionCancel = nil
			} else {
				cancel = nil
			}
			subscriptionsMu.Unlock()
			if cancel == nil {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrNotFound, Message: "session event subscription not found"}})
				continue
			}
			cancel()
			result, _ := json.Marshal(map[string]bool{"unsubscribed": true})
			if err := wire.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
				return
			}
			continue
		}
		if request.Type == "session.output_subscribe" {
			if !hasCapability(negotiated.Capabilities, "output_subscribe") {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "output subscription capability was not negotiated"}})
				continue
			}
			subscriptionsMu.Lock()
			connectionFull := len(outputSubscriptions) >= maxOutputSubscriptionsPerConnection
			subscriptionsMu.Unlock()
			if connectionFull {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrBusy, Message: "output subscription connection capacity reached", Retryable: true}})
				continue
			}
			prepared, protocolError := s.prepareOutputSubscription(request)
			if protocolError != nil {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: protocolError})
				continue
			}
			result, _ := json.Marshal(prepared.metadata)
			if err := wire.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
				prepared.cancel()
				return
			}
			subscriptionsMu.Lock()
			outputSubscriptions[prepared.metadata.SubscriptionID] = prepared.cancel
			subscriptionsMu.Unlock()
			subscriptionHandlers.Add(1)
			go func() {
				defer subscriptionHandlers.Done()
				s.streamOutputSubscription(wire, prepared)
				subscriptionsMu.Lock()
				delete(outputSubscriptions, prepared.metadata.SubscriptionID)
				subscriptionsMu.Unlock()
			}()
			continue
		}
		if request.Type == "session.output_unsubscribe" {
			if !hasCapability(negotiated.Capabilities, "output_unsubscribe") {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "output unsubscribe capability was not negotiated"}})
				continue
			}
			var body protocol.OutputUnsubscribe
			if request.InstanceID != string(s.instanceID) || len(request.SessionID) != 0 || request.RuntimeGeneration != nil || request.OwnershipEpoch != nil ||
				decodeStrict(request.Body, &body) != nil || body.SubscriptionID == "" {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid output unsubscribe"}})
				continue
			}
			subscriptionsMu.Lock()
			cancel := outputSubscriptions[body.SubscriptionID]
			if cancel != nil {
				delete(outputSubscriptions, body.SubscriptionID)
			}
			subscriptionsMu.Unlock()
			if cancel == nil {
				_ = wire.Write(protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrNotFound, Message: "output subscription not found"}})
				continue
			}
			cancel()
			result, _ := json.Marshal(map[string]bool{"unsubscribed": true})
			if err := wire.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
				return
			}
			continue
		}
		select {
		case routeRequests <- request:
		case <-s.done:
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
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: []string{"supervisor_recovery", "output_publish", "terminal_attention", "agent_activity"}}
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
	if err := decodeStrict(completeRequest.Body, &complete); err != nil || complete.ChallengeID != challenge.ID || len(complete.LaunchFailure) > 1024 {
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
	if complete.LaunchFailure != "" {
		if err := runtimeConn.arm(); err != nil {
			s.registry.Disconnect(identity)
			return
		}
		if err := s.state.MarkRuntimeExited(context.Background(), identity.SessionID, identity.Generation, false, complete.LaunchFailure); err != nil {
			s.registry.Disconnect(identity)
			writeSupervisorError(codec, completeRequest.ID, protocol.ErrStaleGeneration, "runtime state changed before launch failure")
			return
		}
		s.requestRetainedOutputSweep()
		s.registry.Disconnect(identity)
		result, _ := json.Marshal(map[string]bool{"recorded": true})
		_ = codec.Write(protocol.Response{ID: completeRequest.ID, Result: result})
		return
	}
	output := s.activateOutput(identity)
	defer func() {
		s.deactivateOutput(identity)
		s.registry.Disconnect(identity)
		s.forgetAttention(identity)
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
			start, end := output.Bounds()
			recoveredSeed := published.Gap && start == 0 && end == 0
			if !recoveredSeed && published.Offset != end {
				writeSupervisorError(codec, request.ID, protocol.ErrStaleGeneration, "output offset is not contiguous")
				continue
			}
			var frame duckruntime.OutputFrame
			if published.Gap {
				var publishErr error
				frame, publishErr = output.PublishRecovered(published.Offset, published.Data, true)
				if publishErr != nil {
					writeSupervisorError(codec, request.ID, protocol.ErrStaleGeneration, "recovered output is invalid")
					continue
				}
			} else {
				frame = output.Publish(published.Data)
			}
			result, _ = json.Marshal(protocol.SupervisorOutputAck{Offset: frame.Offset, Length: uint64(len(frame.Data))})
		case "supervisor.agent_event":
			var event protocol.SupervisorAgentEvent
			if err := decodeStrict(request.Body, &event); err != nil {
				writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "invalid supervisor agent event")
				continue
			}
			if protocolError := s.applySupervisorAgentEvent(identity, event); protocolError != nil {
				_ = codec.Write(protocol.Response{ID: request.ID, Error: protocolError})
				continue
			}
			task, _ := s.service.GetManagedTask(context.Background(), identity.SessionID, event.TaskID)
			result, _ = json.Marshal(protocol.SupervisorAgentEventReceipt{Recorded: true, Acknowledged: task.AckedEventSeq >= event.Sequence})
		case "supervisor.exited":
			var exited protocol.SupervisorExit
			if err := decodeStrict(request.Body, &exited); err != nil || len(exited.Reason) > 1024 {
				writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "invalid supervisor exit report")
				continue
			}
			if err := s.state.MarkRuntimeExited(context.Background(), identity.SessionID, identity.Generation, exited.Success, exited.Reason); err != nil {
				writeSupervisorError(codec, request.ID, protocol.ErrStaleGeneration, "runtime state changed before exit")
				continue
			}
			s.requestRetainedOutputSweep()
			result, _ = json.Marshal(map[string]bool{"recorded": true})
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
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: []string{"runtime_control", "resize_barrier"}}
	negotiated, protocolError := protocol.Negotiate(local, remote)
	if protocolError != nil || !hasCapability(negotiated.Capabilities, "runtime_control") || !hasCapability(negotiated.Capabilities, "resize_barrier") {
		if protocolError == nil {
			protocolError = &protocol.Error{Code: protocol.ErrIncompatible, Message: "runtime control and resize barrier capabilities are required"}
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
	if err != nil || (session.Status != model.StatusRecovering && session.Status != model.StatusRunning) || session.RuntimeGeneration != generation || len(session.RecoveryPublicKey) != ed25519.PublicKeySize {
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
		removed := false
		s.controlMu.Lock()
		if s.controls[sessionID] == peer {
			delete(s.controls, sessionID)
			removed = true
		}
		s.controlMu.Unlock()
		if removed {
			_ = s.state.MarkRuntimeDisconnected(context.Background(), identity.SessionID, identity.Generation)
		}
	}()
	if err := s.state.MarkRuntimeConnected(context.Background(), identity.SessionID, identity.Generation); err != nil {
		writeSupervisorError(codec, complete.ID, protocol.ErrStaleGeneration, "runtime state changed during control registration")
		return
	}
	ready, _ := json.Marshal(protocol.SupervisorControlReady{SessionID: string(sessionID), RuntimeGeneration: generation})
	if err := codec.Write(protocol.Response{ID: complete.ID, Result: ready}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
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
			isInput := call.request.Type == "supervisor.input"
			var input protocol.SupervisorInput
			if isInput {
				if err := json.Unmarshal(call.request.Body, &input); err != nil {
					call.result <- protocol.Response{ID: call.request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid runtime input command"}}
					continue
				}
			}
			var response protocol.Response
			for attempt := 0; ; attempt++ {
				if isInput {
					input.Sequence = s.nextInputSequence(identity)
					call.request.Body, _ = json.Marshal(input)
				}
				if err := codec.Write(call.request); err != nil {
					call.result <- protocol.Response{ID: call.request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "runtime control disconnected", Retryable: true}}
					return
				}
				response = protocol.Response{}
				readErr := codec.Read(&response)
				validationErr := response.Validate()
				if readErr != nil || validationErr != nil || response.ID != call.request.ID {
					log.Printf("ducklion: runtime control response failed for %s: read=%v validate=%v response_id=%q request_id=%q", identity.SessionID, readErr, validationErr, response.ID, call.request.ID)
					call.result <- protocol.Response{ID: call.request.ID, Error: &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "runtime control response failed", Retryable: true}}
					return
				}
				if !isInput || response.Error == nil || response.Error.Message != supervisor.ErrInputSequence.Error() ||
					attempt+1 >= maxInputSequenceRecoveryAttempts {
					break
				}
			}
			call.result <- response
		}
	}
}

func (s *Server) handleSupervisorActivity(conn *net.UnixConn, codec *bridge.Codec, remote protocol.Handshake) {
	sessionID, err := model.ParseSessionID(remote.Principal)
	if err != nil || string(sessionID) != remote.Principal {
		_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "activity principal must be the canonical session ID"}})
		return
	}
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: []string{"terminal_attention", "agent_activity"}}
	negotiated, protocolError := protocol.Negotiate(local, remote)
	if protocolError != nil || !hasCapability(negotiated.Capabilities, "terminal_attention") {
		if protocolError == nil {
			protocolError = &protocol.Error{Code: protocol.ErrIncompatible, Message: "terminal attention capability is required"}
		}
		_ = codec.Write(protocol.HandshakeResponse{Error: protocolError})
		return
	}
	if err := codec.Write(protocol.HandshakeResponse{Handshake: &negotiated}); err != nil {
		return
	}
	var begin protocol.Request
	if err := codec.Read(&begin); err != nil || begin.Validate() != nil || begin.Type != "supervisor.activity_begin" || begin.SessionID != string(sessionID) || begin.RuntimeGeneration == nil || len(begin.Body) != 0 {
		writeSupervisorError(codec, begin.ID, protocol.ErrInvalidArgument, "invalid activity authentication request")
		return
	}
	generation := *begin.RuntimeGeneration
	session, err := s.state.GetSession(context.Background(), sessionID)
	if err != nil || (session.Status != model.StatusRecovering && session.Status != model.StatusRunning) || session.RuntimeGeneration != generation || len(session.RecoveryPublicKey) != ed25519.PublicKeySize {
		writeSupervisorError(codec, begin.ID, protocol.ErrAdapterUnhealthy, "runtime activity is unavailable")
		return
	}
	identity, ok := s.registry.Current(sessionID, generation)
	if !ok {
		writeSupervisorError(codec, begin.ID, protocol.ErrAdapterUnhealthy, "runtime activity is unavailable")
		return
	}
	nonce, err := model.NewRecoveryNonce()
	if err != nil {
		writeSupervisorError(codec, begin.ID, protocol.ErrInternal, "could not create activity challenge")
		return
	}
	challengeID := uuid.NewString()
	challengeJSON, _ := json.Marshal(protocol.SupervisorChallenge{ChallengeID: challengeID, Nonce: nonce, InstanceID: string(s.instanceID)})
	if err := codec.Write(protocol.Response{ID: begin.ID, Result: challengeJSON}); err != nil {
		return
	}
	var complete protocol.Request
	if err := codec.Read(&complete); err != nil || complete.Validate() != nil || complete.Type != "supervisor.activity_complete" || complete.SessionID != string(sessionID) ||
		complete.InstanceID != string(s.instanceID) || complete.RuntimeGeneration == nil || *complete.RuntimeGeneration != generation {
		writeSupervisorError(codec, complete.ID, protocol.ErrInvalidArgument, "invalid activity proof request")
		return
	}
	var proof protocol.SupervisorRegisterComplete
	if err := decodeStrict(complete.Body, &proof); err != nil || proof.ChallengeID != challengeID ||
		!model.VerifyRecoveryProof(ed25519.PublicKey(session.RecoveryPublicKey), s.instanceID, sessionID, generation, nonce, proof.Proof, uint16(negotiated.Major), uint16(negotiated.Minor)) {
		writeSupervisorError(codec, complete.ID, protocol.ErrAdapterUnhealthy, "runtime activity authentication failed")
		return
	}
	if current, currentOK := s.registry.Current(sessionID, generation); !currentOK || current != identity {
		writeSupervisorError(codec, complete.ID, protocol.ErrStaleGeneration, "runtime changed during activity authentication")
		return
	}
	select {
	case s.activitySlots <- struct{}{}:
		defer func() { <-s.activitySlots }()
	default:
		_ = codec.Write(protocol.Response{ID: complete.ID, Error: &protocol.Error{Code: protocol.ErrBusy, Message: "runtime activity connection limit reached", Retryable: true}})
		return
	}
	ready, _ := json.Marshal(protocol.SupervisorControlReady{SessionID: string(sessionID), RuntimeGeneration: generation})
	if err := codec.Write(protocol.Response{ID: complete.ID, Result: ready}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	for {
		var request protocol.Request
		if err := codec.Read(&request); err != nil {
			return
		}
		if request.Validate() != nil || request.Type != "supervisor.terminal_attention" || request.RuntimeGeneration == nil {
			writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "invalid terminal attention request")
			continue
		}
		if request.InstanceID != string(s.instanceID) || request.SessionID != string(sessionID) || *request.RuntimeGeneration != generation || !s.registry.IsCurrent(identity) {
			writeSupervisorError(codec, request.ID, protocol.ErrStaleGeneration, "runtime activity identity changed")
			continue
		}
		var attention protocol.SupervisorActivity
		outputEnd, available := s.runtimeOutputEnd(identity)
		if err := decodeStrict(request.Body, &attention); err != nil || !available || attention.OutputOffset > outputEnd {
			writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "supervisor activity has an invalid output boundary")
			continue
		}
		category := attention.Category
		if category == "" {
			category = model.NotificationTerminalAttention
		}
		if category != model.NotificationTerminalAttention && category != model.NotificationTaskCompleted && category != model.NotificationTaskFailed {
			writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "invalid supervisor activity category")
			continue
		}
		if category != model.NotificationTerminalAttention && !hasCapability(negotiated.Capabilities, "agent_activity") {
			writeSupervisorError(codec, request.ID, protocol.ErrIncompatible, "agent activity capability was not negotiated")
			continue
		}
		if category != model.NotificationTerminalAttention {
			if attention.EventID == 0 {
				writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "agent activity requires an event id")
				continue
			}
			sequence, advanced, err := s.state.RecordAgentActivity(context.Background(), identity.SessionID, category, identity.Generation, attention.EventID, attention.OutputOffset)
			if err != nil {
				writeSupervisorError(codec, request.ID, protocol.ErrInternal, "could not record supervisor activity")
				continue
			}
			result, _ := json.Marshal(protocol.SupervisorActivityReceipt{Category: category, Sequence: sequence, Advanced: advanced, OutputOffset: attention.OutputOffset, EventID: attention.EventID})
			if err := codec.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
				return
			}
			continue
		}
		if attention.OutputOffset == 0 || attention.EventID != 0 {
			writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "terminal attention requires a published output offset")
			continue
		}
		sequence, advanced, cached := s.coalescedAttention(identity, attention.OutputOffset)
		if !cached {
			sequence, advanced, err = s.state.RecordActivityAtOffset(context.Background(), identity.SessionID, model.NotificationTerminalAttention, time.Second, identity.Generation, attention.OutputOffset)
			if err != nil {
				writeSupervisorError(codec, request.ID, protocol.ErrInternal, "could not record terminal attention")
				continue
			}
			s.rememberAttention(identity, attention.OutputOffset, sequence)
		}
		result, _ := json.Marshal(protocol.SupervisorActivityReceipt{Category: model.NotificationTerminalAttention, Sequence: sequence, Advanced: advanced, OutputOffset: attention.OutputOffset})
		if err := codec.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
			return
		}
	}
}

func (s *Server) runtimeOutputEnd(identity duckruntime.RuntimeIdentity) (uint64, bool) {
	s.outputMu.Lock()
	registered, ok := s.outputs[identity.SessionID]
	s.outputMu.Unlock()
	if !ok || registered.identity != identity {
		return 0, false
	}
	_, end := registered.hub.Bounds()
	return end, true
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

func (s *Server) prepareOutputSubscription(request protocol.Request) (*preparedOutputSubscription, *protocol.Error) {
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil || request.InstanceID != string(s.instanceID) || request.RuntimeGeneration == nil || len(request.Body) == 0 {
		return nil, &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid output subscription"}
	}
	var subscription protocol.OutputSubscribe
	if err := decodeStrict(request.Body, &subscription); err != nil || subscription.TailBytes > 4<<20 || subscription.TailBytes > 0 && subscription.Offset != 0 {
		return nil, &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid output subscription"}
	}
	s.outputMu.Lock()
	current, ok := s.outputs[sessionID]
	s.outputMu.Unlock()
	if !ok {
		return s.prepareRetainedOutputSubscription(sessionID, *request.RuntimeGeneration, subscription)
	}
	if current.identity.Generation != *request.RuntimeGeneration {
		return nil, &protocol.Error{Code: protocol.ErrStaleGeneration, Message: "runtime generation changed"}
	}
	releaseQuota, quotaError := s.reserveOutputSubscription(sessionID)
	if quotaError != nil {
		return nil, quotaError
	}
	offset := subscription.Offset
	if subscription.TailBytes > 0 {
		start, end := current.hub.Bounds()
		offset = start
		if end > subscription.TailBytes && end-subscription.TailBytes > start {
			offset = end - subscription.TailBytes
		}
	}
	replay, stream, cancel, err := current.hub.Subscribe(offset, 32)
	if err != nil {
		releaseQuota()
		return nil, &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}
	}
	var cancelOnce sync.Once
	cancelWithQuota := func() {
		cancelOnce.Do(func() {
			cancel()
			releaseQuota()
		})
	}
	_, end := current.hub.Bounds()
	subscriptionID := uuid.NewString()
	metadata := protocol.OutputSubscribeResult{SubscriptionID: subscriptionID, RuntimeID: current.identity.LeaseID, InstanceID: string(s.instanceID), SessionID: string(sessionID),
		RuntimeGeneration: current.identity.Generation, StartOffset: replay.Offset, EndOffset: end, Gap: replay.Gap}
	return &preparedOutputSubscription{metadata: metadata, identity: current.identity, replay: replay, stream: stream, cancel: cancelWithQuota, hub: current.hub}, nil
}

func (s *Server) prepareRetainedOutputSubscription(sessionID model.SessionID, generation uint64, subscription protocol.OutputSubscribe) (*preparedOutputSubscription, *protocol.Error) {
	session, err := s.state.GetSession(context.Background(), sessionID)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrNotFound, Message: "session does not exist"}
	}
	if session.RuntimeGeneration != generation {
		return nil, &protocol.Error{Code: protocol.ErrStaleGeneration, Message: "runtime generation changed"}
	}
	if session.Status != model.StatusStopped {
		return nil, &protocol.Error{Code: protocol.ErrAdapterUnhealthy, Message: "session runtime is reconnecting", Retryable: true}
	}
	snapshot, err := supervisor.ReadRetainedOutput(filepath.Join(s.root, "sessions", string(sessionID)), sessionID, generation)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrOutputUnavailable, Message: "retained PTY output is unavailable or has expired"}
	}
	if !snapshot.UpdatedAt.IsZero() && time.Since(snapshot.UpdatedAt) > s.retainedOutputTTL {
		_ = removeRetainedOutputPair(supervisor.RetainedOutputPath(filepath.Join(s.root, "sessions", string(sessionID)), generation))
		return nil, &protocol.Error{Code: protocol.ErrOutputUnavailable, Message: "retained PTY output has expired"}
	}
	releaseQuota, quotaError := s.reserveOutputSubscription(sessionID)
	if quotaError != nil {
		return nil, quotaError
	}
	hub := duckruntime.NewOutputHub(supervisor.RetainedOutputCapacity)
	if _, err := hub.PublishRecovered(snapshot.StartOffset, snapshot.Data, snapshot.StartOffset != 0); err != nil {
		releaseQuota()
		return nil, &protocol.Error{Code: protocol.ErrInternal, Message: "retained PTY output could not be restored"}
	}
	offset := subscription.Offset
	if subscription.TailBytes > 0 {
		offset = snapshot.StartOffset
		if snapshot.EndOffset > subscription.TailBytes && snapshot.EndOffset-subscription.TailBytes > offset {
			offset = snapshot.EndOffset - subscription.TailBytes
		}
	}
	replay, stream, cancel, err := hub.Subscribe(offset, 1)
	if err != nil {
		releaseQuota()
		return nil, &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}
	}
	hub.Close()
	var once sync.Once
	cancelWithQuota := func() { once.Do(func() { cancel(); releaseQuota() }) }
	identity := duckruntime.RuntimeIdentity{SessionID: sessionID, Generation: generation, LeaseID: "retained-" + uuid.NewString()}
	metadata := protocol.OutputSubscribeResult{SubscriptionID: uuid.NewString(), RuntimeID: identity.LeaseID, InstanceID: string(s.instanceID), SessionID: string(sessionID),
		RuntimeGeneration: generation, StartOffset: replay.Offset, EndOffset: snapshot.EndOffset, Gap: replay.Gap}
	return &preparedOutputSubscription{metadata: metadata, identity: identity, replay: replay, stream: stream, cancel: cancelWithQuota, hub: hub}, nil
}

func (s *Server) cleanupExpiredRetainedOutput(ctx context.Context, now time.Time) error {
	sessions, err := s.state.ListSessions(ctx)
	if err != nil {
		return err
	}
	type candidate struct {
		path      string
		updatedAt time.Time
		size      int64
		removable bool
	}
	var candidates []candidate
	var total int64
	var cleanupErrors []error
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(cleanupErrors, err)...)
		}
		dir := filepath.Join(s.root, "sessions", string(session.ID))
		entries, readErr := os.ReadDir(dir)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			cleanupErrors = append(cleanupErrors, readErr)
			continue
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(cleanupErrors, err)...)
			}
			if strings.HasPrefix(entry.Name(), ".output-compact-") || strings.HasPrefix(entry.Name(), ".output-meta-") {
				path := filepath.Join(dir, entry.Name())
				info, statErr := supervisor.RetainedArtifactInfo(path)
				if statErr != nil {
					cleanupErrors = append(cleanupErrors, statErr)
					continue
				}
				total += info.Size()
				// A live writer may briefly own this path. Leave a grace window;
				// crash leftovers enter normal TTL/quota eviction afterward.
				if now.Sub(info.ModTime()) >= 5*time.Minute {
					candidates = append(candidates, candidate{path: path, updatedAt: info.ModTime(), size: info.Size(), removable: true})
					if now.Sub(info.ModTime()) > s.retainedOutputTTL {
						if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
							cleanupErrors = append(cleanupErrors, err)
						} else {
							total -= info.Size()
							candidates[len(candidates)-1].removable = false
						}
					}
				}
				continue
			}
			generation, ok := supervisor.ParseRetainedOutputGeneration(entry.Name())
			if !ok || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				continue
			}
			active := (session.Status == model.StatusRunning || session.Status == model.StatusRecovering) && generation == session.RuntimeGeneration
			snapshot, snapshotErr := supervisor.RetainedOutputInfo(dir, session.ID, generation)
			updatedAt := snapshot.UpdatedAt
			size := int64(snapshot.EndOffset - snapshot.StartOffset)
			if snapshotErr != nil {
				info, statErr := supervisor.RetainedOutputArtifactInfo(dir, generation)
				if statErr != nil {
					cleanupErrors = append(cleanupErrors, statErr)
					continue
				}
				updatedAt, size = info.ModTime(), info.Size()
			}
			total += size
			path := supervisor.RetainedOutputPath(dir, generation)
			if active {
				continue
			}
			if updatedAt.IsZero() || now.Sub(updatedAt) <= s.retainedOutputTTL {
				candidates = append(candidates, candidate{path: path, updatedAt: updatedAt, size: size, removable: true})
				continue
			}
			if err := removeRetainedOutputPair(path); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			} else {
				total -= size
			}
		}
	}
	if total > maxRetainedOutputBytesGlobal {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].updatedAt.Before(candidates[j].updatedAt) })
		for _, item := range candidates {
			if total <= maxRetainedOutputBytesGlobal {
				break
			}
			if item.removable {
				if err := removeRetainedOutputPair(item.path); err != nil {
					cleanupErrors = append(cleanupErrors, err)
					continue
				}
				total -= item.size
			}
		}
	}
	return errors.Join(cleanupErrors...)
}

func removeRetainedOutputPair(path string) error {
	var errs []error
	for _, artifact := range []string{path, path + ".json"} {
		if err := os.Remove(artifact); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Server) requestRetainedOutputSweep() {
	select {
	case s.retainedOutputSweep <- struct{}{}:
	default:
	}
}

func (s *Server) runRetainedOutputSweeper(ctx context.Context) {
	defer s.retainedOutputWG.Done()
	interval := time.Hour
	if half := s.retainedOutputTTL / 2; half > 0 && half < interval {
		interval = half
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.cleanupExpiredRetainedOutput(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[ducklion] retained PTY cleanup: %v", err)
			}
		case <-s.retainedOutputSweep:
			if err := s.cleanupExpiredRetainedOutput(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[ducklion] retained PTY cleanup: %v", err)
			}
		}
	}
}

func (s *Server) reserveOutputSubscription(sessionID model.SessionID) (func(), *protocol.Error) {
	s.outputMu.Lock()
	if s.outputSubscriptions >= maxOutputSubscriptionsGlobal || s.outputSubscriptionsBySession[sessionID] >= maxOutputSubscriptionsPerSession {
		s.outputMu.Unlock()
		return nil, &protocol.Error{Code: protocol.ErrBusy, Message: "output subscription capacity reached", Retryable: true}
	}
	s.outputSubscriptions++
	s.outputSubscriptionsBySession[sessionID]++
	s.outputMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.outputMu.Lock()
			s.outputSubscriptions--
			s.outputSubscriptionsBySession[sessionID]--
			if s.outputSubscriptionsBySession[sessionID] == 0 {
				delete(s.outputSubscriptionsBySession, sessionID)
			}
			s.outputMu.Unlock()
		})
	}, nil
}

func (s *Server) prepareSessionSubscription(request protocol.Request) (*preparedSessionSubscription, *protocol.Error) {
	var body protocol.SessionEventsSubscribe
	if request.InstanceID != string(s.instanceID) {
		return nil, &protocol.Error{Code: protocol.ErrNotFound, Message: "Ducklion instance does not match"}
	}
	if len(request.SessionID) != 0 || request.RuntimeGeneration != nil || request.OwnershipEpoch != nil ||
		decodeStrict(request.Body, &body) != nil {
		return nil, &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid session event subscription"}
	}
	release, quotaError := s.reserveSessionEventSubscription()
	if quotaError != nil {
		return nil, quotaError
	}
	snapshot, err := s.state.SessionSnapshot(context.Background())
	if err != nil {
		release()
		return nil, &protocol.Error{Code: protocol.ErrInternal, Message: "could not read session snapshot", Retryable: true}
	}
	if body.AfterRevision > snapshot.Revision {
		release()
		return nil, &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session event revision is ahead of Ducklion"}
	}
	ctx, cancelContext := context.WithCancel(context.Background())
	var cancelOnce sync.Once
	cancel := func() { cancelOnce.Do(func() { cancelContext(); release() }) }
	metadata := protocol.SessionSnapshotSubscribeResult{SubscriptionID: uuid.NewString(), InstanceID: string(s.instanceID), SnapshotRevision: snapshot.Revision,
		EarliestRevision: snapshot.EarliestRevision, Sessions: s.summariesFor(snapshot.Sessions)}
	if body.AfterRevision > 0 && snapshot.EarliestRevision > 0 && body.AfterRevision+1 < snapshot.EarliestRevision {
		metadata.Gap = true
	}
	return &preparedSessionSubscription{metadata: metadata, cursor: snapshot.Revision, ctx: ctx, cancel: cancel}, nil
}

func (s *Server) reserveSessionEventSubscription() (func(), *protocol.Error) {
	s.sessionEventMu.Lock()
	if s.sessionEventSubscriptions >= maxSessionEventSubscriptionsGlobal {
		s.sessionEventMu.Unlock()
		return nil, &protocol.Error{Code: protocol.ErrBusy, Message: "session event subscription capacity reached", Retryable: true}
	}
	s.sessionEventSubscriptions++
	s.sessionEventMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.sessionEventMu.Lock()
			s.sessionEventSubscriptions--
			s.sessionEventMu.Unlock()
		})
	}, nil
}

func (s *Server) streamSessionSubscription(wire interface{ Write(any) error }, prepared *preparedSessionSubscription) {
	defer prepared.cancel()
	cursor := prepared.cursor
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		revisions, earliest, latest, err := s.state.SessionRevisionsAfter(prepared.ctx, cursor, 256)
		if err != nil {
			_ = wire.Write(protocol.SessionRevisionEvent{Type: "session_events_end", SubscriptionID: prepared.metadata.SubscriptionID,
				InstanceID: string(s.instanceID), Revision: cursor, Reason: "journal_error"})
			return
		}
		if earliest > 0 && cursor+1 < earliest && latest > cursor {
			_ = wire.Write(protocol.SessionRevisionEvent{Type: "session_events_end", SubscriptionID: prepared.metadata.SubscriptionID,
				InstanceID: string(s.instanceID), Revision: cursor, Reason: "journal_gap"})
			return
		}
		for _, revision := range revisions {
			if revision.Revision != cursor+1 {
				_ = wire.Write(protocol.SessionRevisionEvent{Type: "session_events_end", SubscriptionID: prepared.metadata.SubscriptionID,
					InstanceID: string(s.instanceID), Revision: cursor, Reason: "journal_gap"})
				return
			}
			event := protocol.SessionRevisionEvent{Type: "session_revision", SubscriptionID: prepared.metadata.SubscriptionID, InstanceID: string(s.instanceID),
				Revision: revision.Revision, SessionID: string(revision.SessionID), Change: revision.Change,
				ActivityCategory: revision.ActivityCategory, ActivitySequence: revision.ActivitySequence}
			if err := wire.Write(event); err != nil {
				return
			}
			cursor = revision.Revision
		}
		select {
		case <-prepared.ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
		}
	}
}

func attentionRateKey(identity duckruntime.RuntimeIdentity) string {
	return string(identity.SessionID) + "\x00" + fmt.Sprint(identity.Generation)
}

func (s *Server) coalescedAttention(identity duckruntime.RuntimeIdentity, offset uint64) (uint64, bool, bool) {
	s.attentionMu.Lock()
	defer s.attentionMu.Unlock()
	rate, ok := s.attentionRates[attentionRateKey(identity)]
	if !ok || rate.sequence == 0 {
		return 0, false, false
	}
	if offset <= rate.offset {
		return rate.sequence, false, true
	}
	if time.Since(rate.at) < time.Second {
		rate.offset = offset
		s.attentionRates[attentionRateKey(identity)] = rate
		return rate.sequence, false, true
	}
	return 0, false, false
}

func (s *Server) rememberAttention(identity duckruntime.RuntimeIdentity, offset, sequence uint64) {
	s.attentionMu.Lock()
	if s.attentionRates == nil {
		s.attentionRates = make(map[string]attentionRate)
	}
	s.attentionRates[attentionRateKey(identity)] = attentionRate{offset: offset, sequence: sequence, at: time.Now()}
	s.attentionMu.Unlock()
}

func (s *Server) forgetAttention(identity duckruntime.RuntimeIdentity) {
	s.attentionMu.Lock()
	delete(s.attentionRates, attentionRateKey(identity))
	s.attentionMu.Unlock()
}

func (s *Server) summariesFor(projections []store.SessionProjection) []protocol.SessionSummary {
	summaries := make([]protocol.SessionSummary, 0, len(projections))
	for _, projection := range projections {
		session := projection.Session
		summary := protocol.SessionSummary{SessionID: string(session.ID), Handle: session.Handle, Kind: session.Kind, AgentType: session.AgentType,
			ProjectName: session.ProjectName, CWD: session.CWD, Status: session.Status, Writer: session.Writer, OwnershipEpoch: session.OwnershipEpoch, RuntimeGeneration: session.RuntimeGeneration,
			TaskState: session.TaskState, AdapterState: session.AdapterState, ExitSuccess: session.ExitSuccess, ExitReason: session.ExitReason,
			ChannelHandle: projection.ChannelHandle, ManagementHandle: projection.ManagementHandle, ActivitySequences: projection.ActivitySequences}
		if session.Status == model.StatusStopped {
			if retained, err := supervisor.RetainedOutputInfo(filepath.Join(s.root, "sessions", string(session.ID)), session.ID, session.RuntimeGeneration); err == nil &&
				time.Since(retained.UpdatedAt) <= s.retainedOutputTTL {
				summary.RetainedOutputBytes = int64(retained.EndOffset - retained.StartOffset)
				summary.RetainedOutputUntilMS = retained.UpdatedAt.Add(s.retainedOutputTTL).UnixMilli()
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func (s *Server) streamOutputSubscription(codec interface{ Write(any) error }, prepared *preparedOutputSubscription) {
	defer prepared.cancel()
	if err := writeOutputEvents(codec, prepared.metadata.SubscriptionID, s.instanceID, prepared.identity, prepared.replay); err != nil {
		return
	}
	nextOffset := prepared.replay.Offset + uint64(len(prepared.replay.Data))
	for frame := range prepared.stream {
		if err := writeOutputEvents(codec, prepared.metadata.SubscriptionID, s.instanceID, prepared.identity, frame); err != nil {
			return
		}
		nextOffset = frame.Offset + uint64(len(frame.Data))
	}
	_, finalEnd := prepared.hub.Bounds()
	reason := "runtime_disconnected"
	if nextOffset < finalEnd {
		reason = "subscriber_lag"
	}
	_ = codec.Write(protocol.OutputEvent{Type: "output_end", SubscriptionID: prepared.metadata.SubscriptionID, RuntimeID: prepared.identity.LeaseID,
		InstanceID: string(s.instanceID), SessionID: string(prepared.identity.SessionID), RuntimeGeneration: prepared.identity.Generation,
		Frame: protocol.OutputFrame{Offset: nextOffset}, Reason: reason})
}

func writeOutputEvents(codec interface{ Write(any) error }, subscriptionID string, instanceID model.InstanceID, identity duckruntime.RuntimeIdentity, frame duckruntime.OutputFrame) error {
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

func (s *Server) route(request protocol.Request, capabilities []string, role protocol.PeerRole, principal string) protocol.Response {
	if request.InstanceID != "" && request.InstanceID != string(s.instanceID) {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrNotFound, Message: "Ducklion instance does not match"}}
	}
	switch request.Type {
	case "status":
		if !hasCapability(capabilities, "status") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "status capability was not negotiated"}}
		}
		result, _ := json.Marshal(map[string]any{"instance_id": s.instanceID, "protocol_major": protocol.Major, "protocol_minor": protocol.Minor,
			"pty_log_retention_days": int(s.retainedOutputTTL / (24 * time.Hour))})
		return protocol.Response{ID: request.ID, Result: result}
	case "sessions.list":
		if !hasCapability(capabilities, "sessions_list") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session list capability was not negotiated"}}
		}
		snapshot, err := s.state.SessionSnapshot(context.Background())
		if err != nil {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not list sessions", Retryable: true}}
		}
		result, _ := json.Marshal(s.summariesFor(snapshot.Sessions))
		return protocol.Response{ID: request.ID, Result: result}
	case "session.create":
		if role == protocol.RoleDuckwayCC && !hasCapability(capabilities, "session_create_agent") || role != protocol.RoleDuckwayCC && !hasCapability(capabilities, "session_create") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session create capability was not negotiated"}}
		}
		return s.routeSessionCreate(request, role, principal)
	case "session.stop":
		if !hasCapability(capabilities, "session_stop") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session stop capability was not negotiated"}}
		}
		return s.routeSessionStop(request, role, principal)
	case "session.destroy":
		if !hasCapability(capabilities, "session_destroy") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session destroy capability was not negotiated"}}
		}
		return s.routeSessionDestroy(request, role, principal)
	case "session.lifecycle":
		if !hasCapability(capabilities, "session_lifecycle") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session lifecycle capability was not negotiated"}}
		}
		return s.routeSessionLifecycle(request, role, principal)
	case "session.yield":
		if !hasCapability(capabilities, "session_yield") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session yield capability was not negotiated"}}
		}
		return s.routeSessionYield(request, role, principal)
	case "session.task_begin", "session.task_complete":
		if role != protocol.RoleDuckwayCC || !hasCapability(capabilities, "session_task") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "session task capability was not negotiated"}}
		}
		return s.routeSessionTask(request, principal)
	case "session.agent_submit":
		if role != protocol.RoleDuckwayCC || !hasCapability(capabilities, "agent_task") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "agent task capability was not negotiated"}}
		}
		return s.routeAgentTaskSubmit(request, principal)
	case "session.agent_events":
		if role != protocol.RoleDuckwayCC || !hasCapability(capabilities, "agent_task") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "agent task capability was not negotiated"}}
		}
		return s.routeAgentTaskEvents(request, principal)
	case "session.agent_event_ack":
		if role != protocol.RoleDuckwayCC || !hasCapability(capabilities, "agent_task") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrNotOwner, Message: "agent event acknowledgement requires a CC connection"}}
		}
		return s.routeAgentTaskEventAck(request, principal)
	case "session.bind_discord":
		if role != protocol.RoleDuckwayCC || !hasCapability(capabilities, "discord_binding") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "Discord binding capability was not negotiated"}}
		}
		return s.routeSessionBind(request, principal)
	case "session.unbind_discord":
		if role != protocol.RoleDuckwayCC || !hasCapability(capabilities, "discord_unbind") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "Discord unbind capability was not negotiated"}}
		}
		return s.routeSessionUnbind(request, principal)
	case "binding.current":
		if role != protocol.RoleDuckwayCC || !hasCapability(capabilities, "discord_binding") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "Discord binding capability was not negotiated"}}
		}
		return s.routeCurrentBinding(request, principal)
	case "binding.by_session":
		if role != protocol.RoleDuckwayCC || !hasCapability(capabilities, "discord_binding") {
			return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "Discord binding capability was not negotiated"}}
		}
		return s.routeBindingBySession(request)
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

func (s *Server) sessionOperation(id model.SessionID) *sync.Mutex {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if s.operations == nil {
		s.operations = make(map[model.SessionID]*sync.Mutex)
	}
	if s.operations[id] == nil {
		s.operations[id] = &sync.Mutex{}
	}
	return s.operations[id]
}

func (s *Server) authorizeTerminalControl(request protocol.Request, principal string) (model.Session, model.Owner, *protocol.Error) {
	return s.authorizeOwnerControl(request, protocol.RoleDucklord, principal)
}

func (s *Server) authorizeOwnerControl(request protocol.Request, role protocol.PeerRole, principal string) (model.Session, model.Owner, *protocol.Error) {
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
	ownerKind := model.OwnerTerminal
	if role == protocol.RoleDuckwayCC {
		ownerKind = model.OwnerCC
	}
	owner := model.Owner{Kind: ownerKind, ID: principal}
	if session.Kind == model.KindAgent {
		if err := session.AuthorizeAgentInput(owner, *request.OwnershipEpoch, *request.RuntimeGeneration); err != nil {
			epoch, generation := session.OwnershipEpoch, session.RuntimeGeneration
			return model.Session{}, model.Owner{}, &protocol.Error{Code: serviceMapError(err), Message: err.Error(), OwnershipEpoch: &epoch, RuntimeGeneration: &generation}
		}
	} else if role == protocol.RoleDuckwayCC {
		return model.Session{}, model.Owner{}, &protocol.Error{Code: protocol.ErrNotOwner, Message: "Discord CC cannot manage shell sessions"}
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
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}}
	}
	operation := s.sessionOperation(sessionID)
	operation.Lock()
	defer operation.Unlock()
	session, owner, protocolError := s.authorizeTerminalControl(request, principal)
	if protocolError != nil {
		return protocol.Response{ID: request.ID, Error: protocolError}
	}
	if pending, err := s.state.GetPendingLifecycle(context.Background(), session.ID); err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not inspect lifecycle drain", Retryable: true}}
	} else if pending != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrDraining, Message: "a lifecycle drain prevents PTY input"}}
	}
	if session.Kind == model.KindAgent {
		if response := s.syncRuntimeOwnership(session); response.Error != nil {
			response.ID = request.ID
			return response
		}
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

func (s *Server) syncRuntimeOwnership(session model.Session) protocol.Response {
	epoch, generation := session.OwnershipEpoch, session.RuntimeGeneration
	forwarded := protocol.Request{ID: uuid.NewString(), Type: "supervisor.ownership", InstanceID: string(s.instanceID), SessionID: string(session.ID),
		OwnershipEpoch: &epoch, RuntimeGeneration: &generation}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.callControl(ctx, session.ID, forwarded)
}

func (s *Server) routeSessionResize(request protocol.Request, principal string) protocol.Response {
	var resize protocol.SessionResize
	if err := decodeStrict(request.Body, &resize); err != nil || resize.Rows < 5 || resize.Rows > 200 || resize.Cols < 20 || resize.Cols > 500 {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "resize is outside supported bounds"}}
	}
	sessionID, err := model.ParseSessionID(request.SessionID)
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: err.Error()}}
	}
	operation := s.sessionOperation(sessionID)
	operation.Lock()
	defer operation.Unlock()
	session, _, protocolError := s.authorizeTerminalControl(request, principal)
	if protocolError != nil {
		return protocol.Response{ID: request.ID, Error: protocolError}
	}
	if pending, err := s.state.GetPendingLifecycle(context.Background(), session.ID); err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInternal, Message: "could not inspect lifecycle drain", Retryable: true}}
	} else if pending != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrDraining, Message: "a lifecycle drain prevents PTY resize"}}
	}
	if session.Kind == model.KindAgent {
		if response := s.syncRuntimeOwnership(session); response.Error != nil {
			response.ID = request.ID
			return response
		}
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
	case errors.Is(err, model.ErrTaskActive):
		return protocol.ErrTaskActive
	case errors.Is(err, model.ErrLifecyclePending):
		return protocol.ErrDraining
	case errors.Is(err, model.ErrPendingYield):
		return protocol.ErrPendingYield
	case errors.Is(err, model.ErrAdapterNotHealthy), errors.Is(err, model.ErrSessionNotRunning), errors.Is(err, model.ErrYieldUnsupported):
		return protocol.ErrAdapterUnhealthy
	default:
		return protocol.ErrInternal
	}
}

func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closing = true
		close(s.done)
		if s.retainedOutputCancel != nil {
			s.retainedOutputCancel()
		}
		s.registry.CloseAll()
		closeErr = s.listener.Close()
		s.lifecycleMu.Unlock()
		s.connMu.Lock()
		for conn := range s.connections {
			_ = conn.Close()
		}
		s.connMu.Unlock()
		s.handlers.Wait()
		s.retainedOutputWG.Wait()
		s.lifecycleCoordinatorWG.Wait()
		s.lifecycleWorkersWG.Wait()
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
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
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
