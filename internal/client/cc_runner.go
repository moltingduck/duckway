package client

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ccRunFn executes one prompt and returns the session ID, result text, whether
// it was an error, and any execution error. Abstracted so tests can inject a
// stub without spawning a real PTY.
type ccRunFn func(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error)

type ccAgentSpec struct {
	Type        string
	DisplayName string
	Bin         string
	RunFn       ccRunFn
	TmuxRunFn   ccRunFn
	UseTmux     bool
	ExtraEnv    []string
}

// ccRunner owns the per-channel FIFO queue + claude exec for one task
// channel. One runner per channel handle; the daemon spawns them lazily
// on first message and tears them down on channel_delete.
//
// Concurrency: messages for the SAME channel are serialized — claude
// can't run two prompts on one session at the same time. Different
// channels run in parallel.
type ccRunner struct {
	handle       string
	configDir    string
	cwd          string // resolved at construction
	agentType    string
	agentName    string
	agentEnv     []string
	bin          string
	runFn        ccRunFn
	runnerMode   string
	debug        bool
	queue        chan ccTask
	stop         chan struct{}
	wg           sync.WaitGroup
	sessions     *CCSessionStore
	processed    *CCProcessedStore
	postMessage  func(ctx context.Context, handle, content string) error // bound to APIClient.PostCC
	postReply    func(ctx context.Context, handle, content, replyToMessageID string) error
	react        func(ctx context.Context, handle, messageID, emoji string) error
	reportTest   func(ctx context.Context, testID, status, errText string) error
	logger       func(format string, args ...interface{})
	mu           sync.Mutex
	activeCancel context.CancelFunc
	activeSeq    int64
	stopped      bool
	recoverStart bool
	reacted      map[string]struct{}
	history      []ccHistoryEntry
}

// ccTask is one queued prompt.
type ccTask struct {
	Content     string
	AuthorID    string
	MessageID   string
	ChannelKind string // "management" or "task" — drives prompt injection
	TestID      string
	// The runner waits for the queued acknowledgement to reach Discord so its
	// running-state reaction cannot overtake the duck reaction.
	queuedReactionDone <-chan struct{}
}

type ccHistoryEntry struct {
	Role string
	Text string
	At   time.Time
}

const (
	ccQueueDepth = 10 // user spec: cap 10
	ccDefaultDir = "cc-workspace"
)

var (
	ccLongRunFirstNotice = 10 * time.Minute
	ccLongRunInterval    = 10 * time.Minute
)

// chooseCCRunFn picks the runner to use. tmux gives the user a live,
// attachable per-channel session (`tmux attach -t <handle>`) so
// they can watch the agent work. When tmux isn't installed — or the user
// explicitly disabled it — we fall back to the headless runner.
func chooseCCRunFn(spec ccAgentSpec, noTmux bool) ccRunFn {
	fn, _ := chooseCCRunFnAndMode(spec, noTmux)
	return fn
}

func chooseCCRunFnAndMode(spec ccAgentSpec, noTmux bool) (ccRunFn, string) {
	canUseAgentTmux := spec.UseTmux && !noTmux && tmuxAvailable() && spec.TmuxRunFn != nil
	if spec.RunFn != nil {
		if !canUseAgentTmux {
			return spec.RunFn, "headless"
		}
	}
	if canUseAgentTmux {
		return spec.TmuxRunFn, "tmux"
	}
	if spec.UseTmux && !noTmux && tmuxAvailable() {
		return runViaTmux, "tmux"
	}
	return runViaPrint, "headless"
}

func newCCRunner(handle, configDir, channelCwd string, spec ccAgentSpec, sessions *CCSessionStore, postMessage func(ctx context.Context, handle, content string) error, postReply func(ctx context.Context, handle, content, replyToMessageID string) error, react func(ctx context.Context, handle, messageID, emoji string) error, reportTest func(ctx context.Context, testID, status, errText string) error, noTmux, debug bool) (*ccRunner, error) {
	return newCCRunnerWithProcessed(handle, configDir, channelCwd, spec, sessions, nil, postMessage, postReply, react, reportTest, noTmux, debug)
}

