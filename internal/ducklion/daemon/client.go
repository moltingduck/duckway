package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

type Client struct {
	conn       net.Conn
	codec      *bridge.Codec
	mu         sync.Mutex
	streaming  bool
	instanceID string
}

var ErrClientStreaming = errors.New("ducklion client connection is dedicated to output streaming")

type OutputStreamEnded struct {
	Reason     string
	NextOffset uint64
}

func (e *OutputStreamEnded) Error() string { return "output stream ended: " + e.Reason }

func Dial(socketPath, principal string) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	codec := bridge.NewCodec(conn, conn, bridge.DefaultMaxFrame)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := codec.Write(protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Role: protocol.RoleDucklord, Principal: principal, Capabilities: []string{"status", "sessions_list", "output_subscribe"}}); err != nil {
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
	if negotiated.Major != protocol.Major || negotiated.Role != protocol.RoleDucklord || negotiated.Principal != principal {
		conn.Close()
		return nil, fmt.Errorf("ducklion returned an invalid handshake")
	}
	_ = conn.SetDeadline(time.Time{})
	client := &Client{conn: conn, codec: codec}
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
}

func (c *Client) SubscribeOutput(sessionID string, generation, offset uint64) (*OutputSubscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streaming {
		return nil, ErrClientStreaming
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
