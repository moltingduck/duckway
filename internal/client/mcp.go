package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
)

// Minimal MCP server (Model Context Protocol) over stdio. Speaks JSON-RPC
// 2.0 — one message per line — exactly what `claude` launches when it reads
// our `~/.claude/mcp.json` entry.
//
// Surface:
//   * initialize, notifications/initialized — handshake
//   * tools/list, tools/call — the only thing agents actually use
//   * ping — keep-alive
//
// Each tool delegates to /client/cc/* on the duckway server. cc.json (written
// by `duckway sync`) is the authoritative list of CCs the agent can touch.

// MCPServer is the runtime. It loads its CC state lazily so a fresh
// `duckway sync` between Claude Code restarts is picked up immediately.
type MCPServer struct {
	configDir string
	cfg       *Config

	mu       sync.Mutex
	stateAt  string // file mtime cached as ISO; "" means unloaded
	state    *CCStateFile
}

func NewMCPServer(configDir string, cfg *Config) *MCPServer {
	return &MCPServer{configDir: configDir, cfg: cfg}
}

// Run reads JSON-RPC requests line by line from in and writes responses to
// out. Exits when in returns EOF (Claude Code closing the pipe).
func (s *MCPServer) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	enc := json.NewEncoder(out)

	// Logging goes to stderr — stdout is the protocol channel and any
	// stray bytes there break the agent.
	log.SetOutput(io.Discard)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		if len(line) == 0 {
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// Malformed message — emit a parse error response.
			enc.Encode(errResponse(nil, -32700, "parse error: "+err.Error()))
			continue
		}

		// Notifications carry no id and expect no response.
		if req.ID == nil {
			s.handleNotification(req)
			continue
		}

		resp := s.handleRequest(ctx, req)
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
	}
}

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func okResponse(id json.RawMessage, result interface{}) jsonrpcResponse {
	return jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errResponse(id json.RawMessage, code int, msg string) jsonrpcResponse {
	if id == nil {
		id = json.RawMessage("null")
	}
	return jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: msg}}
}

func (s *MCPServer) handleNotification(req jsonrpcRequest) {
	// No-op for now — Claude Code sends `notifications/initialized` after
	// the handshake completes; we don't need to act on it.
}

func (s *MCPServer) handleRequest(ctx context.Context, req jsonrpcRequest) jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return okResponse(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "duckway-cc",
				"version": "1",
			},
		})
	case "ping":
		return okResponse(req.ID, map[string]interface{}{})
	case "tools/list":
		return okResponse(req.ID, map[string]interface{}{
			"tools": s.toolDefinitions(),
		})
	case "tools/call":
		return s.handleToolCall(ctx, req)
	default:
		return errResponse(req.ID, -32601, "method not found: "+req.Method)
	}
}

// loadState reads cc.json on demand. Cheap enough to do per-call; means
// re-running `duckway sync` is picked up without restarting the MCP server.
func (s *MCPServer) loadState() (*CCStateFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := readCCState(s.configDir)
	if err != nil {
		return nil, err
	}
	s.state = state
	return state, nil
}

func readCCState(configDir string) (*CCStateFile, error) {
	raw, err := readFile(ccStateFilePath(configDir))
	if err != nil {
		// No state file = no CCs assigned. Don't error — just empty.
		empty := &CCStateFile{CCs: []CCStateAssignment{}}
		return empty, nil
	}
	var state CCStateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("parse cc.json: %w", err)
	}
	return &state, nil
}

// readFile is a tiny indirection so tests can stub it if they want.
var readFile = func(path string) ([]byte, error) {
	return readFileImpl(path)
}