func newCCRunnerWithProcessed(handle, configDir, channelCwd string, spec ccAgentSpec, sessions *CCSessionStore, processed *CCProcessedStore, postMessage func(ctx context.Context, handle, content string) error, postReply func(ctx context.Context, handle, content, replyToMessageID string) error, react func(ctx context.Context, handle, messageID, emoji string) error, reportTest func(ctx context.Context, testID, status, errText string) error, noTmux, debug bool) (*ccRunner, error) {
	cwd := channelCwd
	if cwd == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		cwd = filepath.Join(home, ".duckway", ccDefaultDir, handle)
		if err := os.MkdirAll(cwd, 0700); err != nil {
			return nil, fmt.Errorf("mkdir cwd %s: %w", cwd, err)
		}
	}
	runFn, runnerMode := chooseCCRunFnAndMode(spec, noTmux)
	recoverStart := false
	if runnerMode == "tmux" {
		_, _, _, recoverStart = pendingInFlightForHandle(handle)
	}
	r := &ccRunner{
		handle:       handle,
		configDir:    configDir,
		cwd:          cwd,
		agentType:    spec.Type,
		agentName:    spec.DisplayName,
		agentEnv:     append([]string(nil), spec.ExtraEnv...),
		bin:          spec.Bin,
		runFn:        runFn,
		runnerMode:   runnerMode,
		debug:        debug,
		queue:        make(chan ccTask, ccQueueDepth),
		stop:         make(chan struct{}),
		sessions:     sessions,
		processed:    processed,
		postMessage:  postMessage,
		postReply:    postReply,
		react:        react,
		reportTest:   reportTest,
		logger:       log.Printf,
		recoverStart: recoverStart,
		reacted:      make(map[string]struct{}),
	}
	r.wg.Add(1)
	go r.loop()
	return r, nil
}

// Enqueue tries to add a task. Returns false when the buffer is full —
// caller should warn the channel that the message was dropped (per spec
// the user said cap 10, drop excess + notify).
func (r *ccRunner) Enqueue(t ccTask) bool {
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		return false
	}
	select {
	case r.queue <- t:
		return true
	default:
		return false
	}
}

// Stop cancels the in-flight task and exits. Used on channel_delete.
func (r *ccRunner) Stop() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		r.wg.Wait()
		return
	}
	r.stopped = true
	close(r.stop)
	if r.activeCancel != nil {
		r.activeCancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *ccRunner) loop() {
	defer r.wg.Done()
	if r.recoverStart {
		r.recoverPendingStart()
	}
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		select {
		case <-r.stop:
			return
		case t := <-r.queue:
			if t.queuedReactionDone != nil {
				select {
				case <-t.queuedReactionDone:
				case <-r.stop:
					return
				}
			}
			r.run(t)
		}
	}
}

func (r *ccRunner) recoverPendingStart() {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		cancel()
		return
	}
	r.activeCancel = cancel
	r.activeSeq++
	activeSeq := r.activeSeq
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.activeSeq == activeSeq {
			r.activeCancel = nil
		}
		r.mu.Unlock()
		cancel()
	}()

	if !tmuxHasChannelSession(r.handle) {
		f, _, eventsDir, ok := pendingInFlightForHandle(r.handle)
		if !ok {
			return
		}
		_, found, err := findStopEvent(eventsDir, f.TurnTS)
		if err != nil || !found {
			r.logger("[cc-watch] %s: stale in-flight marker has no tmux session; leaving marker for startup recovery", r.handle)
			return
		}
	}
	r.logger("[cc-watch] %s: adopting still-running tmux turn before processing queued messages", r.handle)
	result, err := consumePendingTurnEvent(ctx, r.handle)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		r.logger("[cc-watch] %s: recover still-running turn failed: %v", r.handle, err)
		return
	}
	if result == nil || !result.HadResult {
		return
	}
	if result.SessionID != "" {
		_ = r.sessions.Set(r.handle, result.SessionID)
	}
	if r.processed != nil && result.MessageID != "" {
		_ = r.processed.Mark(result.MessageID, r.handle)
	}
	body := result.LastAssistantMessage
	if body == "" {
		body = "_(agent finished with no response)_"
	}
	body = "♻️ (recovered after daemon restart)\n\n" + body
	t := ccTask{MessageID: result.MessageID, ChannelKind: "task"}
	if err := r.postTaskMessage(t, body); err != nil {
		r.logger("[cc-watch] %s: recover post failed: %v", r.handle, err)
		return
	}
	r.reactToTask(t, "✅")
}

