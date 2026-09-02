package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CCWatch is the `duckway cc watch` daemon. It connects to the server's
// SSE stream, dispatches incoming Discord task-channel messages to
// per-channel claude runners, and cleans up when channels are deleted.
//
// Reconnects forever with exponential backoff (5s → 10s → 30s → 60s cap)
// so a brief duckway-server outage doesn't kill the daemon.
type CCWatch struct {
	cfg          *Config
	configDir    string
	agentTypes   map[string]string            // cc_id -> agent_type, loaded from cc.json
	agentOptions map[string]map[string]string // cc_id -> sanitized agent-specific options
	sessions     *CCSessionStore
	processed    *CCProcessedStore
	// noTmux forces the headless --print runner even when tmux is installed.
	// Set via `duckway cc watch --no-tmux` or DUCKWAY_CC_NO_TMUX=1.
	noTmux bool
	debug  bool

	mu             sync.Mutex
	runners        map[string]*ccRunner        // agent prompts, by channel handle
	shellRunners   map[string]*ccRunner        // !! commands, by channel handle
	commandRunners map[string]*ccCommandRunner // ! commands, by channel handle
	// clientCommandHandler is a test seam; production uses handleClientCommand.
	clientCommandHandler func(context.Context, []byte)
	sseConnected         bool
	pendingNew           map[string]pendingNewProject
	deleted              map[string]struct{}
	recoverSeen          map[string]struct{}
	stopping             bool

	api *APIClient
}

// CCWatchOptions tweaks the daemon without growing NewCCWatch's signature
// every time we add a knob. Zero value = current defaults.
type CCWatchOptions struct {
	// NoTmux disables the tmux runner unconditionally. Falls back to
	// runViaPrint regardless of whether tmux is on PATH.
	NoTmux bool
	// Debug logs command argv and prompt summaries for agent launches.
	Debug bool
}

func NewCCWatch(configDir string, cfg *Config) (*CCWatch, error) {
	return NewCCWatchWithOptions(configDir, cfg, CCWatchOptions{})
}

func NewCCWatchWithOptions(configDir string, cfg *Config, opts CCWatchOptions) (*CCWatch, error) {
	agentTypes := map[string]string{}
	agentOptions := map[string]map[string]string{}
	if state, err := LoadCCState(configDir); err == nil {
		for _, cc := range state.CCs {
			if cc.CCID != "" && cc.AgentType != "" {
				agentTypes[cc.CCID] = cc.AgentType
				agentOptions[cc.CCID] = sanitizeAgentOptions(cc.AgentType, cc.AgentOptions)
			}
		}
	}
	return &CCWatch{
		cfg:            cfg,
		configDir:      configDir,
		agentTypes:     agentTypes,
		agentOptions:   agentOptions,
		sessions:       NewCCSessionStore(configDir),
		processed:      NewCCProcessedStore(configDir),
		noTmux:         opts.NoTmux,
		debug:          opts.Debug,
		runners:        map[string]*ccRunner{},
		shellRunners:   map[string]*ccRunner{},
		commandRunners: map[string]*ccCommandRunner{},
		pendingNew:     map[string]pendingNewProject{},
		deleted:        map[string]struct{}{},
		recoverSeen:    map[string]struct{}{},
		api:            NewAPIClient(cfg.ServerURL, cfg.Token),
	}, nil
}

