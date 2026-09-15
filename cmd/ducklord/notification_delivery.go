package main

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

type notificationDelivery interface {
	Deliver(localNotification) bool
	Close()
}

type localNotification struct {
	Project string
	Session string
	Class   ducklord.NotificationClass
	Level   ducklord.NotificationLevel
	Sound   string
	Key     string
}

// The queue keeps session inventory updates independent of local audio and
// desktop services. It deliberately never includes agent output or prompts.
type localNotificationSink struct {
	queue           chan localNotification
	done            chan struct{}
	ctx             context.Context
	cancel          context.CancelFunc
	now             func() time.Time
	mu              sync.Mutex
	lastAttention   map[string]time.Time
	closed          bool
	workers         sync.WaitGroup
	backendWarnings map[string]string
}

func newLocalNotificationSink() *localNotificationSink {
	ctx, cancel := context.WithCancel(context.Background())
	s := &localNotificationSink{queue: make(chan localNotification, 1024), done: make(chan struct{}), ctx: ctx, cancel: cancel, now: time.Now,
		lastAttention: make(map[string]time.Time)}
	for range 4 {
		s.workers.Add(1)
		go s.run()
	}
	go func() { s.workers.Wait(); close(s.done) }()
	return s
}

func (s *localNotificationSink) Deliver(n localNotification) bool {
	if n.Level.Rank() < ducklord.NotificationSound.Rank() {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if n.Class == ducklord.NotificationAttention && s.now().Sub(s.lastAttention[n.Key]) < 2*time.Second {
		return true
	}
	select {
	case s.queue <- n:
		if n.Class == ducklord.NotificationAttention {
			s.lastAttention[n.Key] = s.now()
		}
		return true
	default: // Never stall PTY rendering on a local notification backend.
		return false
	}
}

func (s *localNotificationSink) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		close(s.queue)
	}
	s.mu.Unlock()
	<-s.done
}

func (s *localNotificationSink) run() {
	defer s.workers.Done()
	for {
		var n localNotification
		select {
		case <-s.ctx.Done():
			return
		case item, ok := <-s.queue:
			if !ok {
				return
			}
			n = item
		}
		if n.Level == ducklord.NotificationSystem {
			s.recordBackendResult("desktop", showDesktopNotification(s.ctx, n))
		}
		if n.Sound != "" {
			s.recordBackendResult("sound", playLocalSound(s.ctx, n.Sound), n.Sound)
		}
	}
}

// Keep diagnostics bounded and free of subprocess output, paths and credentials.
func (s *localNotificationSink) recordBackendResult(backend string, err error, configuration ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.backendWarnings == nil {
		s.backendWarnings = make(map[string]string)
	}
	key := backend + "\x00" + strings.Join(configuration, "\x00")
	if err == nil {
		delete(s.backendWarnings, key)
		return
	}
	s.backendWarnings[key] = backend + " notification failed; check the local notification backend and settings"
}

func (s *localNotificationSink) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var warnings []string
	for _, backend := range []string{"sound", "desktop"} {
		for key, warning := range s.backendWarnings {
			if strings.HasPrefix(key, backend+"\x00") {
				warnings = append(warnings, warning)
				break
			}
		}
	}
	return warnings
}

// Drain the player's diagnostics without allowing arbitrary decoder output to
// grow memory usage. Diagnostics are never included in operator-facing errors.
type soundErrorCapture struct {
	data [4096]byte
	n    int
}

func (w *soundErrorCapture) Write(p []byte) (int, error) {
	w.n += copy(w.data[w.n:], p)
	return len(p), nil
}