// run executes one prompt against the configured agent and posts the response back.
func (r *ccRunner) run(t ccTask) {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		cancel()
		return
	}
	r.activeCancel = cancel
	r.activeSeq++
	activeSeq := r.activeSeq
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.activeSeq == activeSeq {
			r.activeCancel = nil
		}
		r.mu.Unlock()
		cancel()
	}()

	prompt := t.Content
	if cmd, ok := directShellCommand(prompt); ok {
		r.runDirectShellCommand(ctx, t, cmd)
		return
	}
	// Discord/the daemon reserve the `/` trigger character agents use for
	// slash commands, so users escape it with a leading `!`:
	//   "!/..."  → slash command (`/usage`, `/compact`, …)
	// Strip the leading `!` so the agent receives the real `/usage`.
	// `!!...` is handled above as a direct daemon-side shell command.
	if trimmed := strings.TrimSpace(prompt); strings.HasPrefix(trimmed, "!/") {
		prompt = trimmed[1:]
	}
	// `/clear` wipes the agent's running conversation. If we keep the
	// cached session_id mapped, a daemon restart would `--resume` into
	// the old (un-cleared) state. Drop the mapping after a successful
	// turn so the next launch starts fresh.
	clearedSession := strings.HasPrefix(strings.TrimSpace(prompt), "/clear")
	sid := r.sessions.Get(r.handle)
	// Management-channel preamble: only on the FIRST message of a session.
	if t.ChannelKind == "management" && sid == "" {
		prompt = managementPreamble() + "\n\n---\n\n" + prompt
	}

	extraEnv := []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=" + r.handle,
		// Propagate the Discord message id so the tmux runner can record
		// it in the in-flight marker. Recovery after a daemon crash uses
		// this to attribute the recovered Stop event to a specific
		// message.
		"DUCKWAY_CC_MESSAGE_ID=" + t.MessageID,
	}
	extraEnv = append(extraEnv, r.agentEnv...)
	extraEnv = append(extraEnv, agentProxyEnv(r.configDir)...)
	keysEnv := loadKeysEnv(r.configDir)
	if r.agentType == "codex" && codexNativeOAuthConfigured(r.configDir) {
		keysEnv = removeEnv(keysEnv, "OPENAI_API_KEY")
	}
	extraEnv = append(extraEnv, keysEnv...)

	r.logger("[cc-watch] %s: starting agent_type=%s runner_mode=%s %s cwd=%s resume=%v", r.handle, r.agentType, r.runnerMode, r.agentSecurityLogFields(sid), r.cwd, sid != "")
	if r.debug {
		r.logger("[cc-watch] %s: debug cli=%s", r.handle, r.debugCLI(prompt, sid, extraEnv))
	}
	r.appendHistory("user", prompt)
	r.reactToTask(t, "⏳")
	r.reportTaskTest(t, "started", "")
	done := r.startLongRunReporter(t)
	newSID, result, isError, err := r.runFn(ctx, r.bin, r.cwd, prompt, sid, extraEnv)
	close(done)
	if err != nil && sid != "" && isStaleAgentSessionError(err) {
		_ = r.sessions.Drop(r.handle)
		r.logger("[cc-watch] %s: stale %s session %s dropped after resume failure: %v", r.handle, r.agentName, shortForLog(sid), err)
		done = r.startLongRunReporter(t)
		newSID, result, isError, err = r.runFn(ctx, r.bin, r.cwd, prompt, "", extraEnv)
		close(done)
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		r.reportTaskTest(t, "failed", err.Error())
		r.appendHistory("error", err.Error())
		r.reactToTask(t, "⚠️")
		if postErr := r.postTaskMessage(t, fmt.Sprintf("%s error: %v", r.agentName, err)); postErr != nil {
			r.logger("[cc-watch] %s: post error message failed: %v", r.handle, postErr)
		}
		return
	}

	if newSID != "" {
		_ = r.sessions.Set(r.handle, newSID)
	}
	if clearedSession {
		_ = r.sessions.Drop(r.handle)
		r.logger("[cc-watch] %s: /clear sent, dropped cached session_id", r.handle)
	}

	body := result
	if body == "" {
		body = fmt.Sprintf("_(%s finished with no response)_", r.agentName)
	}
	if isError {
		body = fmt.Sprintf("⚠️ %s reported an error:\n%s", r.agentName, body)
		r.appendHistory("assistant_error", result)
	} else {
		r.appendHistory("assistant", result)
	}
	if err := r.postTaskMessage(t, body); err != nil {
		r.logger("[cc-watch] %s: discord post failed: %v", r.handle, err)
		r.reportTaskTest(t, "failed", "discord post failed: "+err.Error())
		r.reactToTask(t, "⚠️")
		return
	}
	if isError {
		r.reactToTask(t, "⚠️")
	} else {
		r.reactToTask(t, "✅")
	}
	r.reportTaskTest(t, "replied", "")
}

