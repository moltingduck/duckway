package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

type Client struct {
	conn                 io.ReadWriteCloser
	codec                *bridge.Codec
	writeGate            chan struct{}
	stateMu              sync.Mutex
	pending              map[string]chan responseResult
	subscriptions        map[string]*OutputSubscription
	orphanEvents         map[string][]protocol.OutputEvent
	ignoredSubscriptions map[string]bool
	ignoredOrder         []string
	done                 chan struct{}
	closeOnce            sync.Once
	readErr              error
	instanceID           string
	capabilities         map[string]bool
}

type responseResult struct {
	response protocol.Response
	err      error
}

type outputResult struct {
	event protocol.OutputEvent
	err   error
}

type OutputStreamEnded struct {
	Reason     string
	NextOffset uint64
}

var ErrOutputSubscriptionClosed = errors.New("output subscription is closed")

type RemoteError struct{ Detail protocol.Error }

func (e *RemoteError) Error() string { return string(e.Detail.Code) + ": " + e.Detail.Message }

func (e *OutputStreamEnded) Error() string { return "output stream ended: " + e.Reason }

func Dial(socketPath, principal string) (*Client, error) {
	return DialRole(socketPath, principal, protocol.RoleDucklord)
}

func DialCC(socketPath, channelHandle string) (*Client, error) {
	return DialRole(socketPath, channelHandle, protocol.RoleDuckwayCC)
}

func DialRole(socketPath, principal string, role protocol.PeerRole) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return ConnectRole(conn, principal, role)
}

func Connect(conn io.ReadWriteCloser, principal string) (*Client, error) {
	return ConnectRole(conn, principal, protocol.RoleDucklord)
}

func ConnectRole(conn io.ReadWriteCloser, principal string, role protocol.PeerRole) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return ConnectRoleContext(ctx, conn, principal, role)
}

func ConnectContext(ctx context.Context, conn io.ReadWriteCloser, principal string) (client *Client, returnErr error) {
	return ConnectRoleContext(ctx, conn, principal, protocol.RoleDucklord)
}

func ConnectRoleContext(ctx context.Context, conn io.ReadWriteCloser, principal string, role protocol.PeerRole) (client *Client, returnErr error) {
	cancelDone := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		close(cancelDone)
	})
	defer func() {
		if !stopCancellation() {
			<-cancelDone
		}
		if ctx.Err() != nil {
			_ = conn.Close()
			client = nil
			returnErr = ctx.Err()
		}
	}()
	codec := bridge.NewCodec(conn, conn, bridge.DefaultMaxFrame)
	setDeadline(conn, time.Now().Add(10*time.Second))
	offeredCapabilities := []string{"status", "sessions_list", "session_create", "session_stop", "session_yield", "output_subscribe", "output_unsubscribe", "session_input", "session_resize"}
	if role == protocol.RoleDuckwayCC {
		offeredCapabilities = []string{"status", "sessions_list", "session_yield", "session_task"}
	}
	if err := codec.Write(protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Role: role, Principal: principal, Capabilities: offeredCapabilities}); err != nil {
		conn.Close()
		return nil, err
	}
	var handshakeResponse protocol.HandshakeResponse
	if err := codec.Read(&handshakeResponse); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ducklion handshake: %w", err)
	}
	if err := handshakeResponse.Validate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ducklion handshake: %w", err)
	}
	if handshakeResponse.Error != nil {
		conn.Close()
		return nil, &RemoteError{Detail: *handshakeResponse.Error}
	}
	negotiated := handshakeResponse.Handshake
	offered := make(map[string]bool, len(offeredCapabilities))
	for _, capability := range offeredCapabilities {
		offered[capability] = true
	}
	validCapabilities := true
	for _, capability := range negotiated.Capabilities {
		validCapabilities = validCapabilities && offered[capability]
	}
	if negotiated.Major != protocol.Major || negotiated.Minor < 0 || negotiated.Minor > protocol.Minor || negotiated.Role != role ||
		negotiated.Principal != principal || !hasCapability(negotiated.Capabilities, "status") || !validCapabilities {
		conn.Close()
		return nil, fmt.Errorf("ducklion returned an invalid handshake")
	}
	setDeadline(conn, time.Time{})
	capabilities := make(map[string]bool, len(negotiated.Capabilities))
	for _, capability := range negotiated.Capabilities {
		capabilities[capability] = true
	}
	client = &Client{conn: conn, codec: codec, capabilities: capabilities, pending: make(map[string]chan responseResult),
		subscriptions: make(map[string]*OutputSubscription), orphanEvents: make(map[string][]protocol.OutputEvent), ignoredSubscriptions: make(map[string]bool),
		writeGate: make(chan struct{}, 1), done: make(chan struct{})}
	client.writeGate <- struct{}{}
	go client.readLoop()
	response, err := client.CallContext(ctx, protocol.Request{ID: uuid.NewString(), Type: "status"})
	if err != nil || response.Error != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ducklion status during dial failed")
	}
	var status struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(response.Result, &status); err != nil || status.InstanceID == "" {
		_ = conn.Close()
		return nil, fmt.Errorf("ducklion returned invalid status during dial")
	}
	client.instanceID = status.InstanceID
	return client, nil
}

