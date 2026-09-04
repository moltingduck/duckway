package daemon

import (
	"bytes"
	"context"
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
		connections: make(map[*net.UnixConn]struct{})}, nil
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
	if remote.Role != protocol.RoleDucklord {
		_ = codec.Write(protocol.HandshakeResponse{Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "peer role is not available on this endpoint"}})
		return
	}
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: []string{"status"}}
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
		response := s.route(request)
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
	local := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Capabilities: []string{"supervisor_recovery"}}
	negotiated, protocolError := protocol.Negotiate(local, remote)
	if protocolError != nil || !hasCapability(negotiated.Capabilities, "supervisor_recovery") {
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
	defer func() {
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
		if request.Validate() != nil || request.Type != "supervisor.ping" || request.InstanceID != string(s.instanceID) ||
			request.SessionID != string(identity.SessionID) || request.RuntimeGeneration == nil || *request.RuntimeGeneration != identity.Generation || len(request.Body) != 0 {
			writeSupervisorError(codec, request.ID, protocol.ErrInvalidArgument, "operation is unavailable to supervisors")
			continue
		}
		result, _ := json.Marshal(map[string]bool{"ok": true})
		if err := codec.Write(protocol.Response{ID: request.ID, Result: result}); err != nil {
			return
		}
	}
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

func (s *Server) route(request protocol.Request) protocol.Response {
	if request.InstanceID != "" && request.InstanceID != string(s.instanceID) {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrNotFound, Message: "Ducklion instance does not match"}}
	}
	switch request.Type {
	case "status":
		result, _ := json.Marshal(map[string]any{"instance_id": s.instanceID, "protocol_major": protocol.Major, "protocol_minor": protocol.Minor})
		return protocol.Response{ID: request.ID, Result: result}
	case "sessions.list":
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
	default:
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "unsupported operation"}}
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
