package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// runOne sends a single JSON-RPC line through the server and returns the
// decoded response.
func runOne(t *testing.T, srv *MCPServer, req string) jsonrpcResponse {
	t.Helper()
	in := bytes.NewBufferString(req + "\n")
	out := &bytes.Buffer{}
	if err := srv.Run(context.Background(), in, out); err != nil {
		t.Fatal(err)
	}
	var resp jsonrpcResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, out.String())
	}
	return resp
}

func newTestServer(t *testing.T, state CCStateFile, dwServerURL string) *MCPServer {
	t.Helper()
	tmp := t.TempDir()
	b, _ := json.Marshal(state)
	if err := writeStateFile(tmp, b); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ServerURL: dwServerURL, Token: "test-tok"}
	return NewMCPServer(tmp, cfg)
}

func writeStateFile(dir string, content []byte) error {
	return os.WriteFile(dir+"/cc.json", content, 0600)
}

func TestMCP_Initialize(t *testing.T) {
	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{}}, "")
	resp := runOne(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	r, _ := resp.Result.(map[string]interface{})
	if r["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", r["protocolVersion"])
	}
	si, _ := r["serverInfo"].(map[string]interface{})
	if si["name"] != "duckway-cc" {
		t.Errorf("serverInfo.name = %v", si["name"])
	}
}

func TestMCP_ToolsList(t *testing.T) {
	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{}}, "")
	resp := runOne(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	r, _ := resp.Result.(map[string]interface{})
	tools, _ := r["tools"].([]interface{})
	if len(tools) < 8 {
		t.Errorf("expected at least 8 tools, got %d", len(tools))
	}
	// Verify a known tool by name.
	found := false
	for _, tt := range tools {
		m, _ := tt.(map[string]interface{})
		if m["name"] == "discord_post" {
			found = true
			schema, _ := m["inputSchema"].(map[string]interface{})
			req, _ := schema["required"].([]interface{})
			if len(req) < 2 {
				t.Errorf("discord_post should require channel_handle + content, got %v", req)
			}
		}
	}
	if !found {
		t.Error("discord_post tool not exposed")
	}
}

func TestMCP_GetMyCC(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/cc" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"assigned":true,"cc_id":"cc1","cc_name":"alpha","agent_type":"claude_code","management_handle":"dwch_a"}`))
	}))
	defer mock.Close()

	srv := newTestServer(t, CCStateFile{
		CCs: []CCStateAssignment{{CCID: "cc1", CCName: "alpha"}},
	}, mock.URL)
	resp := runOne(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"discord_get_my_cc","arguments":{}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); isErr {
		t.Fatalf("got error result: %+v", r)
	}
	content, _ := r["content"].([]interface{})
	first, _ := content[0].(map[string]interface{})
	text, _ := first["text"].(string)
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "dwch_a") {
		t.Errorf("expected CC name + handle in output, got: %s", text)
	}
}

func TestMCP_ToolCall_NoCCAssigned(t *testing.T) {
	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{}}, "")
	// discord_post requires a CC. With empty state, it should bail before
	// hitting the server.
	resp := runOne(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"discord_post","arguments":{"channel_handle":"x","content":"hi"}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); !isErr {
		t.Errorf("expected isError=true when no CCs assigned")
	}
}

func TestMCP_ToolCall_PassThrough(t *testing.T) {
	// Stand up a fake duckway server that records the proxied call.
	var hits []string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		if r.Header.Get("X-Duckway-Token") != "test-tok" {
			t.Errorf("missing token: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"message_id":"M-fake-1"}`))
	}))
	defer mock.Close()

	srv := newTestServer(t, CCStateFile{
		CCs: []CCStateAssignment{{CCID: "cc1", CCName: "x"}},
	}, mock.URL)

	resp := runOne(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"discord_post","arguments":{"channel_handle":"H","content":"hello"}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); isErr {
		content, _ := r["content"].([]interface{})
		t.Fatalf("unexpected error: %+v", content)
	}
	if len(hits) != 1 || hits[0] != "POST /client/cc/channels/H/messages" {
		t.Errorf("expected proxied POST, got %v", hits)
	}
	content, _ := r["content"].([]interface{})
	first, _ := content[0].(map[string]interface{})
	text, _ := first["text"].(string)
	if !strings.Contains(text, "M-fake-1") {
		t.Errorf("expected message_id in output, got: %s", text)
	}
}

func TestMCP_ToolCall_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":"client not assigned to this CC"}`)
	}))
	defer mock.Close()

	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{{CCID: "cc1"}}}, mock.URL)
	resp := runOne(t, srv, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"discord_list_channels","arguments":{}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); !isErr {
		t.Error("expected isError=true on 403")
	}
	content, _ := r["content"].([]interface{})
	first, _ := content[0].(map[string]interface{})
	text, _ := first["text"].(string)
	if !strings.Contains(text, "403") {
		t.Errorf("expected status in error: %s", text)
	}
}

