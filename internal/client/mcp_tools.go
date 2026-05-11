package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// readFileImpl is the real implementation of readFile (split out so tests can
// override the var without losing the original).
func readFileImpl(path string) ([]byte, error) { return os.ReadFile(path) }

// toolDefinitions returns the JSON-Schema descriptions Claude Code shows
// the model. Names start with `discord_` since that's the only service v1
// supports — when CC adds Slack/Telegram we can prefix-them similarly.
//
// CC v2: 1:1 client↔CC, so cc_id is implicit on every call.
func (s *MCPServer) toolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "discord_get_my_cc",
			"description": "Show the Discord Control Channel this agent is bound to (1:1 with the client). Returns cc_name, agent_type, and the management_handle (the channel the daemon listens on for !commands).",
			"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			"name":        "discord_list_channels",
			"description": "List channels under the CC's category. Returns handles (opaque IDs) — never raw Discord channel IDs. Pass the handle to other discord_* tools.",
			"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			"name":        "discord_create_task_channel",
			"description": "Create a new text channel under the CC's category. Use one channel per task so messages stay focused. Returns the new channel handle.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":  map[string]interface{}{"type": "string", "description": "Channel name. Will be lowercased and dashes substituted to fit Discord's rules."},
					"topic": map[string]interface{}{"type": "string", "description": "Optional channel topic shown in Discord's UI."},
					"cwd":   map[string]interface{}{"type": "string", "description": "Optional working directory the daemon will spawn `claude` in. Defaults to ~/.duckway/cc-workspace/<handle>."},
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "discord_archive_channel",
			"description": "Archive a task channel (rename + move out of category). Cannot archive the management channel. Idempotent.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel_handle": map[string]interface{}{"type": "string"},
				},
				"required": []string{"channel_handle"},
			},
		},
		{
			"name":        "discord_post",
			"description": "Post a message to a channel. Returns the message_id (use it for edit/delete).",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel_handle": map[string]interface{}{"type": "string"},
					"content":        map[string]interface{}{"type": "string", "description": "Message body. Discord supports markdown."},
				},
				"required": []string{"channel_handle", "content"},
			},
		},
		{
			"name":        "discord_edit_message",
			"description": "Replace a message's content. Only works on messages the bot itself posted.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel_handle": map[string]interface{}{"type": "string"},
					"message_id":     map[string]interface{}{"type": "string"},
					"content":        map[string]interface{}{"type": "string"},
				},
				"required": []string{"channel_handle", "message_id", "content"},
			},
		},
		{
			"name":        "discord_delete_message",
			"description": "Delete a message. Only works on messages the bot itself posted.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel_handle": map[string]interface{}{"type": "string"},
					"message_id":     map[string]interface{}{"type": "string"},
				},
				"required": []string{"channel_handle", "message_id"},
			},
		},
		{
			"name":        "discord_read_recent",
			"description": "Read recent messages in a channel (newest first). Use limit to cap the number. Lighter than wait_for_message — does not block.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel_handle": map[string]interface{}{"type": "string"},
					"limit":          map[string]interface{}{"type": "number", "description": "Max messages to return (1..100, default 50)."},
				},
				"required": []string{"channel_handle"},
			},
		},
		{
			"name":        "discord_wait_for_message",
			"description": "Long-poll for new gateway events (incoming messages, edits, deletes). Returns immediately if events are buffered, otherwise blocks up to timeout_seconds. Use this in agent loops to react to a human reply.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"since":           map[string]interface{}{"type": "number", "description": "Cursor from the previous call's response (0 = from the start)."},
					"channel_handles": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional list of handles to filter to."},
					"timeout_seconds": map[string]interface{}{"type": "number", "description": "0..60 seconds to wait. Default 25."},
				},
			},
		},
		{
			"name":        "duckway_list_local_sessions",
			"description": "List claude-code sessions stored locally under ~/.claude/projects/ on this agent machine. Returns session_id, cwd, last_active, message_count, first_message preview, and bound_to (the CC channel handle if already wired). Use this to let a human pick a pre-existing conversation to attach to a Discord channel via duckway_bind_session.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"only_unbound": map[string]interface{}{"type": "boolean", "description": "If true (default), hide sessions already bound to a CC channel. Set false to see every session including bound ones."},
					"cwd_filter":   map[string]interface{}{"type": "string", "description": "Optional substring match on cwd to narrow the list (e.g. \"duckway\" or \"/home/me/projects/api\")."},
					"limit":        map[string]interface{}{"type": "number", "description": "Cap the number of sessions returned (1..200, default 50). Newest first."},
				},
			},
		},
		{
			"name":        "duckway_bind_session",
			"description": "Tie an existing local claude session_id to a CC channel handle so the daemon will `claude --resume <session_id>` on the next inbound message. Use after duckway_list_local_sessions when the human picks one. channel_handle defaults to the current channel (the one this MCP call is running in, available as DUCKWAY_CC_CHANNEL_HANDLE in env).",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id":     map[string]interface{}{"type": "string", "description": "Claude session_id (UUID) from duckway_list_local_sessions."},
					"channel_handle": map[string]interface{}{"type": "string", "description": "CC channel to bind. Defaults to the env var DUCKWAY_CC_CHANNEL_HANDLE (the channel this conversation is running in)."},
				},
				"required": []string{"session_id"},
			},
		},
		{
			"name":        "discord_request_approval",
			"description": "Ask a human in Discord to approve / pick an option via reaction vote. Posts the question, pre-adds emoji reactions (✅/❌ for default yes/no, or 1️⃣2️⃣3️⃣… for multi-option), and BLOCKS until someone reacts or the timeout fires. Returns {chosen, reactor_user_id, timed_out}. Use before destructive or significant actions.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel_handle":    map[string]interface{}{"type": "string", "description": "Channel to ask in. Use the current task channel handle from your context."},
					"question":          map[string]interface{}{"type": "string", "description": "What to ask the human (markdown OK)."},
					"options":           map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional list of choices. Default is [\"yes\",\"no\"]. Max 10."},
					"timeout_seconds":   map[string]interface{}{"type": "number", "description": "How long to wait (1..3600). Default 300 = 5 minutes."},
					"required_reactors": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional list of Discord user_ids — only these users can decide."},
				},
				"required": []string{"channel_handle", "question"},
			},
		},
	}
}

