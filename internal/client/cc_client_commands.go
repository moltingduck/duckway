package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/cccommand"
	duckliondaemon "github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

// clientCommandPayload is what the server packs into a `client_command`
// SSE event. The server itself doesn't dispatch these — it just forwards
// them so we can act on the agent's filesystem.
type clientCommandPayload struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type pendingNewProject struct {
	Slug      string
	Topic     string
	Cwd       string
	CreatedAt time.Time
}

// ccCommandRunner serializes daemon-side `!` commands for one channel. It is
// deliberately separate from both agent prompts and `!!` shell commands.
type ccCommandRunner struct {
	queue            chan []byte
	stop             chan struct{}
	wg               sync.WaitGroup
	handle           func(context.Context, []byte)
	mu               sync.Mutex
	stopped          bool
	activeCancel     context.CancelFunc
	overflowNotified bool
}

func newCCCommandRunner(handle func(context.Context, []byte)) *ccCommandRunner {
	r := &ccCommandRunner{
		queue:  make(chan []byte, ccQueueDepth),
		stop:   make(chan struct{}),
		handle: handle,
	}
	r.wg.Add(1)
	go r.loop()
	return r
}

func (r *ccCommandRunner) Enqueue(data []byte) bool {
	data = bytes.Clone(data)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	select {
	case r.queue <- data:
		return true
	default:
		return false
	}
}

func (r *ccCommandRunner) Stop() {
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		close(r.stop)
		if r.activeCancel != nil {
			r.activeCancel()
		}
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *ccCommandRunner) markOverflowNotice() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.overflowNotified {
		return false
	}
	r.overflowNotified = true
	return true
}

func (r *ccCommandRunner) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		select {
		case <-r.stop:
			return
		case data := <-r.queue:
			ctx, cancel := context.WithCancel(context.Background())
			r.mu.Lock()
			if r.stopped {
				r.mu.Unlock()
				cancel()
				return
			}
			r.activeCancel = cancel
			r.overflowNotified = false
			r.mu.Unlock()
			r.handle(ctx, data)
			r.mu.Lock()
			r.activeCancel = nil
			r.mu.Unlock()
			cancel()
		}
	}
}

func (w *CCWatch) enqueueClientCommand(data []byte) {
	var env sseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		log.Printf("[cc-watch] bad client_command envelope: %v", err)
		return
	}
	if env.Handle == "" {
		return
	}

	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return
	}
	if _, deleted := w.deleted[env.Handle]; deleted {
		w.mu.Unlock()
		return
	}
	if w.commandRunners == nil {
		w.commandRunners = make(map[string]*ccCommandRunner)
	}
	runner := w.commandRunners[env.Handle]
	if runner == nil {
		handler := w.clientCommandHandler
		if handler == nil {
			handler = w.handleClientCommandContext
		}
		runner = newCCCommandRunner(handler)
		w.commandRunners[env.Handle] = runner
	}
	w.mu.Unlock()

	if !runner.Enqueue(data) {
		log.Printf("[cc-watch] %s: command queue full, dropping client command", env.Handle)
		if runner.markOverflowNotice() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = w.api.PostCC(ctx, env.Handle,
					"⚠️ command queue full (10 commands backed up) — your command was dropped; retry after the current command finishes.")
			}()
		}
	}
}

// handleClientCommand is the cc-watch entry point for `!sessions` /
// `!bind` (and any future filesystem-only commands). The reply is always
// posted back into the same channel via PostCC.
func (w *CCWatch) handleClientCommand(data []byte) {
	w.handleClientCommandContext(context.Background(), data)
}

