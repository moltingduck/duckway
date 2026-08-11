package client

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTmuxSessionName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"fix-login", "fix-login-duckway"},
		{"feat:auth", "feat-auth-duckway"},
		{"v1.2.3", "v1-2-3-duckway"},
		{"my channel", "my-channel-duckway"},
		{"complex:thing.v2 alpha", "complex-thing-v2-alpha-duckway"},
	}
	for _, tt := range tests {
		got := tmuxSessionName(tt.in)
		if got != tt.want {
			t.Errorf("tmuxSessionName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTmuxLegacySessionName(t *testing.T) {
	if got := tmuxLegacySessionName("feat:auth"); got != "duckway-feat-auth" {
		t.Fatalf("tmuxLegacySessionName = %q", got)
	}
}

func TestEnvValue(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"DUCKWAY_CC_CHANNEL_HANDLE=fix-login",
		"OTHER=value=with=equals",
	}
	if got := envValue(env, "DUCKWAY_CC_CHANNEL_HANDLE"); got != "fix-login" {
		t.Errorf("envValue handle = %q, want fix-login", got)
	}
	if got := envValue(env, "OTHER"); got != "value=with=equals" {
		t.Errorf("envValue OTHER = %q, want value=with=equals", got)
	}
	if got := envValue(env, "MISSING"); got != "" {
		t.Errorf("envValue MISSING = %q, want empty", got)
	}
}

func TestShellSingleQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"simple", "'simple'"},
		{"/path/to/bin", "'/path/to/bin'"},
		{"has space", "'has space'"},
		{"with'apostrophe", `'with'\''apostrophe'`},
		{"", "''"},
	}
	for _, tt := range tests {
		got := shellSingleQuote(tt.in)
		if got != tt.want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsClaudeProcess(t *testing.T) {
	yes := []string{"node", "claude", "claude-cli"}
	no := []string{"sh", "bash", "vim", ""}
	for _, c := range yes {
		if !isClaudeProcess(c) {
			t.Errorf("isClaudeProcess(%q) = false, want true", c)
		}
	}
	for _, c := range no {
		if isClaudeProcess(c) {
			t.Errorf("isClaudeProcess(%q) = true, want false", c)
		}
	}
}

func TestWriteLaunchScript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "launch.sh")
	if err := writeLaunchScript(path, "/usr/local/bin/claude", "abc123", "/tmp/settings.json"); err != nil {
		t.Fatalf("writeLaunchScript: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	s := string(body)
	if !strings.HasPrefix(s, "#!/bin/sh\n") {
		t.Errorf("script missing shebang: %q", s)
	}
	if !strings.Contains(s, "exec '/usr/local/bin/claude'") {
		t.Errorf("script missing exec line: %q", s)
	}
	if !strings.Contains(s, "'--resume' 'abc123'") {
		t.Errorf("script missing --resume: %q", s)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm()&0100 == 0 {
		t.Errorf("script not executable: %v", fi.Mode().Perm())
	}
}

func TestWriteLaunchScriptNoResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "launch.sh")
	if err := writeLaunchScript(path, "/usr/bin/claude", "", "/tmp/settings.json"); err != nil {
		t.Fatalf("writeLaunchScript: %v", err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "--resume") {
		t.Errorf("empty sid should omit --resume; got: %q", string(body))
	}
}

func TestWriteHookScriptEscapesEventsDir(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "hook.sh")
	// eventsDir with a single quote — script must single-quote-escape.
	eventsDir := "/tmp/has'apostrophe/events"
	if err := writeHookScript(hookPath, eventsDir); err != nil {
		t.Fatalf("writeHookScript: %v", err)
	}
	body, _ := os.ReadFile(hookPath)
	if !strings.Contains(string(body), `'/tmp/has'\''apostrophe/events'`) {
		t.Errorf("hook script didn't quote-escape events dir: %q", string(body))
	}
	if !strings.Contains(string(body), `date +%s%N`) {
		t.Errorf("hook script missing nanosecond timestamp: %q", string(body))
	}
	fi, _ := os.Stat(hookPath)
	if fi.Mode().Perm()&0100 == 0 {
		t.Errorf("hook script not executable: %v", fi.Mode().Perm())
	}
}

func TestParseEventFilename(t *testing.T) {
	tests := []struct {
		in     string
		wantTS int64
		wantEv string
		ok     bool
	}{
		{"1747000000123456789.stop.json", 1747000000123456789, "stop", true},
		{"42.foo.json", 42, "foo", true},
		{"notatimestamp.stop.json", 0, "", false},
		{"42.stop", 0, "", false},
		{".json", 0, "", false},
		{"42..json", 0, "", false},
	}
	for _, tt := range tests {
		ts, ev, ok := parseEventFilename(tt.in)
		if ok != tt.ok || ts != tt.wantTS || ev != tt.wantEv {
			t.Errorf("parseEventFilename(%q) = (%d, %q, %v), want (%d, %q, %v)",
				tt.in, ts, ev, ok, tt.wantTS, tt.wantEv, tt.ok)
		}
	}
}