func (c *Client) requireCapability(capability string) error {
	if !c.capabilities[capability] {
		return fmt.Errorf("ducklion capability %q was not negotiated", capability)
	}
	return nil
}

func setDeadline(conn io.ReadWriteCloser, deadline time.Time) {
	if setter, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = setter.SetDeadline(deadline)
	}
}

func (c *Client) Call(request protocol.Request) (protocol.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.CallContext(ctx, request)
}

func (c *Client) CallContext(ctx context.Context, request protocol.Request) (protocol.Response, error) {
	if err := request.Validate(); err != nil {
		return protocol.Response{}, err
	}
	result := make(chan responseResult, 1)
	c.stateMu.Lock()
	if _, exists := c.pending[request.ID]; exists {
		c.stateMu.Unlock()
		return protocol.Response{}, fmt.Errorf("duplicate in-flight request ID %q", request.ID)
	}
	select {
	case <-c.done:
		err := c.readErr
		c.stateMu.Unlock()
		if err == nil {
			err = io.ErrClosedPipe
		}
		return protocol.Response{}, err
	default:
	}
	c.pending[request.ID] = result
	c.stateMu.Unlock()
	select {
	case <-ctx.Done():
		c.stateMu.Lock()
		delete(c.pending, request.ID)
		c.stateMu.Unlock()
		_ = c.Close()
		return protocol.Response{}, ctx.Err()
	case <-c.done:
		return protocol.Response{}, io.ErrClosedPipe
	case <-c.writeGate:
	}
	stopWriteCancellation := context.AfterFunc(ctx, func() { _ = c.Close() })
	err := c.codec.Write(request)
	stopWriteCancellation()
	c.writeGate <- struct{}{}
	if ctx.Err() != nil {
		c.stateMu.Lock()
		delete(c.pending, request.ID)
		c.stateMu.Unlock()
		return protocol.Response{}, ctx.Err()
	}
	if err != nil {
		c.stateMu.Lock()
		delete(c.pending, request.ID)
		c.stateMu.Unlock()
		_ = c.Close()
		return protocol.Response{}, err
	}
	select {
	case outcome := <-result:
		return outcome.response, outcome.err
	case <-c.done:
		select {
		case outcome := <-result:
			return outcome.response, outcome.err
		default:
		}
		c.stateMu.Lock()
		err := c.readErr
		c.stateMu.Unlock()
		if err == nil {
			err = io.ErrClosedPipe
		}
		return protocol.Response{}, err
	case <-ctx.Done():
		c.stateMu.Lock()
		delete(c.pending, request.ID)
		c.stateMu.Unlock()
		_ = c.Close()
		return protocol.Response{}, ctx.Err()
	}
}

type OutputSubscription struct {
	client       *Client
	metadata     protocol.OutputSubscribeResult
	next         uint64
	readMu       sync.Mutex
	events       chan outputResult
	terminalMu   sync.Mutex
	terminalErr  error
	terminalDone chan struct{}
	terminalOnce sync.Once
	closeOnce    sync.Once
}

func (c *Client) SubscribeOutput(sessionID string, generation, offset uint64) (*OutputSubscription, error) {
	return c.subscribeOutput(sessionID, generation, protocol.OutputSubscribe{Offset: offset})
}

func (c *Client) SubscribeOutputTail(sessionID string, generation, tailBytes uint64) (*OutputSubscription, error) {
	if tailBytes == 0 || tailBytes > 4<<20 {
		return nil, fmt.Errorf("output tail must contain 1 to 4194304 bytes")
	}
	return c.subscribeOutput(sessionID, generation, protocol.OutputSubscribe{TailBytes: tailBytes})
}