func (w *CCWatch) handleClientCommandContext(ctx context.Context, data []byte) {
	var env sseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		log.Printf("[cc-watch] bad client_command envelope: %v", err)
		return
	}
	var payload clientCommandPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		log.Printf("[cc-watch] bad client_command payload: %v", err)
		return
	}
	if env.Handle == "" {
		return
	}
	if err := cccommand.Validate(payload.Command, payload.Args); errors.Is(err, cccommand.ErrUnknownCommand) {
		_ = w.api.PostCC(ctx, env.Handle,
			"❌ daemon doesn't know how to handle `"+payload.Command+"` — update your `duckway` binary on the agent.")
		return
	} else if err != nil {
		msg := "❌ " + err.Error()
		if usage := cccommand.Usage(payload.Command); usage != "" {
			msg += "\nUsage: `" + usage + "`"
		}
		_ = w.api.PostCC(ctx, env.Handle, msg)
		return
	}

	switch payload.Command {
	case "!sessions":
		w.cmdDucklionSessions(ctx, env.Handle, payload.Args)
	case "!bind":
		w.cmdDucklionBind(ctx, env.Handle, payload.Args)
	case "!yield":
		w.cmdYield(ctx, env.Handle, payload.Args)
	case "!projects":
		w.cmdProjects(ctx, env.Handle, payload.Args)
	case "!new":
		w.cmdNewProject(ctx, env.Handle, payload.Args)
	case "!new-confirm":
		w.cmdNewProjectConfirm(ctx, env.Handle, payload.Args)
	case "!log":
		w.cmdLog(ctx, env.Handle, payload.Args)
	case "!duckway-version":
		w.cmdDuckwayVersion(ctx, env.Handle, payload.Args)
	case "!duckway-doctor":
		w.cmdDuckwayDoctor(ctx, env.Handle, payload.Args)
	case "!duckway-restart":
		w.cmdDuckwayRestart(ctx, env.Handle, payload.Args)
	case "!duckway-update":
		w.cmdDuckwayUpdate(ctx, env.Handle, payload.Args)
	default:
		_ = w.api.PostCC(ctx, env.Handle,
			"❌ daemon doesn't know how to handle `"+payload.Command+"` — update your `duckway` binary on the agent.")
	}
}

func (w *CCWatch) dialDucklionCC(handle string) (*duckliondaemon.Client, error) {
	socketPath := filepath.Join(w.configDir, "ducklion", "ducklion.sock")
	return duckliondaemon.DialCC(socketPath, handle)
}

func (w *CCWatch) cmdYield(ctx context.Context, replyHandle string, args []string) {
	wait := len(args) == 1 && (args[0] == "-w" || args[0] == "--wait")
	client, err := w.dialDucklionCC(replyHandle)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ Ducklion is unavailable, so control was not transferred: "+err.Error())
		return
	}
	defer client.Close()
	binding, err := client.CurrentDiscordBinding(ctx)
	if err != nil {
		if isDucklionNotFound(err) {
			_ = w.api.PostCC(ctx, replyHandle, "❌ This Discord channel is not bound to a Ducklion session.")
		} else {
			_ = w.api.PostCC(ctx, replyHandle, "❌ Could not resolve this channel's Ducklion binding: "+err.Error())
		}
		return
	}
	sessions, err := client.ListSessions()
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ Could not read the current session state: "+err.Error())
		return
	}
	var selected *protocol.SessionSummary
	for i := range sessions {
		if sessions[i].SessionID == binding.SessionID {
			selected = &sessions[i]
			break
		}
	}
	if selected == nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ The bound Ducklion session no longer exists.")
		return
	}
	operationID := uuid.NewString()
	result, err := client.YieldSessionWithID(ctx, operationID, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, wait)
	if err != nil && !isAuthoritativeDucklionError(err) {
		if retry, dialErr := w.dialDucklionCC(replyHandle); dialErr == nil {
			result, err = retry.YieldSessionWithID(ctx, operationID, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, wait)
			_ = retry.Close()
		}
	}
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, formatCCYieldError(err, wait))
		return
	}
	switch result.Decision {
	case "waiting":
		_ = w.api.PostCC(ctx, replyHandle, "⏳ Control transfer queued. Discord will become the writer as soon as the current task finishes.")
	case "unchanged":
		_ = w.api.PostCC(ctx, replyHandle, "ℹ️ Discord already controls this session.")
	default:
		_ = w.api.PostCC(ctx, replyHandle, fmt.Sprintf("✅ Discord now owns session `%s` (epoch %d).", result.SessionID, result.OwnershipEpoch))
	}
}