// Run is the main loop. Blocks until ctx is cancelled.
func (w *CCWatch) Run(ctx context.Context) error {
	lockFile, err := acquireCCWatchLock(w.configDir)
	if err != nil {
		return err
	}
	defer releaseCCWatchLock(lockFile)
	log.Printf("[cc-watch] starting; server=%s debug=%v no_tmux=%v", w.cfg.ServerURL, w.debug, w.noTmux)
	StartUpdateCheckLoop(ctx, w.cfg, "cc-watch")
	StartControlPlaneLoop(ctx, w.configDir, w.cfg, "cc_watch")

	// Recover any turns whose Stop event arrived while the previous
	// daemon instance was dead. Best-effort: errors are logged and we
	// continue starting up.
	w.reconcileCCState(ctx, "started")
	go w.pollInbox(ctx)

	backoff := 5 * time.Second
	maxBackoff := 60 * time.Second
	for {
		if err := ctx.Err(); err != nil {
			w.shutdown()
			return nil
		}
		if err := w.connectAndStream(ctx); err != nil {
			log.Printf("[cc-watch] stream error: %v (retry in %s)", err, backoff)
		} else if ctx.Err() != nil {
			w.shutdown()
			return nil
		}
		select {
		case <-ctx.Done():
			w.shutdown()
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func acquireCCWatchLock(configDir string) (*os.File, error) {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(configDir, "cc-watch.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("another duckway cc watch is already running for %s", configDir)
		}
		return nil, fmt.Errorf("lock cc-watch: %w", err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return f, nil
}

func releaseCCWatchLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// connectAndStream opens one SSE connection and processes events until
// the connection drops or ctx is cancelled.
func (w *CCWatch) connectAndStream(ctx context.Context) error {
	url := strings.TrimRight(w.cfg.ServerURL, "/") + "/client/cc/events"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Duckway-Token", w.cfg.Token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := directClient.Do(req)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}
	log.Printf("[cc-watch] SSE connected")
	w.onSSEConnected(ctx)

	return w.processSSE(ctx, resp.Body)
}

// processSSE parses the line-by-line SSE format and dispatches events.
// Frames look like:
//
//	event: message_create
//	data: {...json...}
//	(blank line)
//
// We collect `event` + `data` lines until the blank-line terminator,
// then handle.
func (w *CCWatch) processSSE(ctx context.Context, body io.ReadCloser) error {
	r := bufio.NewReader(body)
	var (
		curEvent string
		curData  bytes.Buffer
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("server closed stream")
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			// End of frame.
			if curEvent != "" {
				w.handleEvent(curEvent, curData.Bytes())
			}
			curEvent = ""
			curData.Reset()
		case strings.HasPrefix(line, ":"):
			// Comment frame (heartbeat) — ignore.
		case strings.HasPrefix(line, "event:"):
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if curData.Len() > 0 {
				curData.WriteByte('\n')
			}
			curData.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}

func (w *CCWatch) handleEvent(eventType string, data []byte) {
	switch eventType {
	case "ready":
		log.Printf("[cc-watch] server: ready")
		return
	}
	// Durable MESSAGE_CREATE delivery is claimed from the inbox poller. SSE is
	// deliberately only a wake-up hint so a live event can never skip an older
	// persisted lane head or advance durability before agent completion.
	if eventType == "message_create" {
		var env sseEnvelope
		if json.Unmarshal(data, &env) == nil && env.InboxID > 0 {
			return
		}
	}
	switch eventType {
	case "message_create":
		w.handleMessageCreate(data)
	case "channel_delete":
		w.handleChannelDelete(data)
	case "session_reset":
		w.handleSessionReset(data)
	case "client_command":
		w.enqueueClientCommand(data)
	default:
		// message_update, channel_update, etc — currently ignored. The
		// session model assumes prompts come from message_create only.
	}
}

// sseEnvelope mirrors services.CCEvent.
type sseEnvelope struct {
	Type         string          `json:"type"`
	CCID         string          `json:"cc_id"`
	Handle       string          `json:"channel_handle"`
	Kind         string          `json:"channel_kind"`
	Payload      json.RawMessage `json:"payload"`
	InboxID      int64           `json:"inbox_id,omitempty"`
	ClaimToken   string          `json:"claim_token,omitempty"`
	AttemptCount int             `json:"attempt_count,omitempty"`
}

// payloadMessageCreate is the Discord MESSAGE_CREATE shape we care about.
type payloadMessageCreate struct {
	ID          string                 `json:"id"`
	TestID      string                 `json:"duckway_test_id"`
	Content     string                 `json:"content"`
	Attachments []discordAttachmentRef `json:"attachments"`
	Author      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
	ChannelID string `json:"channel_id"`
}

type discordAttachmentRef struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	ProxyURL    string `json:"proxy_url"`
}

func (w *CCWatch) handleMessageCreate(data []byte) {
	var env sseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		log.Printf("[cc-watch] bad envelope: %v", err)
		return
	}
	if env.Handle == "" {
		return
	}
	var msg payloadMessageCreate
	_ = json.Unmarshal(env.Payload, &msg)
	w.reportAgentTest(msg.TestID, "received", "")
	if msg.Author.Bot {
		// Skip — server filters these too, but be defensive.
		return
	}
	if strings.TrimSpace(msg.Content) == "" && len(msg.Attachments) == 0 {
		return
	}
	messageID := msg.ID
	if !isDiscordSnowflake(messageID) {
		messageID = ""
	}
	finishClaim := func(success bool, errText string) {
		status := "completed"
		if !success {
			status = "admitted"
			if env.AttemptCount >= 5 {
				status = "dead_letter"
			}
		}
		if env.InboxID > 0 {
			if err := w.api.FinishCCInbox(context.Background(), env.InboxID, env.ClaimToken, status, errText); err != nil {
				log.Printf("[cc-watch] finish inbox %d: %v", env.InboxID, err)
			}
		}
		if success && msg.ID != "" {
			_ = w.processed.Mark(msg.ID, env.Handle)
		}
	}
	renewClaim := func() {
		if env.InboxID > 0 {
			if err := w.api.RenewCCInbox(context.Background(), env.InboxID, env.ClaimToken); err != nil {
				log.Printf("[cc-watch] renew inbox %d: %v", env.InboxID, err)
			}
		}
	}

	var runner *ccRunner
	var err error
	content := msg.Content
	if len(msg.Attachments) > 0 {
		augmented, err := w.augmentPromptWithAttachments(context.Background(), env.Handle, msg)
		if err != nil {
			log.Printf("[cc-watch] %s: attachment download failed for message %s: %v", env.Handle, msg.ID, err)
			w.reportAgentTest(msg.TestID, "failed", "attachment download failed: "+err.Error())
			if messageID != "" {
				_ = w.api.ReactCC(context.Background(), env.Handle, messageID, "⚠️")
			}
			if messageID != "" || msg.TestID == "" {
				_ = w.api.PostCC(context.Background(), env.Handle, "⚠️ attachment download failed: "+err.Error())
			}
			finishClaim(false, err.Error())
			return
		}
		content = augmented
	}
	_, isDirectShell := directShellCommand(content)
	if isDirectShell {
		runner, err = w.runnerForDirectShell(env.Handle)
	} else {
		w.syncCodexOAuthForCC(env.CCID)
		runner, err = w.runnerFor(env.Handle, env.CCID)
	}
	if err != nil {
		log.Printf("[cc-watch] cannot start runner for %s: %v", env.Handle, err)
		w.reportAgentTest(msg.TestID, "failed", err.Error())
		if messageID != "" {
			_ = w.api.ReactCC(context.Background(), env.Handle, messageID, "⚠️")
		}
		if messageID != "" || msg.TestID == "" {
			_ = w.api.PostCC(context.Background(), env.Handle, "❌ daemon could not start a session: "+err.Error())
		}
		finishClaim(false, err.Error())
		return
	}
	task := ccTask{Content: content, AuthorID: msg.Author.ID, MessageID: messageID, ChannelKind: env.Kind, TestID: msg.TestID,
		InboxID: env.InboxID, ClaimToken: env.ClaimToken, AttemptCount: env.AttemptCount, finishInbox: finishClaim, renewInbox: renewClaim}
	var queuedReactionDone chan struct{}
	if messageID != "" {
		queuedReactionDone = make(chan struct{})
		task.queuedReactionDone = queuedReactionDone
	}
	if !runner.Enqueue(task) {
		if queuedReactionDone != nil {
			close(queuedReactionDone)
		}
		queueName := "agent"
		if isDirectShell {
			queueName = "shell"
		}
		log.Printf("[cc-watch] %s: %s queue full, dropping message %s", env.Handle, queueName, msg.ID)
		w.reportAgentTest(msg.TestID, "failed", queueName+" queue full")
		if messageID != "" {
			_ = w.api.ReactCC(context.Background(), env.Handle, messageID, "⚠️")
		}
		if messageID != "" || msg.TestID == "" {
			_ = runner.postTaskMessage(task,
				fmt.Sprintf("⚠️ %s queue full (10 messages backed up) — your message was dropped; retry after the current %s work finishes.", queueName, queueName))
		}
		finishClaim(false, queueName+" queue full")
		return
	}
	if messageID != "" {
		if err := w.api.ReactCC(context.Background(), env.Handle, messageID, "🦆"); err != nil {
			log.Printf("[cc-watch] %s: queued reaction failed for message %s: %v", env.Handle, msg.ID, err)
		}
		close(queuedReactionDone)
	}
}

func (w *CCWatch) onSSEConnected(ctx context.Context) {
	w.mu.Lock()
	wasConnected := w.sseConnected
	w.sseConnected = true
	w.mu.Unlock()
	if wasConnected {
		w.reconcileCCState(ctx, "reconnected")
	}
}

// handleSessionReset clears the daemon's cached session_id for a handle
// (server-side `!reset` command). The runner stays alive — only the
// session map entry is dropped, so the next message starts the agent
// without --resume.
func (w *CCWatch) handleSessionReset(data []byte) {
	var env sseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	if env.Handle == "" {
		return
	}
	w.mu.Lock()
	runner := w.runners[env.Handle]
	w.mu.Unlock()
	if runner != nil {
		runner.resetSession()
	} else {
		_ = w.sessions.Drop(env.Handle)
	}
	log.Printf("[cc-watch] %s: session reset", env.Handle)
}

func (w *CCWatch) handleChannelDelete(data []byte) {
	var env sseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	if env.Handle == "" {
		return
	}
	var agentRunner, shellRunner *ccRunner
	var commandRunner *ccCommandRunner
	w.mu.Lock()
	if _, ok := w.deleted[env.Handle]; ok {
		w.mu.Unlock()
		return
	}
	if w.deleted == nil {
		w.deleted = map[string]struct{}{}
	}
	w.deleted[env.Handle] = struct{}{}
	if existing, ok := w.runners[env.Handle]; ok {
		agentRunner = existing
		delete(w.runners, env.Handle)
	}
	if existing, ok := w.shellRunners[env.Handle]; ok {
		shellRunner = existing
		delete(w.shellRunners, env.Handle)
	}
	if existing, ok := w.commandRunners[env.Handle]; ok {
		commandRunner = existing
		delete(w.commandRunners, env.Handle)
	}
	w.mu.Unlock()
	if agentRunner != nil {
		agentRunner.Stop()
	}
	if shellRunner != nil {
		shellRunner.Stop()
	}
	if commandRunner != nil {
		commandRunner.Stop()
	}
	_ = w.sessions.Drop(env.Handle)
	// If this channel had a live tmux pane (tmux runner), kill it now —
	// otherwise dead sessions accumulate every time a Discord channel is
	// deleted. No-op when the session doesn't exist.
	tmuxKillSession(env.Handle)
	log.Printf("[cc-watch] %s: channel deleted, session dropped", env.Handle)
}

// runnerFor returns the runner for a handle, lazily creating one. Looks
// up the channel's cwd from /client/cc/channels (the daemon doesn't
// cache channel metadata; the cost is one HTTP call per first-message).
func (w *CCWatch) runnerFor(handle, ccID string) (*ccRunner, error) {
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return nil, fmt.Errorf("cc-watch is shutting down")
	}
	if _, deleted := w.deleted[handle]; deleted {
		w.mu.Unlock()
		return nil, fmt.Errorf("channel %s was deleted", handle)
	}
	if r, ok := w.runners[handle]; ok {
		w.mu.Unlock()
		return r, nil
	} else {
		w.mu.Unlock()
	}

	cwd, err := w.fetchChannelCwd(handle)
	if err != nil {
		return nil, err
	}
	spec, err := w.agentSpec(ccID)
	if err != nil {
		return nil, err
	}
	r, err := newCCRunnerWithProcessed(handle, w.configDir, cwd, spec, w.sessions, w.processed, w.api.PostCC, w.api.PostCCReply, w.api.ReactCC, w.api.ReportCCAgentTest, w.noTmux, w.debug)
	if err != nil {
		return nil, err
	}
	r.postProgress = w.api.PostCCMessage
	r.editProgress = w.api.EditCCMessage
	w.mu.Lock()
	// Re-check under lock in case we raced.
	if w.stopping {
		w.mu.Unlock()
		r.Stop()
		return nil, fmt.Errorf("cc-watch is shutting down")
	}
	if _, deleted := w.deleted[handle]; deleted {
		w.mu.Unlock()
		r.Stop()
		return nil, fmt.Errorf("channel %s was deleted", handle)
	}
	if existing, ok := w.runners[handle]; ok {
		w.mu.Unlock()
		r.Stop()
		return existing, nil
	}
	w.runners[handle] = r
	w.mu.Unlock()
	return r, nil
}

// runnerForDirectShell returns the channel's independent `!!` worker. Shell
// commands remain FIFO with each other but never wait for an agent prompt.
func (w *CCWatch) runnerForDirectShell(handle string) (*ccRunner, error) {
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return nil, fmt.Errorf("cc-watch is shutting down")
	}
	if _, deleted := w.deleted[handle]; deleted {
		w.mu.Unlock()
		return nil, fmt.Errorf("channel %s was deleted", handle)
	}
	if w.shellRunners == nil {
		w.shellRunners = make(map[string]*ccRunner)
	}
	if r, ok := w.shellRunners[handle]; ok {
		w.mu.Unlock()
		return r, nil
	}
	w.mu.Unlock()

	cwd, err := w.fetchChannelCwd(handle)
	if err != nil {
		return nil, err
	}
	spec := ccAgentSpec{
		Type:        "shell_command",
		DisplayName: "shell",
		RunFn: func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
			return "", "", false, fmt.Errorf("agent runner is not available for this shell-only channel runner")
		},
	}
	r, err := newCCRunnerWithProcessed(handle, w.configDir, cwd, spec, w.sessions, w.processed, w.api.PostCC, w.api.PostCCReply, w.api.ReactCC, w.api.ReportCCAgentTest, w.noTmux, w.debug)
	if err != nil {
		return nil, err
	}
	r.postProgress = w.api.PostCCMessage
	r.editProgress = w.api.EditCCMessage
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		r.Stop()
		return nil, fmt.Errorf("cc-watch is shutting down")
	}
	if _, deleted := w.deleted[handle]; deleted {
		w.mu.Unlock()
		r.Stop()
		return nil, fmt.Errorf("channel %s was deleted", handle)
	}
	if existing, ok := w.shellRunners[handle]; ok {
		w.mu.Unlock()
		r.Stop()
		return existing, nil
	}
	w.shellRunners[handle] = r
	w.mu.Unlock()
	return r, nil
}