func TestMCP_ListLocalSessions_FiltersBound(t *testing.T) {
	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{{CCID: "cc1"}}}, "")

	// Point the server at a synthetic claude-projects tree.
	projects := t.TempDir()
	srv.claudeProjectsDir = projects
	writeFakeSession(t, projects, "-home-me-bound", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", []string{
		`{"type":"user","cwd":"/home/me/bound","message":{"role":"user","content":"already bound"}}`,
	})
	writeFakeSession(t, projects, "-home-me-free", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", []string{
		`{"type":"user","cwd":"/home/me/free","message":{"role":"user","content":"pickable"}}`,
	})
	if err := srv.sessions.Set("dwch_taken", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}

	resp := runOne(t, srv, `{"jsonrpc":"2.0","id":50,"method":"tools/call","params":{"name":"duckway_list_local_sessions","arguments":{}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	content, _ := r["content"].([]interface{})
	first, _ := content[0].(map[string]interface{})
	text, _ := first["text"].(string)
	if strings.Contains(text, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa") {
		t.Errorf("bound session should be hidden by default, got: %s", text)
	}
	if !strings.Contains(text, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb") {
		t.Errorf("unbound session should appear, got: %s", text)
	}

	// only_unbound=false should reveal both.
	resp2 := runOne(t, srv, `{"jsonrpc":"2.0","id":51,"method":"tools/call","params":{"name":"duckway_list_local_sessions","arguments":{"only_unbound":false}}}`)
	r2, _ := resp2.Result.(map[string]interface{})
	c2, _ := r2["content"].([]interface{})
	t2 := c2[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(t2, "aaaaaaaa") || !strings.Contains(t2, "bbbbbbbb") {
		t.Errorf("only_unbound=false should show both, got: %s", t2)
	}
}

func TestMCP_BindSession_WritesStore(t *testing.T) {
	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{{CCID: "cc1"}}}, "")

	// channel_handle is explicit here — no env reliance.
	resp := runOne(t, srv,
		`{"jsonrpc":"2.0","id":60,"method":"tools/call","params":{"name":"duckway_bind_session","arguments":{"session_id":"sess-pick","channel_handle":"dwch_target"}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	if got := srv.sessions.Get("dwch_target"); got != "sess-pick" {
		t.Errorf("store did not record bind: got %q", got)
	}
}

func TestMCP_BindSession_UsesEnvHandleWhenOmitted(t *testing.T) {
	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{{CCID: "cc1"}}}, "")
	t.Setenv("DUCKWAY_CC_CHANNEL_HANDLE", "dwch_envset")

	resp := runOne(t, srv,
		`{"jsonrpc":"2.0","id":61,"method":"tools/call","params":{"name":"duckway_bind_session","arguments":{"session_id":"sess-env"}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	if got := srv.sessions.Get("dwch_envset"); got != "sess-env" {
		t.Errorf("env-default bind failed: got %q", got)
	}
}

func TestMCP_BindSession_RejectsMissingHandle(t *testing.T) {
	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{{CCID: "cc1"}}}, "")
	t.Setenv("DUCKWAY_CC_CHANNEL_HANDLE", "")

	resp := runOne(t, srv,
		`{"jsonrpc":"2.0","id":62,"method":"tools/call","params":{"name":"duckway_bind_session","arguments":{"session_id":"sess-x"}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); !isErr {
		t.Error("expected isError=true when channel_handle missing and env unset")
	}
}

func TestMCP_ParseError(t *testing.T) {
	srv := newTestServer(t, CCStateFile{}, "")
	in := bytes.NewBufferString("not json\n")
	out := &bytes.Buffer{}
	if err := srv.Run(context.Background(), in, out); err != nil {
		t.Fatal(err)
	}
	var resp jsonrpcResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != -32700 {
		t.Errorf("expected parse error: %+v", resp.Error)
	}
}

func TestMCP_NotificationNoResponse(t *testing.T) {
	srv := newTestServer(t, CCStateFile{}, "")
	in := bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	out := &bytes.Buffer{}
	if err := srv.Run(context.Background(), in, out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("notifications must not produce a response, got: %s", out.String())
	}
}

func TestMCP_ImplicitCC(t *testing.T) {
	// CC v2: 1:1 client↔CC, so cc_id is implicit. Tools always hit
	// /client/cc/... with no per-CC segment.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/cc/channels" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`[]`))
	}))
	defer mock.Close()

	srv := newTestServer(t, CCStateFile{CCs: []CCStateAssignment{{CCID: "only-cc"}}}, mock.URL)
	resp := runOne(t, srv, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"discord_list_channels","arguments":{}}}`)
	r, _ := resp.Result.(map[string]interface{})
	if isErr, _ := r["isError"].(bool); isErr {
		t.Fatalf("unexpected error result: %+v", r)
	}
}