func formatCCYieldError(err error, wait bool) string {
	var remote *duckliondaemon.RemoteError
	if errors.As(err, &remote) {
		switch remote.Detail.Code {
		case protocol.ErrTaskActive:
			return "❌ A task is still running. Use `!yield -w` to take control immediately after it finishes."
		case protocol.ErrPendingYield:
			return "❌ Another controller is already waiting for this session."
		case protocol.ErrAdapterUnhealthy:
			return "❌ This session's agent adapter is unavailable; Ducklion cannot safely determine whether it is idle."
		}
	}
	mode := "yield"
	if wait {
		mode = "wait-yield"
	}
	return "❌ Ducklion rejected the " + mode + " request: " + err.Error()
}

func (w *CCWatch) cmdDucklionSessions(ctx context.Context, replyHandle string, args []string) {
	client, err := w.dialDucklionCC(replyHandle)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ Ducklion is unavailable: "+err.Error())
		return
	}
	defer client.Close()
	sessions, err := client.ListSessions()
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ Could not list Ducklion sessions: "+err.Error())
		return
	}
	filter := strings.TrimSpace(strings.Join(args, " "))
	var rows []protocol.SessionSummary
	for _, session := range sessions {
		if session.Kind != "agent" || session.ChannelHandle != "" || filter != "" && !strings.Contains(session.CWD, filter) {
			continue
		}
		rows = append(rows, session)
	}
	if len(rows) == 0 {
		_ = w.api.PostCC(ctx, replyHandle, "_(no unbound Ducklion agent sessions found)_")
		return
	}
	var b strings.Builder
	b.WriteString("**Unbound Ducklion agent sessions:**\n")
	for _, session := range rows {
		fmt.Fprintf(&b, "• `%s` — **%s** · `%s` · %s/%s · owner `%s:%s`\n", session.SessionID, session.Handle, session.CWD,
			session.Status, session.AdapterState, ownerKind(session.Writer), ownerID(session.Writer))
	}
	b.WriteString("\nUse `!bind <session-id>` to create a read-only Discord task channel. Use `!yield` there when you want Discord to take control.")
	_ = w.api.PostCC(ctx, replyHandle, b.String())
}

func ownerKind(owner *model.Owner) string {
	if owner == nil {
		return "shared"
	}
	return string(owner.Kind)
}

func ownerID(owner *model.Owner) string {
	if owner == nil {
		return "-"
	}
	return owner.ID
}