func directShellCommand(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "!!") {
		return "", false
	}
	return strings.TrimSpace(trimmed[2:]), true
}

func (r *ccRunner) runDirectShellCommand(ctx context.Context, t ccTask, command string) {
	r.appendHistory("shell", "!! "+command)
	r.reactToTask(t, "⏳")
	r.reportTaskTest(t, "started", "")

	if command == "" {
		r.appendHistory("shell_error", "empty shell command")
		_ = r.postTaskMessage(t, "❌ usage: `!! <shell command>`")
		r.reactToTask(t, "⚠️")
		r.reportTaskTest(t, "failed", "empty shell command")
		return
	}

	output, exitCode, err := r.executeShellCommand(ctx, command)
	body := formatShellCommandResult(command, output, exitCode, err)
	if err != nil {
		r.appendHistory("shell_error", body)
	} else {
		r.appendHistory("shell", body)
	}
	if postErr := r.postTaskMessage(t, body); postErr != nil {
		r.logger("[cc-watch] %s: shell command discord post failed: %v", r.handle, postErr)
		r.reportTaskTest(t, "failed", "discord post failed: "+postErr.Error())
		r.reactToTask(t, "⚠️")
		return
	}
	if err != nil {
		r.reactToTask(t, "⚠️")
		r.reportTaskTest(t, "failed", err.Error())
		return
	}
	r.reactToTask(t, "✅")
	r.reportTaskTest(t, "replied", "")
}

func (r *ccRunner) executeShellCommand(ctx context.Context, command string) (string, int, error) {
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-lc", command)
	cmd.Dir = r.cwd
	cmd.Env = append(os.Environ(),
		"DUCKWAY_CC_CHANNEL_HANDLE="+r.handle,
	)
	cmd.Env = append(cmd.Env, agentProxyEnv(r.configDir)...)
	cmd.Env = append(cmd.Env, loadKeysEnv(r.configDir)...)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0, nil
	}
	exitCode := -1
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	}
	return out.String(), exitCode, err
}

func formatShellCommandResult(command, output string, exitCode int, err error) string {
	status := "exit 0"
	if err != nil {
		if exitCode >= 0 {
			status = fmt.Sprintf("exit %d", exitCode)
		} else {
			status = err.Error()
		}
	}
	output = strings.TrimSpace(output)
	if output == "" {
		output = "(no output)"
	}
	output = strings.ReplaceAll(output, "```", "` ` `")
	return fmt.Sprintf("**Shell** `%s`\n**Status** `%s`\n\n```text\n%s\n```", truncateForDiscordLog(command, 180), status, output)
}

func isStaleAgentSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no rollout found for thread id") ||
		strings.Contains(msg, "thread/resume failed")
}

