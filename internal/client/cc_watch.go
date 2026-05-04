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
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CCWatch is the `duckway cc watch` daemon. It connects to the server's
// SSE stream, dispatches incoming Discord task-channel messages to
// per-channel claude runners, and cleans up when channels are deleted.
//
// Reconnects forever with exponential backoff (5s → 10s → 30s → 60s cap)
// so a brief duckway-server outage doesn't kill the daemon.
type CCWatch struct {
	cfg       *Config
	configDir string
	bin       string // resolved `claude` binary path
	sessions  *CCSessionStore

	mu      sync.Mutex
	runners map[string]*ccRunner // by channel handle

	api *APIClient
}

func NewCCWatch(configDir string, cfg *Config) (*CCWatch, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude binary not found in PATH (install Claude Code first): %w", err)
	}
	return &CCWatch{
		cfg:       cfg,
		configDir: configDir,
		bin:       bin,
		sessions:  NewCCSessionStore(configDir),
		runners:   map[string]*ccRunner{},
		api:       NewAPIClient(cfg.ServerURL, cfg.Token),
	}, nil
}

// Run is the main loop. Blocks until ctx is cancelled.
func (w *CCWatch) Run(ctx context.Context) error {
	log.Printf("[cc-watch] starting; claude=%s server=%s", w.bin, w.cfg.ServerURL)

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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}
	log.Printf("[cc-watch] SSE connected")

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
	case "message_create":
		w.handleMessageCreate(data)
	case "channel_delete":
		w.handleChannelDelete(data)
	default:
		// message_update, channel_update, etc — currently ignored. The
		// session model assumes prompts come from message_create only.
	}
}

// sseEnvelope mirrors services.CCEvent.
type sseEnvelope struct {
	Type    string          `json:"type"`
	CCID    string          `json:"cc_id"`
	Handle  string          `json:"channel_handle"`
	Payload json.RawMessage `json:"payload"`
}

// payloadMessageCreate is the Discord MESSAGE_CREATE shape we care about.
type payloadMessageCreate struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Author  struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
	ChannelID string `json:"channel_id"`
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
	if msg.Author.Bot {
		// Skip — server filters these too, but be defensive.
		return
	}
	if strings.TrimSpace(msg.Content) == "" {
		return
	}

	runner, err := w.runnerFor(env.Handle)
	if err != nil {
		log.Printf("[cc-watch] cannot start runner for %s: %v", env.Handle, err)
		_ = w.api.PostCC(context.Background(), env.Handle, "❌ daemon could not start a session: "+err.Error())
		return
	}
	if !runner.Enqueue(ccTask{Content: msg.Content, AuthorID: msg.Author.ID, MessageID: msg.ID}) {
		log.Printf("[cc-watch] %s: queue full, dropping message %s", env.Handle, msg.ID)
		_ = w.api.PostCC(context.Background(), env.Handle,
			"⚠️ session queue full (10 messages backed up) — your message was dropped, please retry once claude catches up.")
	}
}

func (w *CCWatch) handleChannelDelete(data []byte) {
	var env sseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	if env.Handle == "" {
		return
	}
	w.mu.Lock()
	if r, ok := w.runners[env.Handle]; ok {
		r.Stop()
		delete(w.runners, env.Handle)
	}
	w.mu.Unlock()
	_ = w.sessions.Drop(env.Handle)
	log.Printf("[cc-watch] %s: channel deleted, session dropped", env.Handle)
}

// runnerFor returns the runner for a handle, lazily creating one. Looks
// up the channel's cwd from /client/cc/channels (the daemon doesn't
// cache channel metadata; the cost is one HTTP call per first-message).
func (w *CCWatch) runnerFor(handle string) (*ccRunner, error) {
	w.mu.Lock()
	if r, ok := w.runners[handle]; ok {
		w.mu.Unlock()
		return r, nil
	}
	w.mu.Unlock()

	cwd, err := w.fetchChannelCwd(handle)
	if err != nil {
		return nil, err
	}
	r, err := newCCRunner(handle, w.configDir, cwd, w.bin, w.sessions, w.api.PostCC)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	// Re-check under lock in case we raced.
	if existing, ok := w.runners[handle]; ok {
		w.mu.Unlock()
		r.Stop()
		return existing, nil
	}
	w.runners[handle] = r
	w.mu.Unlock()
	return r, nil
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
	w.mu.Lock()
	defer w.mu.Unlock()
	for h, r := range w.runners {
		r.Stop()
		delete(w.runners, h)
	}
	log.Printf("[cc-watch] shutdown complete")
}