func (w *CCWatch) cmdDucklionBind(ctx context.Context, replyHandle string, args []string) {
	client, err := w.dialDucklionCC(replyHandle)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ Ducklion is unavailable: "+err.Error())
		return
	}
	defer client.Close()
	sessions, err := client.ListSessions()
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ Could not list Ducklion sessions: "+err.Error())
		return
	}
	byID := make(map[string]protocol.SessionSummary, len(sessions))
	for _, session := range sessions {
		byID[session.SessionID] = session
	}
	results := make([]BindResult, 0, len(args))
	for _, id := range args {
		session, ok := byID[id]
		result := BindResult{SessionID: id}
		if !ok || session.Kind != "agent" {
			result.Error = "managed agent session not found"
			results = append(results, result)
			continue
		}
		result.Cwd = session.CWD
		if session.ChannelHandle != "" {
			result.AlreadyBound = session.ChannelHandle
			results = append(results, result)
			continue
		}
		created, createErr := w.api.CreateCCChannel(ctx, discordChannelNameFromCwd(session.Handle), "Ducklion session "+session.SessionID, session.CWD)
		if createErr != nil {
			result.Error = "create channel: " + createErr.Error()
			results = append(results, result)
			continue
		}
		// Reserve routing before activation. From this point, inbound prompts
		// fail closed even if Ducklion or the local cache is temporarily lost.
		if markerErr := w.api.SetCCChannelSession(ctx, created.Handle, session.SessionID, session.CWD); markerErr != nil {
			_ = w.api.ArchiveCCChannel(ctx, created.Handle)
			result.Error = "reserve channel binding: " + markerErr.Error()
			results = append(results, result)
			continue
		}
		// The server marker is authoritative; a local cache failure must not
		// strand the reservation before Ducklion activation.
		_ = w.sessions.Set(created.Handle, session.SessionID)
		operationID := uuid.NewString()
		binding, bindErr := client.BindDiscordSession(ctx, operationID, session.SessionID, created.Handle)
		if bindErr != nil {
			// A transport failure may mean the commit succeeded and only its
			// response was lost. Reconnect and reconcile before any cleanup.
			if check, dialErr := w.dialDucklionCC(replyHandle); dialErr == nil {
				// Retry the same mutation ID first: Ducklion either replays a
				// committed result or performs the request that was never received.
				binding, bindErr = check.BindDiscordSession(ctx, operationID, session.SessionID, created.Handle)
				committed, lookupErr := check.DiscordBindingForSession(ctx, session.SessionID)
				_ = check.Close()
				if bindErr == nil {
					committed, lookupErr = binding, nil
				}
				if lookupErr == nil && committed.ChannelHandle == created.Handle {
					binding, bindErr = committed, nil
				} else if isAuthoritativeDucklionError(bindErr) && (isDucklionNotFound(lookupErr) || lookupErr == nil && committed.ChannelHandle != created.Handle) {
					_ = w.api.SetCCChannelSession(ctx, created.Handle, "", session.CWD)
					_ = w.api.ArchiveCCChannel(ctx, created.Handle)
					if lookupErr == nil && committed.ChannelHandle != "" {
						result.AlreadyBound = committed.ChannelHandle
					}
				}
			}
			if bindErr != nil {
				if result.AlreadyBound != "" {
					results = append(results, result)
					continue
				}
				result.Error = "binding outcome unknown; channel was preserved and prompts will remain queued: " + bindErr.Error()
				results = append(results, result)
				continue
			}
		}
		if cacheErr := w.sessions.Set(binding.ChannelHandle, binding.SessionID); cacheErr != nil {
			result.Error = "binding activated, but local cache could not be written: " + cacheErr.Error()
			results = append(results, result)
			continue
		}
		result.Channel, result.Name = binding.ChannelHandle, created.Name
		results = append(results, result)
	}
	_ = w.api.PostCC(ctx, replyHandle, formatBindReport(results))
}

func isDucklionNotFound(err error) bool {
	var remote *duckliondaemon.RemoteError
	return errors.As(err, &remote) && remote.Detail.Code == protocol.ErrNotFound
}

func isAuthoritativeDucklionError(err error) bool {
	var remote *duckliondaemon.RemoteError
	return errors.As(err, &remote) && !remote.Detail.Retryable
}

func (w *CCWatch) cmdLog(ctx context.Context, replyHandle string, args []string) {
	n, err := parseLogCount(args)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ usage: `!log [N]`")
		return
	}
	w.mu.Lock()
	agentRunner := w.runners[replyHandle]
	shellRunner := w.shellRunners[replyHandle]
	w.mu.Unlock()
	if agentRunner == nil && shellRunner == nil {
		_ = w.api.PostCC(ctx, replyHandle, "_(no agent runner has started for this channel yet)_")
		return
	}
	var history []ccHistoryEntry
	if agentRunner != nil {
		history = append(history, agentRunner.historySnapshot()...)
	}
	if shellRunner != nil {
		history = append(history, shellRunner.historySnapshot()...)
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].At.Before(history[j].At) })
	_ = w.api.PostCC(ctx, replyHandle, formatRecentHistory(history, n))
}

