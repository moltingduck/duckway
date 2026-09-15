package main

import (
	"regexp"
	"strings"
	"testing"
)

type liveAgentDiagnostic struct {
	APIClass, APIStatus                  string
	Permission                           bool
	SleepToolVisible, SleepResultVisible bool
}

var (
	liveDiagnosticANSI        = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]`)
	liveDiagnosticClass       = regexp.MustCompile(`\b(invalid_request_error|overloaded_error|authentication_error|permission_error|not_found_error|rate_limit_error|billing_error|api_error|rate_limit|overloaded|authentication_failed|oauth_org_not_allowed|account_on_hold|invalid_request|model_not_found|server_error|max_output_tokens|cloud_credential_error)\b`)
	liveDiagnosticStatus      = regexp.MustCompile(`(?i)(?:\bapi error\s*:?\s*|"(?:status|status_code|statusCode)"\s*:\s*"?)([45][0-9]{2})\b`)
	liveDiagnosticApproval    = regexp.MustCompile(`(?i)\b(?:do you want to (?:proceed|allow)|allow this (?:command|tool|action)|approve this (?:command|tool|action)|requires? (?:your )?(?:permission|approval)|permission (?:required|denied)|waiting for (?:your )?approval)\b`)
	liveDiagnosticSleepTool   = regexp.MustCompile(`(?i)^\s*[●⏺•]?\s*Bash\(\s*sleep\s+3\s*\)\s*$`)
	liveDiagnosticHumanErrors = []struct {
		category string
		pattern  *regexp.Regexp
	}{
		{"content_filter", regexp.MustCompile(`\bcontext\b[^\r\n]*\bflagged this message\b`)},
		{"aborted", regexp.MustCompile(`\b(?:request (?:was )?aborted|operation was aborted|aborterror|user aborted)\b`)},
		{"interrupted", regexp.MustCompile(`\b(?:request (?:was )?interrupted|response (?:was )?interrupted)\b`)},
		{"empty_response", regexp.MustCompile(`\b(?:empty response|no response (?:received|from)|no (?:assistant )?messages? returned)\b`)},
		{"stream_error", regexp.MustCompile(`\b(?:error (?:while )?streaming|stream(?:ing)? (?:error|failed|interrupted|closed)|premature close|unexpected end of (?:stream|json))\b`)},
		{"connection_error", regexp.MustCompile(`\b(?:connection (?:error|reset|refused|closed)|fetch failed|unable to connect|failed to connect|socket hang up|econnreset|econnrefused|enotfound)\b`)},
		{"timeout", regexp.MustCompile(`\b(?:request (?:timed out|timeout)|connection timed out|etimedout)\b`)},
		{"server_error", regexp.MustCompile(`\binternal server error\b`)},
	}
)

// Only fixed categories and validated HTTP status digits may leave this helper.
// Never return captured free text from a live agent's error or terminal output.
func classifyLiveAgentDiagnostic(remote string) liveAgentDiagnostic {
	plain := liveDiagnosticANSI.ReplaceAllString(remote, "")
	diagnostic := liveAgentDiagnostic{APIClass: "none", APIStatus: "none"}
	if match := liveDiagnosticClass.FindString(strings.ToLower(plain)); match != "" {
		diagnostic.APIClass = match
	}
	// Human-readable transport failures often have neither a numeric status nor
	// an API error type. Scope phrase matching to the error's rendered line so
	// an ordinary footer such as "esc to interrupt" cannot classify a failure.
	if diagnostic.APIClass == "none" {
		lower := strings.ToLower(plain)
		if index := strings.LastIndex(lower, "api error"); index >= 0 {
			errorLine := strings.SplitN(lower[index:], "\n", 2)[0]
			for _, rule := range liveDiagnosticHumanErrors {
				if rule.pattern.MatchString(errorLine) {
					diagnostic.APIClass = rule.category
					break
				}
			}
		}
	}
	if match := liveDiagnosticStatus.FindStringSubmatch(plain); len(match) == 2 {
		diagnostic.APIStatus = match[1]
	}
	diagnostic.Permission = liveDiagnosticApproval.MatchString(plain)
	// These are rendered UI hints, not proof of native tool execution. Only
	// recognize the exact fixture command on a standalone tool heading, so the
	// echoed user prompt cannot count as a tool invocation. Associate a result
	// marker with that heading within at most three following rendered lines.
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		if !liveDiagnosticSleepTool.MatchString(line) {
			continue
		}
		diagnostic.SleepToolVisible = true
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			resultLine := strings.TrimSpace(lines[j])
			if strings.HasPrefix(resultLine, "⎿") {
				diagnostic.SleepResultVisible = true
				break
			}
			if resultLine != "" {
				break
			}
		}
	}
	return diagnostic
}

