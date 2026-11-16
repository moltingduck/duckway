package handlers_test

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestGatewayResponseBytesGolden pins the exact status, content type and body
// the proxy sends for each kind of refusal, so refactoring the decision path
// cannot change what agents and git clients receive.
func TestGatewayResponseBytesGolden(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	do := func(method, path string, hdr map[string]string, body string) (int, string, string) {
		t.Helper()
		req, _ := http.NewRequest(method, e.proxy.URL+"/proxy/github"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+brPlaceholder)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	push := func(caps string, cmds ...[3]string) string { return pushBody(caps, cmds...) }

	type golden struct {
		name, method, path string
		hdr                map[string]string
		body               string
		status             int
		ctype, want        string
	}
	cases := []golden{
		{"acl deny", "GET", "/repos/OWNER/REPO/issues", nil, "", 403, "application/json",
			"{\"error\":\"permission denied (placeholder): endpoint GET /repos/OWNER/REPO/issues not allowed by rule 'GitHub repositories'\"}\n"},
		{"rest branch rule", "POST", "/repos/OWNER/REPO/pulls", nil, `{"title":"t","head":"agent/x","base":"feature/x"}`, 403, "application/json",
			"{\"error\":\"duckway: branch \\\"feature/x\\\" has role \\\"read\\\" under the repository's branch rules; pull requests may only target protected or agent branches\"}\n"},
		{"merge toggle", "PUT", "/repos/OWNER/REPO/pulls/1/merge", nil, `{}`, 403, "application/json",
			"{\"error\":\"duckway: merging pull requests is not permitted for this grant\"}\n"},
		{"unclassified endpoint", "DELETE", "/repos/OWNER/REPO/git/refs", nil, "", 403, "application/json",
			"{\"error\":\"duckway: unsupported write request on git refs\"}\n"},
		{"method override", "POST", "/repos/OWNER/REPO/pulls?_method=DELETE", nil, `{}`, 403, "application/json",
			"{\"error\":\"duckway: method override parameters are not allowed under branch rules\"}\n"},
		{"push no report-status", "POST", "/OWNER/REPO.git/git-receive-pack", nil, push("", [3]string{oidA, oidB, "refs/heads/feature/x"}), 403, "text/plain; charset=utf-8",
			"duckway: branch \"refs/heads/feature/x\" has role \"read\" under the repository's branch rules; writing to it requires the agent role\n"},
		{"push content encoding", "POST", "/OWNER/REPO.git/git-receive-pack", map[string]string{"Content-Encoding": "gzip"}, push("report-status", [3]string{oidA, oidB, "refs/heads/agent/x"}), 403, "text/plain; charset=utf-8",
			"duckway: pushes with a Content-Encoding cannot be checked against branch rules\n"},
		{"push malformed", "POST", "/OWNER/REPO.git/git-receive-pack", nil, "zzzz", 403, "text/plain; charset=utf-8",
			"duckway: malformed pkt-line length in git-receive-pack request\n"},
	}
	for _, g := range cases {
		t.Run(g.name, func(t *testing.T) {
			code, ct, body := do(g.method, g.path, g.hdr, g.body)
			if code != g.status || ct != g.ctype || body != g.want {
				t.Fatalf("got %d %q %q\nwant %d %q %q", code, ct, body, g.status, g.ctype, g.want)
			}
		})
	}

	// A report-status rejection, side-band-64k framed, byte for byte.
	code, ct, body := do("POST", "/OWNER/REPO.git/git-receive-pack", nil,
		push("report-status side-band-64k", [3]string{oidA, oidB, "refs/heads/agent/x"}, [3]string{oidA, oidB, "refs/heads/feature/x"}))
	if code != 200 || ct != "application/x-git-receive-pack-result" {
		t.Fatalf("push rejection: %d %q", code, ct)
	}
	reason := "branch \"refs/heads/feature/x\" has role \"read\" under the repository's branch rules; writing to it requires the agent role"
	var report bytes.Buffer
	report.WriteString(pkt("unpack ok\n"))
	report.WriteString(pkt("ng refs/heads/agent/x duckway: another ref in this push was refused\n"))
	report.WriteString(pkt("ng refs/heads/feature/x duckway: " + reason + "\n"))
	report.WriteString("0000")
	var framed bytes.Buffer
	framed.WriteString(pkt("\x01" + report.String())[:4])
	framed.WriteString("\x01")
	framed.Write(report.Bytes())
	framed.WriteString("0000")
	if body != framed.String() {
		t.Fatalf("rejection bytes:\n got %s\nwant %s", hex.EncodeToString([]byte(body)), hex.EncodeToString(framed.Bytes()))
	}
}