func TestFindStopEventSkipsOlderAndTmp(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Older than afterTS — must be ignored.
	mustWrite("100.stop.json", `{"session_id":"old","last_assistant_message":"old"}`)
	// Half-written file with .tmp extension — must be ignored.
	mustWrite("250.stop.json.tmp", "partial")
	// Wrong event name — ignored.
	mustWrite("300.posttooluse.json", `{}`)
	// Two valid stops; the oldest after afterTS=200 wins.
	mustWrite("400.stop.json", `{"session_id":"a","last_assistant_message":"first"}`)
	mustWrite("500.stop.json", `{"session_id":"b","last_assistant_message":"second"}`)

	evt, found, err := findStopEvent(dir, 200)
	if err != nil {
		t.Fatalf("findStopEvent: %v", err)
	}
	if !found {
		t.Fatal("expected to find a stop event")
	}
	if evt.ts != 400 {
		t.Errorf("ts = %d, want 400", evt.ts)
	}
	if !strings.Contains(evt.payload, `"first"`) {
		t.Errorf("payload = %q, want one containing \"first\"", evt.payload)
	}
}

func TestFindStopEventNoneMatching(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "100.stop.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, found, err := findStopEvent(dir, 200)
	if err != nil || found {
		t.Errorf("findStopEvent on stale-only dir: found=%v err=%v, want (false, nil)", found, err)
	}
}

func TestFindStopEventMissingDir(t *testing.T) {
	_, found, err := findStopEvent("/nonexistent/path/that/should/not/exist", 0)
	if err != nil || found {
		t.Errorf("missing dir: found=%v err=%v, want (false, nil)", found, err)
	}
}

func TestExtractAssistantText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain string", `"hello"`, "hello"},
		{"single text block", `[{"type":"text","text":"hi"}]`, "hi"},
		{"text + tool_use mixed", `[{"type":"text","text":"part1"},{"type":"tool_use","id":"x","name":"bash","input":{}},{"type":"text","text":"part2"}]`, "part1\npart2"},
		{"tool_use only", `[{"type":"tool_use","id":"x","name":"bash","input":{}}]`, ""},
		{"thinking only", `[{"type":"thinking","thinking":"hmm"}]`, ""},
		{"empty array", `[]`, ""},
		{"empty raw", ``, ""},
		{"malformed", `{not json}`, ""},
	}
	for _, tt := range tests {
		got := extractAssistantText([]byte(tt.raw))
		if got != tt.want {
			t.Errorf("%s: extractAssistantText(%q) = %q, want %q", tt.name, tt.raw, got, tt.want)
		}
	}
}

func TestReadLastAssistantMessage(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	// Realistic shape: user turn, assistant tool_use only, tool_result,
	// final assistant with text. The last assistant text wins.
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"all done"}]}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readLastAssistantMessage(transcript)
	if err != nil {
		t.Fatalf("readLastAssistantMessage: %v", err)
	}
	if got != "all done" {
		t.Errorf("got %q, want %q", got, "all done")
	}
}

func TestReadLastAssistantMessageSkipsToolOnlyTurns(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"earlier reply"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}]}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readLastAssistantMessage(transcript)
	if err != nil {
		t.Fatal(err)
	}
	// The last assistant entry was tool_use only (no text), so we should
	// fall back to the previous assistant turn that did have text.
	if got != "earlier reply" {
		t.Errorf("got %q, want %q", got, "earlier reply")
	}
}

func TestResolveAssistantMessagePrefersPayloadField(t *testing.T) {
	// When the payload itself already carries text, no transcript read.
	got := resolveAssistantMessage(stopPayload{LastAssistantMessage: "from payload"})
	if got != "from payload" {
		t.Errorf("got %q, want %q", got, "from payload")
	}
}

