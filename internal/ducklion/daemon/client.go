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
	conn         io.ReadWriteCloser
	codec        *bridge.Codec
	mu           sync.Mutex
	streaming    bool
	instanceID   string
	capabilities map[string]bool
}

var ErrClientStreaming = errors.New("ducklion client connection is dedicated to output streaming")

type OutputStreamEnded struct {
	Reason     string
	NextOffset uint64
}

type RemoteError struct{ Detail protocol.Error }

func (e *RemoteError) Error() string { return string(e.Detail.Code) + ": " + e.Detail.Message }

func (e *OutputStreamEnded) Error() string { return "output stream ended: " + e.Reason }

func Dial(socketPath, principal string) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return Connect(conn, principal)
}

func Connect(conn io.ReadWriteCloser, principal string) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return ConnectContext(ctx, conn, principal)
}

func ConnectContext(ctx context.Context, conn io.ReadWriteCloser, principal string) (client *Client, returnErr error) {
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
	if err := codec.Write(protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Role: protocol.RoleDucklord, Principal: principal, Capabilities: []string{"status", "sessions_list", "output_subscribe", "session_input", "session_resize"}}); err != nil {
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
		return nil, fmt.Errorf("ducklion handshake rejected: %s", handshakeResponse.Error.Message)
	}
	negotiated := handshakeResponse.Handshake
	offered := map[string]bool{"status": true, "sessions_list": true, "output_subscribe": true, "session_input": true, "session_resize": true}
	validCapabilities := true
	for _, capability := range negotiated.Capabilities {
		validCapabilities = validCapabilities && offered[capability]
	}
	if negotiated.Major != protocol.Major || negotiated.Minor < 0 || negotiated.Minor > protocol.Minor || negotiated.Role != protocol.RoleDucklord ||
		negotiated.Principal != principal || !hasCapability(negotiated.Capabilities, "status") || !validCapabilities {
		conn.Close()
		return nil, fmt.Errorf("ducklion returned an invalid handshake")
	}
	setDeadline(conn, time.Time{})
	capabilities := make(map[string]bool, len(negotiated.Capabilities))
	for _, capability := range negotiated.Capabilities {
		capabilities[capability] = true
	}
	client = &Client{conn: conn, codec: codec, capabilities: capabilities}
	response, err := client.Call(protocol.Request{ID: "dial-status", Type: "status"})
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streaming {
		return protocol.Response{}, ErrClientStreaming
	}
	if err := request.Validate(); err != nil {
		return protocol.Response{}, err
	}
	if err := c.codec.Write(request); err != nil {
		return protocol.Response{}, err
	}
	var response protocol.Response
	if err := c.codec.Read(&response); err != nil {
		return protocol.Response{}, err
	}
	if err := response.Validate(); err != nil {
		return protocol.Response{}, err
	}
	if response.ID != request.ID {
		return protocol.Response{}, fmt.Errorf("ducklion response ID mismatch")
	}
	return response, nil
}

type OutputSubscription struct {
	client   *Client
	metadata protocol.OutputSubscribeResult
	next     uint64
	readMu   sync.Mutex
}

func (c *Client) SubscribeOutput(sessionID string, generation, offset uint64) (*OutputSubscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streaming {
		return nil, ErrClientStreaming
	}
	if err := c.requireCapability("output_subscribe"); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(protocol.OutputSubscribe{Offset: offset})
	request := protocol.Request{ID: "output-subscribe", Type: "session.output_subscribe", InstanceID: c.instanceID, SessionID: sessionID, RuntimeGeneration: &generation, Body: body}
	if err := c.codec.Write(request); err != nil {
		return nil, err
	}
	var response protocol.Response
	if err := c.codec.Read(&response); err != nil {
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
		metadata.EndOffset < metadata.StartOffset || metadata.StartOffset < offset && !metadata.Gap {
		return nil, fmt.Errorf("ducklion returned invalid output subscription metadata")
	}
	c.streaming = true
	return &OutputSubscription{client: c, metadata: metadata, next: metadata.StartOffset}, nil
}

func (s *OutputSubscription) Metadata() protocol.OutputSubscribeResult { return s.metadata }

func (s *OutputSubscription) Read() (protocol.OutputEvent, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	var event protocol.OutputEvent
	if err := s.client.codec.Read(&event); err != nil {
		return protocol.OutputEvent{}, err
	}
	if event.SubscriptionID != s.metadata.SubscriptionID || event.RuntimeID != s.metadata.RuntimeID || event.InstanceID != s.metadata.InstanceID ||
		event.SessionID != s.metadata.SessionID || event.RuntimeGeneration != s.metadata.RuntimeGeneration {
		return protocol.OutputEvent{}, fmt.Errorf("invalid or non-contiguous output event")
	}
	if event.Type == "output_end" {
		if len(event.Frame.Data) != 0 || event.Frame.Offset != s.next || event.Reason == "" {
			return protocol.OutputEvent{}, fmt.Errorf("invalid output end event")
		}
		return event, &OutputStreamEnded{Reason: event.Reason, NextOffset: s.next}
	}
	if event.Type != "output" || len(event.Frame.Data) == 0 || event.Frame.Offset != s.next {
		return protocol.OutputEvent{}, fmt.Errorf("invalid output event type")
	}
	s.next += uint64(len(event.Frame.Data))
	return event, nil
}

func (s *OutputSubscription) Close() error { return s.client.Close() }

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) ListSessions() ([]protocol.SessionSummary, error) {
	if err := c.requireCapability("sessions_list"); err != nil {
		return nil, err
	}
	response, err := c.Call(protocol.Request{ID: "sessions-list", Type: "sessions.list"})
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

func (c *Client) SendInput(sessionID string, epoch, generation uint64, data []byte) error {
	if err := c.requireCapability("session_input"); err != nil {
		return err
	}
	body, _ := json.Marshal(protocol.SessionInput{Data: data})
	response, err := c.Call(protocol.Request{ID: uuid.NewString(), Type: "session.input", InstanceID: c.instanceID, SessionID: sessionID,
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