func shortForLog(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func (r *ccRunner) agentSecurityLogFields(sid string) string {
	switch r.agentType {
	case "codex":
		sandbox := codexSandboxValue(r.agentEnv)
		if !isAllowedCodexSandbox(sandbox) {
			sandbox = "workspace-write"
		}
		style := "none"
		if sandbox != "none" {
			if sid == "" {
				style = "--sandbox"
			} else {
				style = "-c sandbox_mode"
			}
		}
		return fmt.Sprintf("sandbox_mode=%s sandbox_arg_style=%s", sandbox, style)
	case "claude_code":
		return "permission_mode=dangerously-skip-permissions"
	case "openclaw":
		return "permission_mode=openclaw-local-config"
	default:
		return "permission_mode=unknown"
	}
}

func (r *ccRunner) debugCLI(prompt, sid string, extraEnv []string) string {
	promptArg := promptLogSummary(prompt)
	var args []string
	switch r.agentType {
	case "codex":
		args = append([]string{r.bin}, codexCommandArgs(r.cwd, promptArg, sid, extraEnv)...)
	case "claude_code":
		if r.runnerMode == "tmux" {
			args = []string{"tmux", "new-session/respawn-pane", r.bin, "--dangerously-skip-permissions", "[prompt:" + promptArg + "]"}
		} else {
			args = append([]string{r.bin}, claudePrintCommandArgs(promptArg, sid)...)
		}
	case "openclaw":
		handle := envValue(extraEnv, "DUCKWAY_CC_CHANNEL_HANDLE")
		if handle == "" {
			handle = r.handle
		}
		sessionKey := sid
		if sessionKey == "" || !strings.HasPrefix(sessionKey, "duckway:") {
			sessionKey = "duckway:" + handle
		}
		agentID := strings.TrimSpace(os.Getenv("DUCKWAY_CC_OPENCLAW_AGENT"))
		if agentID == "" {
			agentID = "default"
		}
		args = []string{r.bin, "agent", "--agent", agentID, "--session-key", sessionKey, "--message-file", "<temp-prompt-file:" + promptArg + ">", "--json"}
	default:
		args = []string{r.bin, "[prompt:" + promptArg + "]"}
	}
	return shellJoinForLog(args)
}

func promptLogSummary(prompt string) string {
	r := []rune(prompt)
	if len(r) <= 10 {
		return string(r)
	}
	return string(r[:5]) + "..." + string(r[len(r)-5:])
}

func shellJoinForLog(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, shellSingleQuote(a))
	}
	return strings.Join(quoted, " ")
}

func (r *ccRunner) startLongRunReporter(t ccTask) chan struct{} {
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(ccLongRunFirstNotice)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
			r.postLongRunNotice(t, ccLongRunFirstNotice)
		}
		ticker := time.NewTicker(ccLongRunInterval)
		defer ticker.Stop()
		elapsed := ccLongRunFirstNotice
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				elapsed += ccLongRunInterval
				r.postLongRunNotice(t, elapsed)
			}
		}
	}()
	return done
}

func (r *ccRunner) postLongRunNotice(t ccTask, elapsed time.Duration) {
	if t.TestID != "" && !isDiscordSnowflake(t.MessageID) {
		return
	}
	msg := fmt.Sprintf("⏳ Still running after %s. Use `!log` to show the latest agent conversation.", formatDurationForDiscord(elapsed))
	if recent := r.formatRecentHistory(3); recent != "" {
		msg += "\n\n" + recent
	}
	if err := r.postTaskMessage(t, msg); err != nil {
		r.logger("[cc-watch] %s: long-run notice post failed: %v", r.handle, err)
	}
}

func formatDurationForDiscord(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", mins)
}

func (r *ccRunner) postTaskMessage(t ccTask, content string) error {
	if t.TestID != "" && !isDiscordSnowflake(t.MessageID) {
		return nil
	}
	if r.postReply != nil && t.MessageID != "" {
		return r.postReply(context.Background(), r.handle, content, t.MessageID)
	}
	return r.postMessage(context.Background(), r.handle, content)
}

func (r *ccRunner) reactToTask(t ccTask, emoji string) {
	if r.react == nil || !isDiscordSnowflake(t.MessageID) {
		return
	}
	if !r.claimReaction(t.MessageID, emoji) {
		return
	}
	if err := r.react(context.Background(), r.handle, t.MessageID, emoji); err != nil {
		r.logger("[cc-watch] %s: react %s failed: %v", r.handle, emoji, err)
	}
}

func (r *ccRunner) claimReaction(messageID, emoji string) bool {
	key := messageID + "\x00" + emoji
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.reacted[key]; ok {
		return false
	}
	r.reacted[key] = struct{}{}
	return true
}

func (r *ccRunner) appendHistory(role, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	const maxHistory = 50
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history = append(r.history, ccHistoryEntry{Role: role, Text: text, At: time.Now()})
	if len(r.history) > maxHistory {
		r.history = append([]ccHistoryEntry(nil), r.history[len(r.history)-maxHistory:]...)
	}
}