func (w *CCWatch) reportAgentTest(testID, status, errText string) {
	if testID == "" {
		return
	}
	if err := w.api.ReportCCAgentTest(context.Background(), testID, status, errText); err != nil {
		log.Printf("[cc-watch] report test %s %s failed: %v", testID, status, err)
	}
}

func (w *CCWatch) syncCodexOAuthForCC(ccID string) {
	if w.agentTypeFor(ccID) != "codex" {
		return
	}
	if err := SyncCodexAuthConfig(w.configDir, w.cfg); err != nil {
		log.Printf("[cc-watch] codex config sync failed: %v", err)
	}
}

func (w *CCWatch) agentSpec(ccID string) (ccAgentSpec, error) {
	agentType, opts := w.agentConfigFor(ccID)
	if agentType == "" {
		agentType = "claude_code"
	}
	switch agentType {
	case "claude_code":
		bin, err := exec.LookPath("claude")
		if err != nil {
			return ccAgentSpec{}, fmt.Errorf("claude binary not found in PATH (install Claude Code first): %w", err)
		}
		return ccAgentSpec{
			Type:        agentType,
			DisplayName: "claude",
			Bin:         bin,
			PtyRunFn:    runViaClaudePTY,
			UseTmux:     true,
		}, nil
	case "codex":
		bin, err := exec.LookPath("codex")
		if err != nil {
			return ccAgentSpec{}, fmt.Errorf("codex binary not found in PATH (install Codex CLI first): %w", err)
		}
		return ccAgentSpec{
			Type:        agentType,
			DisplayName: "codex",
			Bin:         bin,
			RunFn:       runViaCodexExec,
			PtyRunFn:    runViaCodexPTY,
			TmuxRunFn:   runViaCodexTmux,
			UseTmux:     true,
			ExtraEnv:    agentOptionEnv(agentType, opts),
		}, nil
	case "openclaw":
		bin, err := exec.LookPath("openclaw")
		if err != nil {
			return ccAgentSpec{}, fmt.Errorf("openclaw binary not found in PATH (install OpenClaw CLI first): %w", err)
		}
		return ccAgentSpec{
			Type:        agentType,
			DisplayName: "openclaw",
			Bin:         bin,
			RunFn:       runViaOpenClaw,
			UseTmux:     false,
		}, nil
	default:
		return ccAgentSpec{}, fmt.Errorf("agent_type %q is not implemented by cc watch", agentType)
	}
}