func parseLogCount(args []string) (int, error) {
	const (
		defaultLogCount = 3
		maxLogCount     = 20
	)
	if len(args) == 0 {
		return defaultLogCount, nil
	}
	joined := strings.TrimSpace(strings.Join(args, " "))
	if joined == "" {
		return defaultLogCount, nil
	}
	parts := strings.Fields(joined)
	if len(parts) == 2 && strings.EqualFold(parts[0], "last") {
		parts = parts[1:]
	}
	if len(parts) != 1 {
		return 0, fmt.Errorf("invalid log count")
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil || n <= 0 || n > maxLogCount {
		return 0, fmt.Errorf("invalid log count")
	}
	return n, nil
}

func (w *CCWatch) cmdProjects(ctx context.Context, replyHandle string, args []string) {
	filter := strings.TrimSpace(strings.Join(args, " "))
	projects, err := NewCCProjectStore(w.configDir).List()
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ read projects failed: "+err.Error())
		return
	}
	if filter != "" {
		var filtered []CCProject
		for _, p := range projects {
			if strings.Contains(p.Name, filter) || strings.Contains(p.Path, filter) {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}
	_ = w.api.PostCC(ctx, replyHandle, formatProjectsReport(projects, filter))
}

func (w *CCWatch) cmdNewProject(ctx context.Context, replyHandle string, args []string) {
	slug, flags, err := splitClientSlugAndFlags(args)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ "+err.Error()+"\nUsage: `!new <slug> --project <name|number> [--topic <text>]` or `!new <slug> --cwd <path> [--topic <text>]`")
		return
	}
	projectRef := strings.TrimSpace(flags["project"])
	cwdRef := strings.TrimSpace(flags["cwd"])
	if projectRef != "" && cwdRef != "" {
		_ = w.api.PostCC(ctx, replyHandle, "❌ choose either `--project` or `--cwd`, not both.")
		return
	}
	if projectRef == "" && cwdRef == "" {
		_ = w.api.PostCC(ctx, replyHandle, "❌ daemon only handles `!new` when `--project <name|number>` or `--cwd <path>` is set.")
		return
	}

	if cwdRef != "" {
		w.cmdNewWithCwd(ctx, replyHandle, slug, flags["topic"], cwdRef)
		return
	}

	project, err := NewCCProjectStore(w.configDir).Resolve(projectRef)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ "+err.Error()+" — run `!projects` to see saved projects.")
		return
	}
	created, err := w.createProjectChannel(ctx, slug, flags["topic"], project.Path)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ create channel: "+err.Error())
		return
	}
	_ = w.api.PostCC(ctx, replyHandle,
		"✅ Created **#"+created.Name+"** — `"+created.Handle+"`\n"+
			"   project: `"+project.Name+"`\n"+
			"   cwd: `"+project.Path+"`\n"+
			"   Send a message in that channel to start the agent.")
}

func (w *CCWatch) cmdNewWithCwd(ctx context.Context, replyHandle, slug, topic, cwdRef string) {
	cwd, err := normalizeProjectPattern(cwdRef)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ invalid cwd: "+err.Error())
		return
	}
	info, err := os.Stat(cwd)
	if err == nil {
		if !info.IsDir() {
			_ = w.api.PostCC(ctx, replyHandle, "❌ cwd exists but is not a directory: `"+cwd+"`")
			return
		}
		created, err := w.createProjectChannel(ctx, slug, topic, cwd)
		if err != nil {
			_ = w.api.PostCC(ctx, replyHandle, "❌ create channel: "+err.Error())
			return
		}
		_ = w.api.PostCC(ctx, replyHandle,
			"✅ Created **#"+created.Name+"** — `"+created.Handle+"`\n"+
				"   cwd: `"+cwd+"`\n"+
				"   Send a message in that channel to start the agent.")
		return
	}
	if !os.IsNotExist(err) {
		_ = w.api.PostCC(ctx, replyHandle, "❌ inspect cwd failed: "+err.Error())
		return
	}

	token := randomConfirmToken()
	w.mu.Lock()
	if w.pendingNew == nil {
		w.pendingNew = map[string]pendingNewProject{}
	}
	w.prunePendingNewLocked(time.Now())
	w.pendingNew[token] = pendingNewProject{Slug: slug, Topic: topic, Cwd: cwd, CreatedAt: time.Now()}
	w.mu.Unlock()
	_ = w.api.PostCC(ctx, replyHandle,
		"⚠️ Project folder does not exist:\n`"+cwd+"`\n\n"+
			"Create it, add it to saved projects, and open the task channel?\n"+
			"Reply with `!new-confirm "+token+"` within 30 minutes.")
}