func (c *Client) subscribeOutput(sessionID string, generation uint64, options protocol.OutputSubscribe) (*OutputSubscription, error) {
	if err := c.requireCapability("output_subscribe"); err != nil {
		return nil, err
	}
	if err := c.requireCapability("output_unsubscribe"); err != nil {
		return nil, fmt.Errorf("multiplexed output requires unsubscribe support: %w", err)
	}
	body, _ := json.Marshal(options)
	request := protocol.Request{ID: uuid.NewString(), Type: "session.output_subscribe", InstanceID: c.instanceID, SessionID: sessionID, RuntimeGeneration: &generation, Body: body}
	response, err := c.Call(request)
	if err != nil {
		return nil, err
	}
	if err := validateResponse(response, request.ID); err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, fmt.Errorf("subscribe output: %s", response.Error.Message)
	}
	var metadata protocol.OutputSubscribeResult
	if err := json.Unmarshal(response.Result, &metadata); err != nil {
		return nil, err
	}
	if metadata.SubscriptionID == "" || metadata.RuntimeID == "" || metadata.InstanceID != c.instanceID || metadata.SessionID != sessionID || metadata.RuntimeGeneration != generation ||
		metadata.EndOffset < metadata.StartOffset || options.TailBytes == 0 && metadata.StartOffset < options.Offset && !metadata.Gap {
		return nil, fmt.Errorf("ducklion returned invalid output subscription metadata")
	}
	subscription := &OutputSubscription{client: c, metadata: metadata, next: metadata.StartOffset, events: make(chan outputResult, 256), terminalDone: make(chan struct{})}
	c.stateMu.Lock()
	orphans := c.orphanEvents[metadata.SubscriptionID]
	delete(c.orphanEvents, metadata.SubscriptionID)
	for _, event := range orphans {
		subscription.events <- outputResult{event: event}
	}
	c.subscriptions[metadata.SubscriptionID] = subscription
	c.stateMu.Unlock()
	return subscription, nil
}

func (s *OutputSubscription) Metadata() protocol.OutputSubscribeResult { return s.metadata }

func (s *OutputSubscription) Read() (protocol.OutputEvent, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	var outcome outputResult
	if err := s.terminalError(); err != nil {
		return protocol.OutputEvent{}, err
	}
	select {
	case <-s.terminalDone:
		return protocol.OutputEvent{}, s.terminalError()
	case outcome = <-s.events:
	case <-s.client.done:
		s.client.stateMu.Lock()
		err := s.client.readErr
		s.client.stateMu.Unlock()
		if err == nil {
			err = io.ErrClosedPipe
		}
		return protocol.OutputEvent{}, err
	}
	if outcome.err != nil {
		return protocol.OutputEvent{}, outcome.err
	}
	event := outcome.event
	if event.SubscriptionID != s.metadata.SubscriptionID || event.RuntimeID != s.metadata.RuntimeID || event.InstanceID != s.metadata.InstanceID ||
		event.SessionID != s.metadata.SessionID || event.RuntimeGeneration != s.metadata.RuntimeGeneration {
		return protocol.OutputEvent{}, fmt.Errorf("invalid or non-contiguous output event")
	}
	if event.Type == "output_end" {
		if len(event.Frame.Data) != 0 || event.Frame.Offset != s.next || event.Reason == "" {
			return protocol.OutputEvent{}, fmt.Errorf("invalid output end event")
		}
		ended := &OutputStreamEnded{Reason: event.Reason, NextOffset: s.next}
		s.terminate(ended)
		return event, ended
	}
	if event.Type != "output" || len(event.Frame.Data) == 0 || event.Frame.Offset != s.next {
		return protocol.OutputEvent{}, fmt.Errorf("invalid output event type")
	}
	s.next += uint64(len(event.Frame.Data))
	return event, nil
}

func (s *OutputSubscription) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.terminate(ErrOutputSubscriptionClosed)
		s.client.stateMu.Lock()
		delete(s.client.subscriptions, s.metadata.SubscriptionID)
		s.client.markIgnoredLocked(s.metadata.SubscriptionID)
		s.client.stateMu.Unlock()
		body, _ := json.Marshal(protocol.OutputUnsubscribe{SubscriptionID: s.metadata.SubscriptionID})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		response, err := s.client.CallContext(ctx, protocol.Request{ID: uuid.NewString(), Type: "session.output_unsubscribe", InstanceID: s.client.instanceID, Body: body})
		if err != nil {
			closeErr = err
			return
		}
		if response.Error != nil && response.Error.Code != protocol.ErrNotFound {
			closeErr = &RemoteError{Detail: *response.Error}
		}
	})
	return closeErr
}

func (c *Client) Close() error {
	var closeErr error
	c.closeOnce.Do(func() { closeErr = c.conn.Close() })
	return closeErr
}