func TestClassifyLiveAgentSleepToolDiagnostic(t *testing.T) {
	tests := []struct {
		name, remote string
		tool, result bool
	}{
		{"echoed prompt", "Run exactly one foreground shell command `sleep 3`", false, false},
		{"quoted heading", "User: Bash(sleep 3)", false, false},
		{"tool pending", "⏺ Bash(sleep 3)", true, false},
		{"tool result", "⏺ Bash(sleep 3)\n  ⎿  (No content)", true, true},
		{"ansi result", "\x1b[32m● Bash(sleep 3)\x1b[0m\n\n  ⎿ private content", true, true},
		{"different command", "⏺ Bash(sleep 30)\n  ⎿ (No content)", false, false},
		{"unrelated result", "⏺ Bash(sleep 3)\nAPI Error: unknown\n⎿ private content", true, false},
		{"distant result", "⏺ Bash(sleep 3)\n\n\n\n⎿ private content", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLiveAgentDiagnostic(tt.remote)
			if got.SleepToolVisible != tt.tool || got.SleepResultVisible != tt.result {
				t.Fatalf("tool/result markers = %t/%t; want %t/%t", got.SleepToolVisible, got.SleepResultVisible, tt.tool, tt.result)
			}
		})
	}
}

func TestClassifyLiveAgentDiagnostic(t *testing.T) {
	tests := []struct {
		name, remote, class, status string
		permission                  bool
	}{
		{"footer", "API Error: request failed\nplan mode on · shift+tab to cycle permissions", "none", "none", false},
		{"api status", "API Error: 403 {\"error\":{\"type\":\"permission_error\",\"message\":\"private text\"}}", "permission_error", "403", false},
		{"nested status", "API Error: {\"error\":{\"type\":\"invalid_request_error\",\"status\":400}}", "invalid_request_error", "400", false},
		{"quoted status", "{\"status_code\":\"429\",\"type\":\"rate_limit_error\"}", "rate_limit_error", "429", false},
		{"camel status", "{\"statusCode\":503,\"error\":\"overloaded_error\"}", "overloaded_error", "503", false},
		{"ansi", "API \x1b[31mError\x1b[0m: \x1b[1m401\x1b[0m authentication_error", "authentication_error", "401", false},
		{"approval", "Do you want to proceed?", "none", "none", true},
		{"denial", "Permission denied", "none", "none", true},
		{"unknown private error", "API Error: {\"type\":\"secret_custom_error\",\"message\":\"private credential\"}", "none", "none", false},
		{"not status", "LIVE_429_OK API Error: private text 403 {\"status\":200}", "none", "none", false},
		{"invalid long status", "API Error: 403123 {\"status\":5000}", "none", "none", false},
		{"osc excluded", "\x1b]0;authentication_error API Error: 401\x07ready", "none", "none", false},
		{"hook category", "{\"error\":\"oauth_org_not_allowed\"}", "oauth_org_not_allowed", "none", false},
		{"aborted request", "API Error: Request was aborted. secret", "aborted", "none", false},
		{"interrupted response", "API Error: response interrupted", "interrupted", "none", false},
		{"empty response", "API Error: empty response", "empty_response", "none", false},
		{"stream failure", "API Error: error while streaming response", "stream_error", "none", false},
		{"connection failure", "API Error: Connection error. private host", "connection_error", "none", false},
		{"fetch failure", "API Error: fetch failed", "connection_error", "none", false},
		{"timeout", "API Error: Request timed out.", "timeout", "none", false},
		{"server error", "API Error: Internal server error", "server_error", "none", false},
		{"provider content filter", "API Error: context checker flagged this message. private request id", "content_filter", "none", false},
		{"filter phrase outside error", "context checker flagged this message\nAPI Error: unknown", "none", "none", false},
		{"filter phrase following error", "API Error: unknown\ncontext checker flagged this message", "none", "none", false},
		{"interrupt footer", "API Error: unknown\nesc to interrupt", "none", "none", false},
		{"unrelated phrase", "user prompt: request was aborted\nAPI Error: unknown", "none", "none", false},
		{"following line excluded", "API Error: unknown\nuser prompt: request was aborted", "none", "none", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLiveAgentDiagnostic(tt.remote)
			want := liveAgentDiagnostic{APIClass: tt.class, APIStatus: tt.status, Permission: tt.permission}
			if got != want {
				t.Fatalf("diagnostic = %+v, want %+v", got, want)
			}
		})
	}
}
