package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

type Client struct {
	conn  net.Conn
	codec *bridge.Codec
	mu    sync.Mutex
}

func Dial(socketPath, principal string) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	codec := bridge.NewCodec(conn, conn, bridge.DefaultMaxFrame)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := codec.Write(protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Role: protocol.RoleDucklord, Principal: principal, Capabilities: []string{"status"}}); err != nil {
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
	return &Client{conn: conn, codec: codec}, nil
}

func (c *Client) Call(request protocol.Request) (protocol.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