// handleToolCall dispatches based on the tool name in params.name and
// converts the structured result into MCP's `{content:[{type:"text", text}]}`.
func (s *MCPServer) handleToolCall(ctx context.Context, req jsonrpcRequest) jsonrpcResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResponse(req.ID, -32602, "invalid params: "+err.Error())
	}

	args := map[string]interface{}{}
	if len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &args)
	}

	state, err := s.loadState()
	if err != nil {
		return toolError(req.ID, "load cc state: "+err.Error())
	}
	switch p.Name {
	case "discord_get_my_cc", "duckway_list_local_sessions", "duckway_bind_session":
		// These don't need a CC bound — list_local_sessions just reads
		// the filesystem; bind_session just writes to cc-sessions.json.
	default:
		if len(state.CCs) == 0 {
			return toolError(req.ID, "no CC bound to this client — ask the admin to create one in /admin/cc")
		}
	}

	var (
		out  interface{}
		err2 error
	)
	switch p.Name {
	case "discord_get_my_cc":
		out, err2 = s.callServer(ctx, "GET", "/client/cc", nil)
	case "discord_list_channels":
		out, err2 = s.callServer(ctx, "GET", "/client/cc/channels", nil)
	case "discord_create_task_channel":
		body := map[string]interface{}{"name": args["name"], "topic": args["topic"], "cwd": args["cwd"]}
		out, err2 = s.callServer(ctx, "POST", "/client/cc/channels", body)
	case "discord_archive_channel":
		handle, _ := args["channel_handle"].(string)
		out, err2 = s.callServer(ctx, "POST", fmt.Sprintf("/client/cc/channels/%s/archive", handle), nil)
	case "discord_post":
		handle, _ := args["channel_handle"].(string)
		out, err2 = s.callServer(ctx, "POST", fmt.Sprintf("/client/cc/channels/%s/messages", handle),
			map[string]interface{}{"content": args["content"]})
	case "discord_edit_message":
		handle, _ := args["channel_handle"].(string)
		mid, _ := args["message_id"].(string)
		out, err2 = s.callServer(ctx, "PATCH", fmt.Sprintf("/client/cc/channels/%s/messages/%s", handle, mid),
			map[string]interface{}{"content": args["content"]})
	case "discord_delete_message":
		handle, _ := args["channel_handle"].(string)
		mid, _ := args["message_id"].(string)
		out, err2 = s.callServer(ctx, "DELETE", fmt.Sprintf("/client/cc/channels/%s/messages/%s", handle, mid), nil)
	case "discord_read_recent":
		handle, _ := args["channel_handle"].(string)
		path := fmt.Sprintf("/client/cc/channels/%s/messages", handle)
		if v, ok := args["limit"].(float64); ok && v > 0 {
			path += fmt.Sprintf("?limit=%d", int(v))
		}
		out, err2 = s.callServer(ctx, "GET", path, nil)
	case "discord_request_approval":
		handle, _ := args["channel_handle"].(string)
		body := map[string]interface{}{"question": args["question"]}
		if v, ok := args["options"]; ok {
			body["options"] = v
		}
		if v, ok := args["timeout_seconds"].(float64); ok {
			body["timeout_seconds"] = int(v)
		}
		if v, ok := args["required_reactors"]; ok {
			body["required_reactors"] = v
		}
		out, err2 = s.callServer(ctx, "POST", fmt.Sprintf("/client/cc/channels/%s/approval", handle), body)
	case "duckway_list_local_sessions":
		out, err2 = s.callListLocalSessions(args)
	case "duckway_bind_session":
		out, err2 = s.callBindSession(args)
	case "discord_wait_for_message":
		path := "/client/cc/inbox"
		q := url.Values{}
		if v, ok := args["since"].(float64); ok {
			q.Set("since", fmt.Sprintf("%d", int64(v)))
		}
		if v, ok := args["timeout_seconds"].(float64); ok {
			q.Set("timeout", fmt.Sprintf("%d", int(v)))
		}
		if v, ok := args["channel_handles"].([]interface{}); ok && len(v) > 0 {
			parts := make([]string, 0, len(v))
			for _, x := range v {
				if s, ok := x.(string); ok {
					parts = append(parts, s)
				}
			}
			q.Set("channels", strings.Join(parts, ","))
		}
		if encoded := q.Encode(); encoded != "" {
			path += "?" + encoded
		}
		out, err2 = s.callServer(ctx, "GET", path, nil)
	default:
		return toolError(req.ID, "unknown tool: "+p.Name)
	}

	if err2 != nil {
		return toolError(req.ID, err2.Error())
	}
	text, _ := json.MarshalIndent(out, "", "  ")
	return okResponse(req.ID, map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": string(text)}},
	})
}

