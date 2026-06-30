package client

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	logger      func(format string, args ...interface{})
}

// ccTask is one queued prompt.
type ccTask struct {
	Content     string
	AuthorID    string
	MessageID   string
	ChannelKind string // "management" or "task" — drives prompt injection
}

const (
	ccQueueDepth = 10 // user spec: cap 10
	ccDefaultDir = "cc-workspace"
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

func newCCRunner(handle, configDir, channelCwd string, spec ccAgentSpec, sessions *CCSessionStore, postMessage func(ctx context.Context, handle, content string) error, noTmux bool) (*ccRunner, error) {
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
	extraEnv = append(extraEnv, loadKeysEnv(r.configDir)...)

	r.logger("[cc-watch] %s: running %s (cwd=%s)", r.handle, r.agentName, r.cwd)
	newSID, result, isError, err := r.runFn(ctx, r.bin, r.cwd, prompt, sid, extraEnv)
	if err != nil {
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