func playLocalSound(parent context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("sound path must be absolute")
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".wav", ".mp3", ".ogg":
	default:
		return fmt.Errorf("unsupported sound format")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sound file unavailable")
	}
	player, err := exec.LookPath("ffplay")
	if err != nil {
		return fmt.Errorf("sound player unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, player, "-nodisp", "-autoexit", "-loglevel", "error", path)
	var diagnostics soundErrorCapture
	cmd.Stderr = &diagnostics
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// ffplay can exit zero after failing to decode input or open an audio
	// device. At error log level, any diagnostic means playback failed.
	if err != nil || diagnostics.n != 0 {
		return fmt.Errorf("sound playback failed")
	}
	return nil
}

func showDesktopNotification(parent context.Context, n localNotification) error {
	program, err := exec.LookPath("notify-send")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, "--", "Ducklord · "+n.Project,
		fmt.Sprintf("%s · %s", n.Session, notificationClassLabel(n.Class)))
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

func notificationClassLabel(class ducklord.NotificationClass) string {
	switch class {
	case ducklord.NotificationCompleted:
		return "task completed"
	case ducklord.NotificationFailed:
		return "task failed"
	case ducklord.NotificationActionNeeded:
		return "action needed"
	default:
		return "terminal attention"
	}
}

func desktopLabel(value string) string {
	var label strings.Builder
	count := 0
	for _, r := range value {
		if count >= 80 {
			break
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			if r == '\n' || r == '\r' || r == '\t' {
				label.WriteByte(' ')
				count++
			}
			continue
		}
		label.WriteRune(r)
		count++
	}
	return html.EscapeString(strings.TrimSpace(label.String()))
}

func soundSetupWarning(cfg *ducklord.Config) string {
	if cfg == nil || len(cfg.NotificationSounds) == 0 {
		return ""
	}
	for class, path := range cfg.NotificationSounds {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return fmt.Sprintf("%s sound must use an absolute local path", class)
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".wav", ".mp3", ".ogg":
		default:
			return fmt.Sprintf("%s sound must be WAV, MP3, or OGG", class)
		}
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Sprintf("%s sound file is unavailable", class)
		}
		if _, err := exec.LookPath("ffplay"); err != nil {
			return "custom sound requires ffplay on this Ducklord machine"
		}
	}
	return ""
}

func desktopSetupWarning(cfg *ducklord.Config, state *ducklord.ActivityState) string {
	if cfg == nil {
		return ""
	}
	wantsSystem := func(levels map[ducklord.NotificationClass]ducklord.NotificationLevel) bool {
		for _, level := range levels {
			if level == ducklord.NotificationSystem {
				return true
			}
		}
		return false
	}
	needed := wantsSystem(cfg.NotificationLevels)
	for _, host := range cfg.Clients {
		needed = needed || wantsSystem(host.NotificationLevels)
	}
	if state != nil {
		for _, session := range state.Sessions {
			needed = needed || wantsSystem(session.NotificationLevels)
		}
	}
	if !needed {
		return ""
	}
	if _, err := exec.LookPath("notify-send"); err != nil {
		return "desktop notifications require notify-send on this Ducklord machine"
	}
	return ""
}

func (s *tuiState) deliverNotification(session ducklord.RemoteSession, category model.NotificationCategory) {
	if s.notificationSink == nil || !s.shouldDeliverNotification(session, category) {
		return
	}
	class, ok := ducklord.NotificationClassFor(category)
	if !ok {
		return
	}
	if class == ducklord.NotificationAttention {
		key := sessionKey(session)
		if identity, valid := ducklord.IdentityFromSession(session); valid {
			key = identity.Key()
		}
		now := time.Now()
		if s.notificationNow != nil {
			now = s.notificationNow()
		}
		if now.Sub(s.attentionDeliveryAt[key]) < 2*time.Second {
			return
		}
		if s.attentionDeliveryAt == nil {
			s.attentionDeliveryAt = make(map[string]time.Time)
		}
		s.attentionDeliveryAt[key] = now
	}
	projectName := "Default Project"
	if identity, valid := ducklord.IdentityFromSession(session); valid {
		projectID := s.activity().ProjectLayout.NavigateProject(identity, "", "")
		if s.workspaceNav != nil {
			projectID = s.workspaceNav.NotificationProjectID(identity)
		}
		if project := s.activity().ProjectLayout.Project(projectID); project != nil {
			projectName = project.Name
		}
	}
	sound := ""
	if s.cfg != nil {
		sound = s.cfg.NotificationSounds[class]
	}
	if sound == "" && s.effectiveNotificationLevel(session, category).Rank() >= ducklord.NotificationSound.Rank() && s.pendingBells < 64 {
		s.pendingBells++ // Emit from the TUI event loop, never the background worker.
	}
	if !s.notificationSink.Deliver(localNotification{Project: desktopLabel(projectName), Session: desktopLabel(session.Name), Class: class,
		Level: s.effectiveNotificationLevel(session, category), Sound: sound, Key: sessionKey(session)}) {
		s.localWarning = "local notification backend is overloaded; check unread Sessions"
	}
}
