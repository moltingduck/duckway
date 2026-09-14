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
	queue         chan localNotification
	done          chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
	now           func() time.Time
	mu            sync.Mutex
	lastAttention map[string]time.Time
	closed        bool
	workers       sync.WaitGroup
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
			showDesktopNotification(s.ctx, n)
		}
		if n.Sound != "" {
			playLocalSound(s.ctx, n.Sound)
		}
	}
}

func playLocalSound(parent context.Context, path string) {
	if !filepath.IsAbs(path) {
		return
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".wav", ".mp3", ".ogg":
	default:
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	player, err := exec.LookPath("ffplay")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, player, "-nodisp", "-autoexit", "-loglevel", "quiet", path)
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
}

func showDesktopNotification(parent context.Context, n localNotification) {
	program, err := exec.LookPath("notify-send")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, "--", "Ducklord · "+n.Project,
		fmt.Sprintf("%s · %s", n.Session, notificationClassLabel(n.Class)))
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
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
	projectName := "Default Project"
	if identity, valid := ducklord.IdentityFromSession(session); valid {
		current := ""
		if s.workspaceNav != nil {
			current = s.workspaceNav.CurrentProjectID()
		}
		projectID := s.activity().ProjectLayout.NavigateProject(identity, current, "")
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
