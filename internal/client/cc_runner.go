package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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
}

// ccRunner owns the per-channel FIFO queue + claude exec for one task
// channel. One runner per channel handle; the daemon spawns them lazily
// on first message and tears them down on channel_delete.
//
// Concurrency: messages for the SAME channel are serialized — claude
// can't run two prompts on one session at the same time. Different
// channels run in parallel.
type ccRunner struct {
	handle      string
	configDir   string
	cwd         string // resolved at construction
	agentName   string
	bin         string
	runFn       ccRunFn
	queue       chan ccTask
	stop        chan struct{}
	wg          sync.WaitGroup
	sessions    *CCSessionStore
	postMessage func(ctx context.Context, handle, content string) error // bound to APIClient.PostCC
	reportTest  func(ctx context.Context, testID, status, errText string) error
	statusPosts bool
	logger      func(format string, args ...interface{})
}

// ccTask is one queued prompt.
type ccTask struct {
	Content     string
	AuthorID    string
	MessageID   string
	ChannelKind string // "management" or "task" — drives prompt injection
	TestID      string
}

const (
	ccQueueDepth = 10 // user spec: cap 10
	ccDefaultDir = "cc-workspace"
)

var (
	ccLongRunFirstNotice = 45 * time.Second
	ccLongRunInterval    = 2 * time.Minute
)

// chooseCCRunFn picks the runner to use. tmux gives the user a live,
// attachable per-channel session (`tmux attach -t duckway-<handle>`) so
// they can watch the agent work. When tmux isn't installed — or the user
// explicitly disabled it — we fall back to the headless runner.
func chooseCCRunFn(spec ccAgentSpec, noTmux bool) ccRunFn {
	canUseAgentTmux := spec.UseTmux && !noTmux && tmuxAvailable() && spec.TmuxRunFn != nil
	if spec.RunFn != nil {
		if !canUseAgentTmux {
			return spec.RunFn
		}
	}
	if canUseAgentTmux {
		return spec.TmuxRunFn
	}
	if spec.UseTmux && !noTmux && tmuxAvailable() {
		return runViaTmux
	}
	return runViaPrint
}

func newCCRunner(handle, configDir, channelCwd string, spec ccAgentSpec, sessions *CCSessionStore, postMessage func(ctx context.Context, handle, content string) error, reportTest func(ctx context.Context, testID, status, errText string) error, noTmux bool) (*ccRunner, error) {
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
	r := &ccRunner{
		handle:      handle,
		configDir:   configDir,
		cwd:         cwd,
		agentName:   spec.DisplayName,
		bin:         spec.Bin,
		runFn:       chooseCCRunFn(spec, noTmux),
		queue:       make(chan ccTask, ccQueueDepth),
		stop:        make(chan struct{}),
		sessions:    sessions,
		postMessage: postMessage,
		reportTest:  reportTest,
		statusPosts: true,
		logger:      log.Printf,
	}
	r.wg.Add(1)
	go r.loop()
	return r, nil
}

// Enqueue tries to add a task. Returns false when the buffer is full —
// caller should warn the channel that the message was dropped (per spec
// the user said cap 10, drop excess + notify).
func (r *ccRunner) Enqueue(t ccTask) bool {
	select {
	case r.queue <- t:
		return true
	default:
		return false
	}
}

// Stop drains the in-flight task and exits. Used on channel_delete.
func (r *ccRunner) Stop() {
	close(r.stop)
	r.wg.Wait()
}

func (r *ccRunner) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.stop:
			return
		case t := <-r.queue:
			r.run(t)
		}
	}
}