func sanitizeAgentOptions(agentType string, opts map[string]string) map[string]string {
	out := map[string]string{}
	switch agentType {
	case "codex":
		sandbox := opts["sandbox"]
		if sandbox == "" {
			sandbox = "workspace-write"
		}
		switch sandbox {
		case "read-only", "workspace-write", "danger-full-access", "none":
			out["sandbox"] = sandbox
		default:
			out["sandbox"] = "workspace-write"
		}
	}
	return out
}

func agentOptionEnv(agentType string, opts map[string]string) []string {
	switch agentType {
	case "codex":
		if sandbox := opts["sandbox"]; sandbox != "" {
			return []string{"DUCKWAY_CC_CODEX_SANDBOX=" + sandbox}
		}
	}
	return nil
}

func (w *CCWatch) fetchChannelCwd(handle string) (string, error) {
	channels, err := w.api.FetchCCChannels()
	if err != nil {
		return "", err
	}
	for _, c := range channels {
		if c.Handle == handle {
			return c.Cwd, nil
		}
	}
	return "", nil // unknown channel — runner uses default cwd
}

func (w *CCWatch) shutdown() {
	var runners []*ccRunner
	var commandRunners []*ccCommandRunner
	w.mu.Lock()
	w.stopping = true
	for h, r := range w.runners {
		runners = append(runners, r)
		delete(w.runners, h)
	}
	for h, r := range w.shellRunners {
		runners = append(runners, r)
		delete(w.shellRunners, h)
	}
	for h, r := range w.commandRunners {
		commandRunners = append(commandRunners, r)
		delete(w.commandRunners, h)
	}
	w.mu.Unlock()
	for _, r := range runners {
		r.Stop()
	}
	for _, r := range commandRunners {
		r.Stop()
	}
	log.Printf("[cc-watch] shutdown complete")
}

