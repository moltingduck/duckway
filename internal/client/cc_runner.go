package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// ccRunner owns the per-channel FIFO queue + claude exec for one task
// channel. One runner per channel handle; the daemon spawns them lazily
// on first message and tears them down on channel_delete.
//
// Concurrency: messages for the SAME channel are serialized — claude
// can't run two prompts on one session at the same time. Different
// channels run in parallel.
type ccRunner struct {
	handle      string
	cwd         string  // resolved at construction
	bin         string  // resolved path to `claude` binary
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
	ccQueueDepth   = 10 // user spec: cap 10
	ccDefaultDir   = "cc-workspace"
)

func newCCRunner(handle, configDir, channelCwd, binPath string, sessions *CCSessionStore, postMessage func(ctx context.Context, handle, content string) error) (*ccRunner, error) {
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
		cwd:         cwd,
		bin:         binPath,
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

// run executes one prompt against claude and posts the response back.
func (r *ccRunner) run(t ccTask) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prompt := t.Content
	sid := r.sessions.Get(r.handle)
	// Management-channel preamble: only on the FIRST message of a session.
	// claude remembers the context across --resume turns, so we don't
	// re-inject on every message. The preamble nudges the model to spin
	// out a dedicated task channel via discord_create_task_channel rather
	// than do sustained work in the control channel.
	if t.ChannelKind == "management" && sid == "" {
		prompt = managementPreamble() + "\n\n---\n\n" + t.Content
	}

	args := []string{
		"-p", prompt,
		"--dangerously-skip-permissions",
		"--output-format", "json",
	}
	if sid != "" {
		args = append([]string{"--resume", sid}, args...)
	}

	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Dir = r.cwd
	cmd.Env = append(os.Environ(),
		// Tell the model which channel it's running in so it can
		// discord_post back without guessing.
		"DUCKWAY_CC_CHANNEL_HANDLE="+r.handle,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	r.logger("[cc-watch] %s: running claude (cwd=%s)", r.handle, r.cwd)
	err := cmd.Run()

	if err != nil {
		errMsg := fmt.Sprintf("claude exited with error: %v\n```\n%s\n```", err, tail(stderr.String(), 1500))
		_ = r.postMessage(context.Background(), r.handle, errMsg)
		return
	}

	// claude -p --output-format json prints a single JSON object on
	// success: {type, subtype, session_id, result, is_error, ...}
	var resp struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		Result    string `json:"result"`
		IsError   bool   `json:"is_error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		// Fallback: post the raw output so the user still sees something.
		_ = r.postMessage(context.Background(), r.handle, "claude returned non-JSON output:\n```\n"+tail(stdout.String(), 1800)+"\n```")
		return
	}
	if resp.SessionID != "" {
		_ = r.sessions.Set(r.handle, resp.SessionID)
	}

	body := resp.Result
	if body == "" {
		body = "_(claude finished with no response)_"
	}
	if resp.IsError {
		body = "⚠️ claude reported an error:\n" + body
	}
	if err := r.postMessage(context.Background(), r.handle, body); err != nil {
		r.logger("[cc-watch] %s: discord post failed: %v", r.handle, err)
	}
}

// tail returns the last n bytes of s, prefixed with "…" if truncated.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
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