func toolError(id json.RawMessage, msg string) jsonrpcResponse {
	return okResponse(id, map[string]interface{}{
		"isError": true,
		"content": []map[string]interface{}{{"type": "text", "text": msg}},
	})
}

// callListLocalSessions scans ~/.claude/projects/ and returns sessions the
// human can pick from. Joins against the local cc-sessions.json so already-
// bound sessions can be hidden (or labeled).
func (s *MCPServer) callListLocalSessions(args map[string]interface{}) (interface{}, error) {
	root := s.claudeProjectsDir
	if root == "" {
		r, err := claudeProjectsRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}

	bound := map[string]string{}
	if s.sessions != nil {
		bound = s.sessions.Snapshot()
	}
	sessions, err := ListLocalSessions(root, bound)
	if err != nil {
		return nil, err
	}

	onlyUnbound := true
	if v, ok := args["only_unbound"].(bool); ok {
		onlyUnbound = v
	}
	cwdFilter, _ := args["cwd_filter"].(string)
	limit := 50
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
		if limit > 200 {
			limit = 200
		}
	}

	filtered := make([]LocalSession, 0, len(sessions))
	for _, sess := range sessions {
		if onlyUnbound && sess.BoundTo != "" {
			continue
		}
		if cwdFilter != "" && !strings.Contains(sess.Cwd, cwdFilter) {
			continue
		}
		filtered = append(filtered, sess)
		if len(filtered) >= limit {
			break
		}
	}

	return map[string]interface{}{
		"sessions":     filtered,
		"total_local":  len(sessions),
		"total_filtered": len(filtered),
		"only_unbound": onlyUnbound,
	}, nil
}

// callBindSession writes the session_id into cc-sessions.json so the
// daemon's NEXT spawn for this channel does `claude --resume <sid>`.
// channel_handle defaults to DUCKWAY_CC_CHANNEL_HANDLE (set by ccRunner
// when claude is invoked for a CC channel).
func (s *MCPServer) callBindSession(args map[string]interface{}) (interface{}, error) {
	sid, _ := args["session_id"].(string)
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	handle, _ := args["channel_handle"].(string)
	if handle == "" {
		handle = os.Getenv("DUCKWAY_CC_CHANNEL_HANDLE")
	}
	if handle == "" {
		return nil, fmt.Errorf("channel_handle not provided and DUCKWAY_CC_CHANNEL_HANDLE is unset — pass channel_handle explicitly")
	}

	if s.sessions == nil {
		return nil, fmt.Errorf("session store unavailable")
	}
	previous := s.sessions.Get(handle)
	if err := s.sessions.Set(handle, sid); err != nil {
		return nil, fmt.Errorf("write cc-sessions.json: %w", err)
	}
	return map[string]interface{}{
		"channel_handle":   handle,
		"session_id":       sid,
		"previous_session": previous, // empty if this was a fresh bind
		"note":             "binding takes effect on the next inbound message; this current turn continues in the existing session",
	}, nil
}

// callServer is a minimal HTTP client for the MCP tools. Uses cfg.Token as
// the X-Duckway-Token header. Returns the decoded JSON body.
func (s *MCPServer) callServer(ctx context.Context, method, path string, body interface{}) (interface{}, error) {
	endpoint := strings.TrimRight(s.cfg.ServerURL, "/") + path
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", s.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("server unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out interface{}
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &out); err != nil {
			out = string(respBody)
		}
	}
	return out, nil
}