func (w *CCWatch) agentTypeFor(ccID string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.agentTypes[ccID]
}

func (w *CCWatch) agentConfigFor(ccID string) (string, map[string]string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	agentType := w.agentTypes[ccID]
	opts := map[string]string{}
	for k, v := range w.agentOptions[ccID] {
		opts[k] = v
	}
	return agentType, opts
}

func (w *CCWatch) pollInbox(ctx context.Context) {
	log.Printf("[cc-watch] durable inbox claim loop ready")
	for {
		ev, err := w.api.ClaimCCInbox(ctx, 3600)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[cc-watch] inbox claim error: %v (retry in 5s)", err)
			if !sleepContext(ctx, 5*time.Second) {
				return
			}
			continue
		}
		if ev == nil {
			if !sleepContext(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		w.handleInboxEvent(*ev)
	}
}

func (w *CCWatch) handleInboxEvent(ev CCInboxEvent) {
	if ev.ChannelHandle == nil || *ev.ChannelHandle == "" {
		_ = w.api.FinishCCInbox(context.Background(), ev.ID, ev.ClaimToken, "dead_letter", "missing channel handle")
		return
	}
	eventType := strings.ToLower(ev.EventType)
	switch ev.EventType {
	case "MESSAGE_CREATE":
		eventType = "message_create"
	case "MESSAGE_UPDATE":
		eventType = "message_update"
	case "MESSAGE_DELETE":
		eventType = "message_delete"
	}
	env := sseEnvelope{
		Type:         eventType,
		CCID:         ev.CCID,
		Handle:       *ev.ChannelHandle,
		Payload:      json.RawMessage(ev.Payload),
		InboxID:      ev.ID,
		ClaimToken:   ev.ClaimToken,
		AttemptCount: ev.AttemptCount,
	}
	data, _ := json.Marshal(env)
	if eventType == "message_create" {
		w.handleMessageCreate(data)
		return
	}
	w.handleEvent(eventType, data)
	_ = w.api.FinishCCInbox(context.Background(), ev.ID, ev.ClaimToken, "completed", "")
}

func ccInboxCursorPath(configDir string) string {
	return filepath.Join(configDir, "cc-inbox-cursor")
}

func loadCCInboxCursor(configDir string) int64 {
	raw, err := os.ReadFile(ccInboxCursorPath(configDir))
	if err != nil {
		return 0
	}
	cursor, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || cursor < 0 {
		return 0
	}
	return cursor
}

func saveCCInboxCursor(configDir string, cursor int64) error {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(ccInboxCursorPath(configDir), []byte(strconv.FormatInt(cursor, 10)+"\n"), 0600)
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type ccReconcileSummary struct {
	Mode           string
	Management     string
	ActiveChannels int
	Recovered      int
	StillRunning   int
}

func (w *CCWatch) reconcileCCState(ctx context.Context, mode string) {
	summary := ccReconcileSummary{Mode: mode}
	if assignments, err := w.api.FetchCC(); err == nil && len(assignments) > 0 {
		summary.Management = assignments[0].ManagementHandle
		w.mu.Lock()
		w.agentTypes = map[string]string{}
		w.agentOptions = map[string]map[string]string{}
		for _, cc := range assignments {
			if cc.CCID != "" && cc.AgentType != "" {
				w.agentTypes[cc.CCID] = cc.AgentType
				w.agentOptions[cc.CCID] = sanitizeAgentOptions(cc.AgentType, cc.AgentOptions)
			}
		}
		w.mu.Unlock()
	} else if err != nil {
		log.Printf("[cc-watch] reconcile fetch cc: %v", err)
	}
	if channels, err := w.api.FetchCCChannels(); err == nil {
		activeHandles := map[string]bool{}
		for _, ch := range channels {
			if ch.Kind == "task" && !ch.Archived {
				summary.ActiveChannels++
			}
			if !ch.Archived {
				activeHandles[ch.Handle] = true
			}
		}
		w.recoverPendingTurns(ctx, &summary, activeHandles)
	} else {
		log.Printf("[cc-watch] reconcile fetch channels: %v", err)
		w.recoverPendingTurns(ctx, &summary, nil)
	}
	if summary.Management != "" {
		_ = w.api.PostCC(ctx, summary.Management, summary.format())
	}
}

func (s ccReconcileSummary) format() string {
	status := "🟢 duckway client started"
	if s.Mode == "reconnected" {
		status = "🟢 duckway client reconnected"
	}
	return fmt.Sprintf("%s\n\nActive task channels: %d\nRecovered replies: %d\nStill running: %d",
		status, s.ActiveChannels, s.Recovered, s.StillRunning)
}

// recoverPendingTurns scans the tmux-runner state files left behind by a
// previous (crashed) daemon. Any turn whose completion event was written while
// the daemon was down gets posted to Discord here, before we connect to
// the SSE stream. Without this, the user's message could have been
// answered by the agent but the reply would never reach the channel.
//
// Best-effort: errors per channel are logged and we keep going.
func (w *CCWatch) recoverPendingTurns(ctx context.Context, summary *ccReconcileSummary, activeHandles map[string]bool) {
	results, err := RecoverPendingTurns()
	if err != nil {
		log.Printf("[cc-watch] recover pending turns: %v", err)
		return
	}
	for _, r := range results {
		if activeHandles != nil && !activeHandles[r.Handle] {
			log.Printf("[cc-watch] recover: discarding pending turn for inactive channel %s", r.Handle)
			tmuxKillSession(r.Handle)
			removePendingInFlight(r.Handle)
			continue
		}
		if !r.HadResult {
			if r.MessageID != "" {
				_ = w.processed.Mark(r.MessageID, r.Handle)
			}
			if summary != nil {
				summary.StillRunning++
			}
			if !w.claimRecoverNotice(r) {
				continue
			}
			log.Printf("[cc-watch] recover: %s has an in-flight turn but no Stop event yet (claude may still be generating)", r.Handle)
			body := "⏳ duckway client reconnected; this agent turn still appears to be running."
			if tmuxAvailable() && tmuxHasChannelSession(r.Handle) {
				body += "\nAttach locally with: `tmux attach -t " + tmuxSessionName(r.Handle) + "`"
			}
			if perr := w.api.PostCC(ctx, r.Handle, body); perr != nil {
				log.Printf("[cc-watch] recover: still-running post to %s failed: %v", r.Handle, perr)
			}
			continue
		}
		body := r.LastAssistantMessage
		if body == "" {
			body = "_(agent finished with no response)_"
		}
		body = "♻️ (recovered after daemon restart)\n\n" + body
		if perr := w.api.PostCC(ctx, r.Handle, body); perr != nil {
			log.Printf("[cc-watch] recover: post to %s failed: %v", r.Handle, perr)
			continue
		}
		if r.SessionID != "" {
			_ = w.sessions.Set(r.Handle, r.SessionID)
		}
		if r.MessageID != "" {
			_ = w.processed.Mark(r.MessageID, r.Handle)
		}
		if summary != nil {
			summary.Recovered++
		}
		log.Printf("[cc-watch] recover: posted reply for in-flight turn on %s (message_id=%s)", r.Handle, r.MessageID)
	}
}

func (w *CCWatch) claimRecoverNotice(r RecoverPendingTurnsResult) bool {
	key := r.Handle + "\x00" + r.MessageID + "\x00" + strconv.FormatInt(r.TurnTS, 10)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.recoverSeen == nil {
		w.recoverSeen = map[string]struct{}{}
	}
	if _, ok := w.recoverSeen[key]; ok {
		return false
	}
	w.recoverSeen[key] = struct{}{}
	return true
}

func isDiscordSnowflake(id string) bool {
	if len(id) < 17 || len(id) > 20 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