func (w *CCWatch) cmdNewProjectConfirm(ctx context.Context, replyHandle string, args []string) {
	if len(args) != 1 {
		_ = w.api.PostCC(ctx, replyHandle, "❌ usage: `!new-confirm <token>`")
		return
	}
	token := strings.TrimSpace(args[0])
	w.mu.Lock()
	if w.pendingNew == nil {
		w.pendingNew = map[string]pendingNewProject{}
	}
	w.prunePendingNewLocked(time.Now())
	pending, ok := w.pendingNew[token]
	if ok {
		delete(w.pendingNew, token)
	}
	w.mu.Unlock()
	if !ok {
		_ = w.api.PostCC(ctx, replyHandle, "❌ no pending `!new` request for that token. Run `!new ... --cwd ...` again.")
		return
	}
	if err := os.MkdirAll(pending.Cwd, 0700); err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ create folder failed: "+err.Error())
		return
	}
	added, err := NewCCProjectStore(w.configDir).Add([]string{pending.Cwd}, "")
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ add project failed: "+err.Error())
		return
	}
	projectName := filepath.Base(pending.Cwd)
	if len(added) > 0 {
		projectName = added[0].Name
	}
	created, err := w.createProjectChannel(ctx, pending.Slug, pending.Topic, pending.Cwd)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ create channel: "+err.Error())
		return
	}
	_ = w.api.PostCC(ctx, replyHandle,
		"✅ Created folder, saved project **"+projectName+"**, and opened **#"+created.Name+"** — `"+created.Handle+"`\n"+
			"   cwd: `"+pending.Cwd+"`\n"+
			"   Send a message in that channel to start the agent.")
}

func (w *CCWatch) createProjectChannel(ctx context.Context, slug, topic, cwd string) (*CreateCCChannelResult, error) {
	return w.api.CreateCCChannel(ctx, slug, topic, cwd)
}

func (w *CCWatch) prunePendingNewLocked(now time.Time) {
	for token, p := range w.pendingNew {
		if now.Sub(p.CreatedAt) > 30*time.Minute {
			delete(w.pendingNew, token)
		}
	}
}

func randomConfirmToken() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// cmdSessions lists local claude sessions that aren't already bound to a
// CC channel. Optional positional arg = cwd substring filter.
func (w *CCWatch) cmdSessions(ctx context.Context, replyHandle string, args []string) {
	root, err := claudeProjectsRoot()
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ couldn't locate ~/.claude/projects: "+err.Error())
		return
	}
	bound := w.sessions.Snapshot()
	all, err := ListLocalSessions(root, bound)
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ scan failed: "+err.Error())
		return
	}

	cwdFilter := strings.TrimSpace(strings.Join(args, " "))
	var unbound []LocalSession
	for _, s := range all {
		if s.BoundTo != "" {
			continue
		}
		if cwdFilter != "" && !strings.Contains(s.Cwd, cwdFilter) {
			continue
		}
		unbound = append(unbound, s)
	}

	if len(unbound) == 0 {
		msg := "_(no unbound local claude sessions found"
		if cwdFilter != "" {
			msg += " matching `" + cwdFilter + "`"
		}
		msg += ")_"
		_ = w.api.PostCC(ctx, replyHandle, msg)
		return
	}

	const maxRows = 20
	rows := unbound
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}

	var b strings.Builder
	if cwdFilter != "" {
		fmt.Fprintf(&b, "**Local claude sessions (unbound, cwd matches `%s`):**\n", cwdFilter)
	} else {
		b.WriteString("**Local claude sessions (unbound):**\n")
	}
	for i, s := range rows {
		fmt.Fprintf(&b, "%d. `%s`  — `%s`  (%d turns, %s)\n",
			i+1, s.SessionID, s.Cwd, s.MessageCount, s.LastActive.Format("2006-01-02 15:04"))
		preview := s.FirstMessage
		if len(preview) > 100 {
			preview = preview[:100] + "…"
		}
		fmt.Fprintf(&b, "   > %s\n", preview)
	}
	if len(unbound) > maxRows {
		fmt.Fprintf(&b, "\n_(showing first %d of %d — use `!sessions <cwd-filter>` to narrow)_\n", maxRows, len(unbound))
	}
	b.WriteString("\nPick one or more with `!bind <session_id> [<session_id> …]` — each binding creates a new task channel.")
	_ = w.api.PostCC(ctx, replyHandle, b.String())
}