func (r *ccRunner) formatRecentHistory(n int) string {
	if n <= 0 {
		n = 3
	}
	r.mu.Lock()
	hist := append([]ccHistoryEntry(nil), r.history...)
	r.mu.Unlock()
	if len(hist) == 0 {
		return "_(no agent conversation recorded yet)_"
	}
	if len(hist) > n {
		hist = hist[len(hist)-n:]
	}
	var b strings.Builder
	b.WriteString("**Latest agent conversation:**\n")
	for _, h := range hist {
		fmt.Fprintf(&b, "• **%s** %s\n", historyRoleLabel(h.Role), truncateForDiscordLog(h.Text, 700))
	}
	return b.String()
}

func historyRoleLabel(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "agent"
	case "assistant_error":
		return "agent error"
	case "shell":
		return "shell"
	case "shell_error":
		return "shell error"
	case "error":
		return "runner error"
	default:
		return role
	}
}

func truncateForDiscordLog(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max < 1 {
		return ""
	}
	return string(r[:max-1]) + "…"
}

func (r *ccRunner) reportTaskTest(t ccTask, status, errText string) {
	if t.TestID == "" || r.reportTest == nil {
		return
	}
	if err := r.reportTest(context.Background(), t.TestID, status, errText); err != nil {
		r.logger("[cc-watch] report test %s %s failed: %v", t.TestID, status, err)
	}
}

func loadKeysEnv(configDir string) []string {
	data, err := os.ReadFile(KeysEnvPath(configDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, _, ok := strings.Cut(line, "=")
		if !ok || !isShellEnvName(k) {
			continue
		}
		out = append(out, line)
	}
	return out
}

func removeEnv(env []string, key string) []string {
	out := env[:0]
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func codexNativeOAuthConfigured(configDir string) bool {
	if CodexOAuthModeActive(configDir) {
		return false
	}
	authPath, err := codexAuthJSONPath()
	if err != nil {
		return false
	}
	auth, err := os.ReadFile(authPath)
	if err != nil || !strings.Contains(string(auth), `"id_token"`) {
		return false
	}
	configPath, err := codexConfigTOMLPath()
	if err != nil {
		return false
	}
	config, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	return !strings.Contains(string(config), "duckway-openai")
}

func agentProxyEnv(configDir string) []string {
	port := 18080
	if cfg, err := LoadConfig(configDir); err == nil && cfg.ProxyPort > 0 {
		port = cfg.ProxyPort
	}
	proxyURL := LocalProxyURL(port)
	noProxy := "localhost,127.0.0.1"
	if existing := strings.TrimSpace(os.Getenv("NO_PROXY")); existing != "" {
		noProxy = ensureLoopbackInNoProxy(existing)
	}
	return []string{
		"HTTP_PROXY=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL,
		"http_proxy=" + proxyURL,
		"https_proxy=" + proxyURL,
		"NO_PROXY=" + noProxy,
		"no_proxy=" + noProxy,
	}
}

func isShellEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// managementPreamble is prepended to the FIRST message of a session that
// started in the management channel. It nudges the model to spin out a
// dedicated task channel for sustained work instead of replying inline
// in the control surface.
//
// Only emitted once per session — claude keeps the context across
// --resume turns.
func managementPreamble() string {
	return "[Duckway Control Channel — system note]\n" +
		"You are responding in the **management channel** of a Duckway Control Channel. This channel is meant for *control commands* and quick replies, not sustained work. Three guidelines:\n" +
		"\n" +
		"1. **If the user is asking for a focused task** (write code, debug, investigate, etc.), call `discord_create_task_channel(name, topic?, cwd?)` to spin up a dedicated `#channel`, then call `discord_post(channel_handle, ...)` to redirect there. Continue the conversation in that channel — every message there will reach you with `--resume` so context carries over.\n" +
		"2. **If the user just wants a one-shot answer or a status check**, answer directly here.\n" +
		"3. **Discord IDs you'll need**: `DUCKWAY_CC_CHANNEL_HANDLE=" + "[set per turn]" + "` is the management channel's own handle. The task channel handles are returned by `discord_create_task_channel`. Never invent handles.\n" +
		"\n" +
		"The user's message follows the `---` separator below."
}
