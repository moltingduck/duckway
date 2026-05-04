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
func (s *MCPServer) toolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "discord_list_assigned_ccs",
			"description": "List the Discord Control Channels (CCs) this agent is assigned to. Each CC is a (bot, guild category) pair. Returns cc_id + name + agent's home channel handle.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			"name":        "discord_list_channels",
			"description": "List channels under a CC's category. Returns handles (opaque IDs) — never raw Discord channel IDs. Pass the handle to other discord_* tools.",
			"inputSchema": ccArg(),
		},
		{
			"name":        "discord_create_task_channel",
			"description": "Create a new text channel under the CC's category. Use one channel per task so messages stay focused. Returns the new channel handle.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"cc_id": map[string]interface{}{"type": "string", "description": "CC id from discord_list_assigned_ccs. Optional if only one CC is assigned."},
					"name":  map[string]interface{}{"type": "string", "description": "Channel name. Will be lowercased and dashes substituted to fit Discord's rules."},
					"topic": map[string]interface{}{"type": "string", "description": "Optional channel topic shown in Discord's UI."},
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "discord_archive_channel",
			"description": "Archive a task channel (rename + move out of category). Cannot archive the home channel. Idempotent.",
			"inputSchema": handleArg("Archive this channel."),
		},
		{
			"name":        "discord_post",
			"description": "Post a message to a channel. Returns the message_id (use it for edit/delete).",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"cc_id":          map[string]interface{}{"type": "string"},
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
					"cc_id":          map[string]interface{}{"type": "string"},
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
					"cc_id":          map[string]interface{}{"type": "string"},
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
					"cc_id":          map[string]interface{}{"type": "string"},
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
					"cc_id":            map[string]interface{}{"type": "string"},
					"since":            map[string]interface{}{"type": "number", "description": "Cursor from the previous call's response (0 = from the start)."},
					"channel_handles":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional list of handles to filter to."},
					"timeout_seconds":  map[string]interface{}{"type": "number", "description": "0..60 seconds to wait. Default 25."},
				},
			},
		},
	}
}

func ccArg() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"cc_id": map[string]interface{}{"type": "string", "description": "CC id. Optional if only one CC is assigned."},
		},
	}
}

func handleArg(desc string) map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"cc_id":          map[string]interface{}{"type": "string"},
			"channel_handle": map[string]interface{}{"type": "string", "description": desc},
		},
		"required": []string{"channel_handle"},
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

	ccID, _ := args["cc_id"].(string)
	// All tools except `discord_list_assigned_ccs` need a cc_id. Default to
	// the only assigned CC if there is exactly one.
	if p.Name != "discord_list_assigned_ccs" && ccID == "" {
		if len(state.CCs) == 1 {
			ccID = state.CCs[0].CCID
		} else if len(state.CCs) == 0 {
			return toolError(req.ID, "no CCs assigned to this client — ask the admin to assign one")
		} else {
			return toolError(req.ID, "multiple CCs assigned; pass cc_id (call discord_list_assigned_ccs to see them)")
		}
	}
	if p.Name != "discord_list_assigned_ccs" && !ccAssigned(state, ccID) {
		return toolError(req.ID, fmt.Sprintf("cc_id %q not in this client's assignment list", ccID))
	}

	var (
		out interface{}
		err2 error
	)
	switch p.Name {
	case "discord_list_assigned_ccs":
		out = state.CCs
	case "discord_list_channels":
		out, err2 = s.callServer(ctx, "GET", fmt.Sprintf("/client/cc/%s/channels", ccID), nil)
	case "discord_create_task_channel":
		body := map[string]interface{}{
			"name":  args["name"],
			"topic": args["topic"],
		}
		out, err2 = s.callServer(ctx, "POST", fmt.Sprintf("/client/cc/%s/channels", ccID), body)
	case "discord_archive_channel":
		handle, _ := args["channel_handle"].(string)
		out, err2 = s.callServer(ctx, "POST", fmt.Sprintf("/client/cc/%s/channels/%s/archive", ccID, handle), nil)
	case "discord_post":
		handle, _ := args["channel_handle"].(string)
		out, err2 = s.callServer(ctx, "POST", fmt.Sprintf("/client/cc/%s/channels/%s/messages", ccID, handle),
			map[string]interface{}{"content": args["content"]})
	case "discord_edit_message":
		handle, _ := args["channel_handle"].(string)
		mid, _ := args["message_id"].(string)
		out, err2 = s.callServer(ctx, "PATCH", fmt.Sprintf("/client/cc/%s/channels/%s/messages/%s", ccID, handle, mid),
			map[string]interface{}{"content": args["content"]})
	case "discord_delete_message":
		handle, _ := args["channel_handle"].(string)
		mid, _ := args["message_id"].(string)
		out, err2 = s.callServer(ctx, "DELETE", fmt.Sprintf("/client/cc/%s/channels/%s/messages/%s", ccID, handle, mid), nil)
	case "discord_read_recent":
		handle, _ := args["channel_handle"].(string)
		path := fmt.Sprintf("/client/cc/%s/channels/%s/messages", ccID, handle)
		if v, ok := args["limit"].(float64); ok && v > 0 {
			path += fmt.Sprintf("?limit=%d", int(v))
		}
		out, err2 = s.callServer(ctx, "GET", path, nil)
	case "discord_wait_for_message":
		path := fmt.Sprintf("/client/cc/%s/inbox", ccID)
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
		"content": []map[string]interface{}{
			{"type": "text", "text": string(text)},
		},
	})
}

func toolError(id json.RawMessage, msg string) jsonrpcResponse {
	// MCP's convention: report tool errors as an *isError* result, not a
	// JSON-RPC error — that way the model sees the message and can react.
	return okResponse(id, map[string]interface{}{
		"isError": true,
		"content": []map[string]interface{}{
			{"type": "text", "text": msg},
		},
	})
}

func ccAssigned(state *CCStateFile, ccID string) bool {
	for _, a := range state.CCs {
		if a.CCID == ccID {
			return true
		}
	}
	return false
}

// callServer is a minimal HTTP client for the MCP tools. Uses cfg.Token as
// the X-Duckway-Token header. Returns the decoded JSON body.
func (s *MCPServer) callServer(ctx context.Context, method, path string, body interface{}) (interface{}, error) {
	url := strings.TrimRight(s.cfg.ServerURL, "/") + path
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
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
		// Surface the server's structured error so the model can read it.
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out interface{}
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &out); err != nil {
			// Server sent non-JSON — return as plain string.
			out = string(respBody)
		}
	}
	return out, nil
}