// cmdBind creates a task channel for each session_id and writes the
// channel_handle → session_id binding into cc-sessions.json so the
// daemon's NEXT spawn for that channel uses --resume.
//
// Naming: channel name is derived from the cwd's basename (Discord-sanitized).
// On collision the server returns 400 and we just report it back.
func (w *CCWatch) cmdBind(ctx context.Context, replyHandle string, args []string) {
	if len(args) == 0 {
		_ = w.api.PostCC(ctx, replyHandle,
			"❌ usage: `!bind <session_id> [<session_id> …]`  — run `!sessions` first to find ids.")
		return
	}

	root, err := claudeProjectsRoot()
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ couldn't locate ~/.claude/projects: "+err.Error())
		return
	}
	all, err := ListLocalSessions(root, w.sessions.Snapshot())
	if err != nil {
		_ = w.api.PostCC(ctx, replyHandle, "❌ scan failed: "+err.Error())
		return
	}
	byID := map[string]LocalSession{}
	for _, s := range all {
		byID[s.SessionID] = s
	}

	results := BindLocalSessionsFromMap(ctx, w.api, w.sessions, args, byID)
	_ = w.api.PostCC(ctx, replyHandle, formatBindReport(results))
}

// BindResult is one line of the !bind / `duckway cc bind` summary.
type BindResult struct {
	SessionID    string
	Channel      string // dwch_ handle on success
	Name         string // discord channel name on success
	Cwd          string
	Error        string // empty on success
	AlreadyBound string // existing handle if session was already in cc-sessions.json
}

// BindLocalSessions is the public entry point used by `duckway cc bind`.
// It scans ~/.claude/projects/ itself (the daemon already has metadata
// cached, but the CLI doesn't), then runs the create-channel + write-store
// flow per session_id.
func BindLocalSessions(ctx context.Context, api *APIClient, store *CCSessionStore, sessionIDs []string) []BindResult {
	root, err := ClaudeProjectsRoot()
	if err != nil {
		// One result per id, all failing — keeps the caller's loop dumb.
		out := make([]BindResult, 0, len(sessionIDs))
		for _, sid := range sessionIDs {
			out = append(out, BindResult{SessionID: sid, Error: err.Error()})
		}
		return out
	}
	all, err := ListLocalSessions(root, store.Snapshot())
	if err != nil {
		out := make([]BindResult, 0, len(sessionIDs))
		for _, sid := range sessionIDs {
			out = append(out, BindResult{SessionID: sid, Error: err.Error()})
		}
		return out
	}
	byID := map[string]LocalSession{}
	for _, s := range all {
		byID[s.SessionID] = s
	}
	return BindLocalSessionsFromMap(ctx, api, store, sessionIDs, byID)
}

// BindLocalSessionsFromMap is the same as BindLocalSessions but accepts a
// pre-built lookup so the daemon can reuse its scan.
func BindLocalSessionsFromMap(ctx context.Context, api *APIClient, store *CCSessionStore, sessionIDs []string, byID map[string]LocalSession) []BindResult {
	out := make([]BindResult, 0, len(sessionIDs))
	reverse := map[string]string{}
	for h, s := range store.Snapshot() {
		reverse[s] = h
	}
	for _, sid := range sessionIDs {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			continue
		}
		r := BindResult{SessionID: sid}
		sess, ok := byID[sid]
		if !ok {
			r.Error = "session_id not found under ~/.claude/projects (run `!sessions` first)"
			out = append(out, r)
			continue
		}
		r.Cwd = sess.Cwd
		if existing := reverse[sid]; existing != "" {
			r.AlreadyBound = existing
			out = append(out, r)
			continue
		}

		name := discordChannelNameFromCwd(sess.Cwd)
		created, err := api.CreateCCChannel(ctx, name, "", sess.Cwd)
		if err != nil {
			r.Error = "create channel: " + err.Error()
			out = append(out, r)
			continue
		}
		if err := store.Set(created.Handle, sid); err != nil {
			r.Error = "channel created (" + created.Handle + ") but writing cc-sessions.json failed: " + err.Error()
			out = append(out, r)
			continue
		}
		r.Channel = created.Handle
		r.Name = created.Name
		out = append(out, r)
	}
	return out
}