func (c *Client) readLoop() {
	for {
		var raw json.RawMessage
		if err := c.codec.Read(&raw); err != nil {
			c.shutdown(err)
			return
		}
		var envelope struct {
			ID             string `json:"id"`
			Type           string `json:"type"`
			SubscriptionID string `json:"subscription_id"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			c.shutdown(err)
			return
		}
		if envelope.ID != "" {
			var response protocol.Response
			if err := json.Unmarshal(raw, &response); err != nil || response.Validate() != nil {
				c.shutdown(fmt.Errorf("invalid Ducklion response"))
				return
			}
			c.stateMu.Lock()
			waiter := c.pending[response.ID]
			delete(c.pending, response.ID)
			c.stateMu.Unlock()
			if waiter == nil {
				c.shutdown(fmt.Errorf("unexpected Ducklion response ID %q", response.ID))
				return
			}
			waiter <- responseResult{response: response}
			continue
		}
		if envelope.SubscriptionID == "" || (envelope.Type != "output" && envelope.Type != "output_end") {
			c.shutdown(fmt.Errorf("unexpected Ducklion event"))
			return
		}
		var event protocol.OutputEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			c.shutdown(err)
			return
		}
		c.dispatchOutput(event)
	}
}

func (c *Client) dispatchOutput(event protocol.OutputEvent) {
	if len(event.SubscriptionID) > 128 || event.Type == "output" && (len(event.Frame.Data) == 0 || len(event.Frame.Data) > 64<<10) ||
		event.Type == "output_end" && (len(event.Frame.Data) != 0 || event.Reason == "") {
		c.shutdown(fmt.Errorf("invalid Ducklion output event"))
		return
	}
	c.stateMu.Lock()
	if c.ignoredSubscriptions[event.SubscriptionID] {
		c.stateMu.Unlock()
		return
	}
	subscription := c.subscriptions[event.SubscriptionID]
	if subscription == nil {
		orphan := c.orphanEvents[event.SubscriptionID]
		if (len(orphan) > 0 || len(c.orphanEvents) < 4) && len(orphan) < 80 {
			c.orphanEvents[event.SubscriptionID] = append(orphan, event)
		}
		c.stateMu.Unlock()
		return
	}
	if event.Type == "output_end" {
		delete(c.subscriptions, event.SubscriptionID)
		c.markIgnoredLocked(event.SubscriptionID)
	}
	c.stateMu.Unlock()
	select {
	case subscription.events <- outputResult{event: event}:
	default:
		c.stateMu.Lock()
		delete(c.subscriptions, event.SubscriptionID)
		c.markIgnoredLocked(event.SubscriptionID)
		c.stateMu.Unlock()
		subscription.terminate(fmt.Errorf("output subscriber lagged behind"))
	}
}

func (s *OutputSubscription) terminate(err error) {
	if err == nil {
		err = ErrOutputSubscriptionClosed
	}
	s.terminalOnce.Do(func() {
		s.terminalMu.Lock()
		s.terminalErr = err
		s.terminalMu.Unlock()
		close(s.terminalDone)
	})
}

func (s *OutputSubscription) terminalError() error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	return s.terminalErr
}

func (c *Client) markIgnoredLocked(subscriptionID string) {
	if c.ignoredSubscriptions[subscriptionID] {
		return
	}
	c.ignoredSubscriptions[subscriptionID] = true
	c.ignoredOrder = append(c.ignoredOrder, subscriptionID)
	if len(c.ignoredOrder) > 256 {
		delete(c.ignoredSubscriptions, c.ignoredOrder[0])
		c.ignoredOrder = c.ignoredOrder[1:]
	}
}

func (c *Client) shutdown(err error) {
	c.stateMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.stateMu.Unlock()
	c.closeOnce.Do(func() { _ = c.conn.Close() })
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

func (c *Client) ListSessions() ([]protocol.SessionSummary, error) {
	if err := c.requireCapability("sessions_list"); err != nil {
		return nil, err
	}
	response, err := c.Call(protocol.Request{ID: uuid.NewString(), Type: "sessions.list"})
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, fmt.Errorf("list sessions: %s", response.Error.Message)
	}
	var sessions []protocol.SessionSummary
	if err := json.Unmarshal(response.Result, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (c *Client) CreateSession(ctx context.Context, request protocol.SessionCreate) (protocol.SessionSummary, error) {
	return c.CreateSessionWithID(ctx, uuid.NewString(), request)
}

func (c *Client) CreateSessionWithID(ctx context.Context, requestID string, request protocol.SessionCreate) (protocol.SessionSummary, error) {
	if err := c.requireCapability("session_create"); err != nil {
		return protocol.SessionSummary{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	response, err := c.CallContext(ctx, protocol.Request{ID: requestID, Type: "session.create", InstanceID: c.instanceID, Body: body})
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	if response.Error != nil {
		return protocol.SessionSummary{}, &RemoteError{Detail: *response.Error}
	}
	var summary protocol.SessionSummary
	if err := json.Unmarshal(response.Result, &summary); err != nil {
		return protocol.SessionSummary{}, err
	}
	return summary, nil
}

func (c *Client) StopSession(ctx context.Context, sessionID string, epoch, generation uint64) error {
	return c.StopSessionWithID(ctx, uuid.NewString(), sessionID, epoch, generation)
}

func (c *Client) StopSessionWithID(ctx context.Context, requestID, sessionID string, epoch, generation uint64) error {
	if err := c.requireCapability("session_stop"); err != nil {
		return err
	}
	response, err := c.CallContext(ctx, protocol.Request{ID: requestID, Type: "session.stop", InstanceID: c.instanceID, SessionID: sessionID,
		OwnershipEpoch: &epoch, RuntimeGeneration: &generation})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return &RemoteError{Detail: *response.Error}
	}
	return nil
}

func (c *Client) YieldSession(ctx context.Context, sessionID string, epoch, generation uint64, wait bool) (protocol.SessionYieldResult, error) {
	return c.YieldSessionWithID(ctx, uuid.NewString(), sessionID, epoch, generation, wait)
}

func (c *Client) YieldSessionWithID(ctx context.Context, requestID, sessionID string, epoch, generation uint64, wait bool) (protocol.SessionYieldResult, error) {
	if err := c.requireCapability("session_yield"); err != nil {
		return protocol.SessionYieldResult{}, err
	}
	body, _ := json.Marshal(protocol.SessionYield{Wait: wait})
	response, err := c.CallContext(ctx, protocol.Request{ID: requestID, Type: "session.yield", InstanceID: c.instanceID, SessionID: sessionID,
		OwnershipEpoch: &epoch, RuntimeGeneration: &generation, Body: body})
	if err != nil {
		return protocol.SessionYieldResult{}, err
	}
	if response.Error != nil {
		return protocol.SessionYieldResult{}, &RemoteError{Detail: *response.Error}
	}
	var result protocol.SessionYieldResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) BeginTask(ctx context.Context, requestID, sessionID string, epoch, generation uint64) (protocol.SessionTaskResult, error) {
	return c.sessionTask(ctx, "session.task_begin", requestID, sessionID, epoch, generation)
}

func (c *Client) CompleteTask(ctx context.Context, requestID, sessionID string, epoch, generation uint64) (protocol.SessionTaskResult, error) {
	return c.sessionTask(ctx, "session.task_complete", requestID, sessionID, epoch, generation)
}

func (c *Client) sessionTask(ctx context.Context, operation, requestID, sessionID string, epoch, generation uint64) (protocol.SessionTaskResult, error) {
	if err := c.requireCapability("session_task"); err != nil {
		return protocol.SessionTaskResult{}, err
	}
	response, err := c.CallContext(ctx, protocol.Request{ID: requestID, Type: operation, InstanceID: c.instanceID, SessionID: sessionID,
		OwnershipEpoch: &epoch, RuntimeGeneration: &generation})
	if err != nil {
		return protocol.SessionTaskResult{}, err
	}
	if response.Error != nil {
		return protocol.SessionTaskResult{}, &RemoteError{Detail: *response.Error}
	}
	var result protocol.SessionTaskResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) SendInput(sessionID string, epoch, generation uint64, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.SendInputContext(ctx, sessionID, epoch, generation, data)
}

func (c *Client) SendInputContext(ctx context.Context, sessionID string, epoch, generation uint64, data []byte) error {
	if err := c.requireCapability("session_input"); err != nil {
		return err
	}
	body, _ := json.Marshal(protocol.SessionInput{Data: data})
	response, err := c.CallContext(ctx, protocol.Request{ID: uuid.NewString(), Type: "session.input", InstanceID: c.instanceID, SessionID: sessionID,
		OwnershipEpoch: &epoch, RuntimeGeneration: &generation, Body: body})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return &RemoteError{Detail: *response.Error}
	}
	return nil
}

func (c *Client) Resize(sessionID string, epoch, generation uint64, rows, cols uint16) error {
	if err := c.requireCapability("session_resize"); err != nil {
		return err
	}
	body, _ := json.Marshal(protocol.SessionResize{Rows: rows, Cols: cols})
	response, err := c.Call(protocol.Request{ID: uuid.NewString(), Type: "session.resize", InstanceID: c.instanceID, SessionID: sessionID,
		OwnershipEpoch: &epoch, RuntimeGeneration: &generation, Body: body})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return &RemoteError{Detail: *response.Error}
	}
	return nil
}