func TestResolveAssistantMessageFallsBackToTranscript(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"from transcript"}]}}`
	if err := os.WriteFile(transcript, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := resolveAssistantMessage(stopPayload{TranscriptPath: transcript})
	if got != "from transcript" {
		t.Errorf("got %q, want %q", got, "from transcript")
	}
}

func TestResolveAssistantMessageMissingTranscript(t *testing.T) {
	got := resolveAssistantMessage(stopPayload{TranscriptPath: "/no/such/file"})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestLooksLikePickerInput(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		// Looks like a picker selection.
		{"1", true},
		{"2", true},
		{"99", true},
		{"  3  ", true}, // whitespace tolerated
		{"cancel", true},
		{"Cancel", true},
		{"ESC", true},
		// Doesn't look like a picker selection.
		{"", false},
		{"0", false},   // claude pickers are 1-indexed
		{"100", false}, // too many digits — heuristic guard
		{"-1", false},
		{"hello", false},
		{"what is the weather?", false},
		{"/usage", false}, // fresh slash command
		{"! ls", false},   // fresh bash command
		{"!/help", false}, // unstripped escape
		{"yes", false},
		{"12a", false}, // not a pure integer
	}
	for _, tt := range tests {
		got := looksLikePickerInput(tt.in)
		if got != tt.want {
			t.Errorf("looksLikePickerInput(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestExtractAfterPromptAnchor(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		prompt string
		want   string
	}{
		{
			name:   "anchor present, content follows",
			text:   "welcome\nprior chat\n❯ /usage\nPanel A\nPanel B\nEsc to cancel\n",
			prompt: "/usage",
			want:   "Panel A\nPanel B\nEsc to cancel\n",
		},
		{
			name:   "uses LAST occurrence when prompt appears multiple times",
			text:   "❯ /usage\nold panel\n❯ /usage\nnew panel\n",
			prompt: "/usage",
			want:   "new panel\n",
		},
		{
			name:   "tolerates whitespace around prompt in arg",
			text:   "intro\n❯ /help\nhelp content\n",
			prompt: "  /help  ",
			want:   "help content\n",
		},
		{
			name:   "anchor at very end → empty result",
			text:   "intro\n❯ /usage",
			prompt: "/usage",
			want:   "",
		},
		{
			name:   "anchor missing → falls back to full text",
			text:   "no anchor here\nsome content\n",
			prompt: "/usage",
			want:   "no anchor here\nsome content\n",
		},
		{
			// Bash-mode rendering: claude shows "!  ls" with padding
			// between the indicator and the command. The anchor must be
			// the command portion ("ls"), not the literal prompt.
			name:   "shell mode: anchor on command portion",
			text:   "welcome banner\n!  ls\n  ⎿  CLAUDE.md\n     README.md\n",
			prompt: "! ls",
			want:   "  ⎿  CLAUDE.md\n     README.md\n",
		},
		{
			name:   "shell mode: prompt without space after bang",
			text:   "intro\n! pwd\n  ⎿  /home/user\n",
			prompt: "!pwd",
			want:   "  ⎿  /home/user\n",
		},
		{
			name:   "shell mode: command with spaces, last occurrence wins",
			text:   "echo cargo test ran\n! cargo test\n  ⎿  test passed\n",
			prompt: "! cargo test",
			want:   "  ⎿  test passed\n",
		},
		{
			// Empty prompt (picker-selection case): fall back to the
			// most-recent "❯ /xxx" anchor in scrollback.
			name:   "empty prompt falls back to last slash anchor",
			text:   "welcome\n❯ /release-notes\npicker stuff\n❯ /effort\nlater output\n",
			prompt: "",
			want:   "later output\n",
		},
		{
			name:   "empty prompt with no slash anchor returns full text",
			text:   "just some text\nno anchor here\n",
			prompt: "",
			want:   "just some text\nno anchor here\n",
		},
	}
	for _, tt := range tests {
		got := extractAfterPromptAnchor(tt.text, tt.prompt)
		if got != tt.want {
			t.Errorf("%s:\n  got:  %q\n  want: %q", tt.name, got, tt.want)
		}
	}
}

func TestFormatSlashPaneForDiscord(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"normal", "line1\nline2\n", "```\nline1\nline2\n```"},
		{"trims trailing blanks", "content\n\n\n", "```\ncontent\n```"},
		{"trims leading blanks", "\n\n\ncontent\n", "```\ncontent\n```"},
		{"empty input", "", "_(no panel output captured)_"},
		{"only whitespace", "\n  \n\n", "_(no panel output captured)_"},
		{
			// Bash mode leaves the input box separator + ❯ + footer
			// below the shell output. Strip trailing chrome only.
			name: "strips input-box chrome below shell output",
			in:   "  ⎿ alpha.txt\n     beta.txt\n\n──────────────────\n❯ \n──────────\n  ⏵⏵ bypass permissions on\n",
			want: "```\n  ⎿ alpha.txt\n     beta.txt\n```",
		},
		{
			// MUST NOT strip "──" lines that appear in the middle of
			// content (slash panels include them as visual separators).
			name: "keeps mid-content separators",
			in:   "panel header\n────── section ──────\npanel body\n",
			want: "```\npanel header\n────── section ──────\npanel body\n```",
		},
		{
			// "──" in chrome at the bottom DOES get stripped, but only
			// while walking from the end. Content above survives.
			name: "leading dashes preserved, trailing chrome cut",
			in:   "─── inside content ───\nbody\n\n──────────\n❯ \n",
			want: "```\n─── inside content ───\nbody\n```",
		},
	}
	for _, tt := range tests {
		got := formatSlashPaneForDiscord(tt.in)
		if got != tt.want {
			t.Errorf("%s:\n  got:  %q\n  want: %q", tt.name, got, tt.want)
		}
	}
}

func TestFormatSlashPaneForDiscordTruncates(t *testing.T) {
	huge := strings.Repeat("x", 2500)
	got := formatSlashPaneForDiscord(huge)
	if !strings.Contains(got, "… (truncated)") {
		t.Errorf("expected truncation marker in oversized output; got len=%d", len(got))
	}
	if len(got) > 2000 {
		t.Errorf("output should fit in Discord's 2000-char message limit; got %d", len(got))
	}
}

func TestInFlightRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in-flight.json")
	if err := writeInFlight(path, "fix-login", "msg123", 1700000000); err != nil {
		t.Fatalf("writeInFlight: %v", err)
	}
	f, err := readInFlight(path)
	if err != nil {
		t.Fatalf("readInFlight: %v", err)
	}
	if f.Handle != "fix-login" || f.MessageID != "msg123" || f.TurnTS != 1700000000 {
		t.Errorf("readInFlight got %+v", f)
	}
}

func TestChooseCCRunFnDefaultsToPTYAndCanUseTmux(t *testing.T) {
	// Reset memo so this test reflects current PATH.
	tmuxAvailableMemo = nil
	defer func() { tmuxAvailableMemo = nil }()
	oldDucklionAvailable := ducklionAvailable
	ducklionAvailable = func() bool { return true }
	defer func() { ducklionAvailable = oldDucklionAvailable }()
	ptyRunner := func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
		return "", "pty", false, nil
	}
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", PtyRunFn: ptyRunner, UseTmux: true}

	t.Setenv("DUCKWAY_CC_USE_TMUX", "")
	_, got, _, err := chooseCCRunFn(spec, false)(context.Background(), "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "pty" {
		t.Fatalf("default runner = %q, want pty", got)
	}

	t.Setenv("DUCKWAY_CC_USE_TMUX", "1")
	gotTmux := isRunViaTmux(chooseCCRunFn(spec, false))
	wantTmux := tmuxAvailable()
	if gotTmux != wantTmux {
		t.Errorf("tmux opt-in runner tmux=%v, want %v (tmux on PATH = %v)", gotTmux, wantTmux, wantTmux)
	}

	_, got, _, err = chooseCCRunFn(spec, true)(context.Background(), "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "pty" {
		t.Fatalf("noTmux runner = %q, want pty", got)
	}
}

func TestChooseCCRunFnFallsBackWhenDucklionMissing(t *testing.T) {
	oldDucklionAvailable := ducklionAvailable
	ducklionAvailable = func() bool { return false }
	defer func() { ducklionAvailable = oldDucklionAvailable }()

	headless := func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
		return "", "headless", false, nil
	}
	ptyRunner := func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
		return "", "pty", false, nil
	}
	spec := ccAgentSpec{Type: "codex", DisplayName: "codex", Bin: "/fake/codex", RunFn: headless, PtyRunFn: ptyRunner, UseTmux: true}

	t.Setenv("DUCKWAY_CC_USE_TMUX", "")
	_, got, _, err := chooseCCRunFn(spec, false)(context.Background(), "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "headless" {
		t.Fatalf("ducklion-missing runner = %q, want headless", got)
	}
}

func TestChooseCCRunFnPrefersAgentSpecificTmuxRunFn(t *testing.T) {
	yes := true
	tmuxAvailableMemo = &yes
	defer func() { tmuxAvailableMemo = nil }()

	headless := func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
		return "", "headless", false, nil
	}
	tmuxRunner := func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
		return "", "tmux", false, nil
	}
	spec := ccAgentSpec{
		Type:        "codex",
		DisplayName: "codex",
		Bin:         "/fake/codex",
		RunFn:       headless,
		TmuxRunFn:   tmuxRunner,
		UseTmux:     true,
	}

	t.Setenv("DUCKWAY_CC_USE_TMUX", "1")
	_, got, _, err := chooseCCRunFn(spec, false)(context.Background(), "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tmux" {
		t.Fatalf("chooseCCRunFn(false) result = %q, want tmux", got)
	}

	_, got, _, err = chooseCCRunFn(spec, true)(context.Background(), "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "headless" {
		t.Fatalf("chooseCCRunFn(true) result = %q, want headless", got)
	}
}

// isRunViaTmux returns true when fn is the runViaTmux function.
func isRunViaTmux(fn ccRunFn) bool {
	return reflect.ValueOf(fn).Pointer() == reflect.ValueOf(ccRunFn(runViaTmux)).Pointer()
}