var nonDiscordName = regexp.MustCompile(`[^a-z0-9-]+`)

// discordChannelNameFromCwd produces a Discord-legal channel name from a
// filesystem cwd. Discord lowercases and substitutes dashes anyway, but
// doing it locally lets the name match what the server posts back.
func discordChannelNameFromCwd(cwd string) string {
	base := filepath.Base(strings.TrimRight(cwd, "/"))
	if base == "" || base == "." || base == "/" {
		base = "session"
	}
	base = strings.ToLower(base)
	base = nonDiscordName.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "session"
	}
	if len(base) > 90 {
		base = base[:90]
	}
	return base
}

// formatBindReport collapses per-session results into a single Discord
// message. Successes first, then "already bound" notes, then errors.
func formatBindReport(rs []BindResult) string {
	var ok, dup, fail []BindResult
	for _, r := range rs {
		switch {
		case r.Error != "":
			fail = append(fail, r)
		case r.AlreadyBound != "":
			dup = append(dup, r)
		default:
			ok = append(ok, r)
		}
	}
	if len(rs) == 0 {
		return "❌ nothing to bind."
	}
	var b strings.Builder
	if len(ok) > 0 {
		b.WriteString("✅ **Bound:**\n")
		for _, r := range ok {
			fmt.Fprintf(&b, "• `%s` → **#%s** (`%s`)  cwd: `%s`\n", r.SessionID, r.Name, r.Channel, r.Cwd)
		}
		b.WriteString("Send a message in the new channel — claude will resume with the existing history.\n")
	}
	if len(dup) > 0 {
		b.WriteString("\nℹ️ **Already bound:**\n")
		for _, r := range dup {
			fmt.Fprintf(&b, "• `%s` → `%s` (use that channel directly)\n", r.SessionID, r.AlreadyBound)
		}
	}
	if len(fail) > 0 {
		b.WriteString("\n❌ **Failed:**\n")
		for _, r := range fail {
			fmt.Fprintf(&b, "• `%s` — %s\n", r.SessionID, r.Error)
		}
	}
	return b.String()
}

func formatProjectsReport(projects []CCProject, filter string) string {
	if len(projects) == 0 {
		if filter != "" {
			return "_(no saved projects matching `" + filter + "`)_"
		}
		return "No saved projects yet.\n\nAdd projects on the agent machine:\n`duckway projects add ~/duckway`\n`duckway projects add ~/projects/*`"
	}
	const maxRows = 30
	rows := projects
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	var b strings.Builder
	if filter != "" {
		fmt.Fprintf(&b, "**Projects matching `%s`:**\n", filter)
	} else {
		b.WriteString("**Saved projects:**\n")
	}
	for i, p := range rows {
		fmt.Fprintf(&b, "%d. `%s`  — `%s`\n", i+1, p.Name, p.Path)
	}
	if len(projects) > maxRows {
		fmt.Fprintf(&b, "\n_(showing first %d of %d — use `!projects <filter>` to narrow)_\n", maxRows, len(projects))
	}
	b.WriteString("\nUse `!new <slug> --project <name|number>`.")
	return b.String()
}

func splitClientSlugAndFlags(args []string) (string, map[string]string, error) {
	parsed, err := cccommand.ParseNewArgs(args)
	if err != nil {
		return "", nil, err
	}
	return parsed.Slug, map[string]string{
		"cwd": parsed.Cwd, "project": parsed.Project, "topic": parsed.Topic,
	}, nil
}
