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
	if p.Name != "discord_get_my_cc" && len(state.CCs) == 0 {
		return toolError(req.ID, "no CC bound to this client — ask the admin to create one in /admin/cc")
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