// run executes one prompt against the configured agent and posts the response back.
func (r *ccRunner) run(t ccTask) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prompt := t.Content
	// Discord/the daemon eat the `/` and `!` trigger characters agents
	// uses for its slash and bash modes, so users escape them with a
	// leading `!`:
	//   "!/..."  → slash command (`/usage`, `/compact`, …)
	//   "!!..."  → shell escape (`! ls`, `! cargo test`, …)
	// Strip the leading `!` so the agent receives the real `/usage` /
	// `! ls`.
	if trimmed := strings.TrimSpace(prompt); strings.HasPrefix(trimmed, "!/") || strings.HasPrefix(trimmed, "!!") {
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
	keysEnv := loadKeysEnv(r.configDir)
	if r.agentName == "codex" && CodexOAuthModeActive(r.configDir) {
		keysEnv = filterEnvByName(keysEnv, "OPENAI_API_KEY")
		if err := validateCodexOAuthAuthJSON(); err != nil {
			r.reportTaskTest(t, "failed", err.Error())
			_ = r.postMessage(context.Background(), r.handle, "❌ "+err.Error())
			return
		}
	}
	extraEnv = append(extraEnv, keysEnv...)

	r.logger("[cc-watch] %s: running %s (cwd=%s)", r.handle, r.agentName, r.cwd)
	r.reportTaskTest(t, "started", "")
	r.postStatus("▶️ " + r.agentName + " started. cwd: `" + r.cwd + "`")
	done := r.startLongRunReporter()
	newSID, result, isError, err := r.runFn(ctx, r.bin, r.cwd, prompt, sid, extraEnv)
	close(done)
	if err != nil {
		r.reportTaskTest(t, "failed", err.Error())
		_ = r.postMessage(context.Background(), r.handle, fmt.Sprintf("%s error: %v", r.agentName, err))
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
	}
	if err := r.postMessage(context.Background(), r.handle, body); err != nil {
		r.logger("[cc-watch] %s: discord post failed: %v", r.handle, err)
		r.reportTaskTest(t, "failed", "discord post failed: "+err.Error())
		return
	}
	r.reportTaskTest(t, "replied", "")
}

func (r *ccRunner) startLongRunReporter() chan struct{} {
	done := make(chan struct{})
	go func() {
		started := time.Now()
		timer := time.NewTimer(ccLongRunFirstNotice)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
			r.postStillRunning(time.Since(started))
		}
		ticker := time.NewTicker(ccLongRunInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.postStillRunning(time.Since(started))
			}
		}
	}()
	return done
}

func (r *ccRunner) postStillRunning(elapsed time.Duration) {
	msg := fmt.Sprintf("⏳ %s is still running after %s. cwd: `%s`", r.agentName, humanDuration(elapsed), r.cwd)
	if tmuxAvailable() && tmuxHasSession(tmuxSessionName(r.handle)) {
		msg += "\nAttach locally with: `tmux attach -t " + tmuxSessionName(r.handle) + "`"
	}
	r.postStatus(msg)
}

func (r *ccRunner) postStatus(content string) {
	if !r.statusPosts || r.postMessage == nil {
		return
	}
	if err := r.postMessage(context.Background(), r.handle, content); err != nil {
		r.logger("[cc-watch] %s: status post failed: %v", r.handle, err)
	}
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	min := int(d / time.Minute)
	sec := int((d % time.Minute) / time.Second)
	if sec == 0 {
		return fmt.Sprintf("%dm", min)
	}
	return fmt.Sprintf("%dm%02ds", min, sec)
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

func validateCodexOAuthAuthJSON() error {
	authPath, err := codexAuthJSONPath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return fmt.Errorf("codex OAuth auth.json is missing; run duckway sync or restart duckway cc watch")
	}
	var auth struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return fmt.Errorf("codex OAuth auth.json is invalid: %w", err)
	}
	if strings.TrimSpace(auth.Tokens.IDToken) == "" {
		return fmt.Errorf("codex OAuth auth.json is missing tokens.id_token; re-upload the full ~/.codex/auth.json in Refreshable Tokens, then run duckway sync or restart duckway cc watch")
	}
	return nil
}

func filterEnvByName(env []string, name string) []string {
	prefix := name + "="
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
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
