package events

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// DefaultMaxLifetime bounds how long one stream stays open. The browser
// reconnects through authentication afterwards.
const DefaultMaxLifetime = 30 * time.Minute

// HandlerConfig tunes the SSE handler.
type HandlerConfig struct {
	Heartbeat    time.Duration // default 15s, capped at 15s
	Retry        time.Duration // client reconnect hint, default 3s
	WriteTimeout time.Duration // per-write deadline extension, default 20s
	MaxLifetime  time.Duration // stream lifetime cap, default 30m
	// ValidateSession, if set, returns the authenticated username and
	// whether the request's session is still valid. It is called before
	// subscribing and on every heartbeat; an invalid session closes the
	// stream.
	ValidateSession func(r *http.Request) (user string, ok bool)
}

// Handler serves GET /api/events. Mount it behind admin auth.
func (s *Service) Handler(cfg HandlerConfig) http.Handler {
	if cfg.Heartbeat <= 0 || cfg.Heartbeat > 15*time.Second {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.Retry <= 0 {
		cfg.Retry = 3 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 20 * time.Second
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = DefaultMaxLifetime
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		// Refuse to stream without a per-write deadline: a stalled client
		// would otherwise pin the goroutine forever.
		if err := rc.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout)); err != nil {
			log.Printf("[events] write deadlines unsupported, refusing stream: %v", err)
			http.Error(w, "streaming unavailable", http.StatusServiceUnavailable)
			return
		}
		user := ""
		if cfg.ValidateSession != nil {
			u, ok := cfg.ValidateSession(r)
			if !ok {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			user = u
		}
		key := sessionKey(r)
		if key == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		sub, err := s.Hub.SubscribeUser(key, user)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, ErrSessionLimit) || errors.Is(err, ErrUserLimit) {
				status = http.StatusTooManyRequests
			}
			w.Header().Set("Retry-After", "5")
			http.Error(w, err.Error(), status)
			return
		}
		defer sub.Close()

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache, no-transform")
		h.Set("X-Accel-Buffering", "no")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		write := func(format string, args ...any) bool {
			_ = rc.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout))
			if _, err := fmt.Fprintf(w, format, args...); err != nil {
				return false
			}
			return rc.Flush() == nil
		}
		if !write("retry: %d\n", cfg.Retry.Milliseconds()) ||
			!write("event: hello\ndata: {\"v\":1}\n\n") {
			return
		}

		type batch struct {
			evs []Event
			ok  bool
		}
		ctx := r.Context()
		life := time.NewTimer(cfg.MaxLifetime)
		defer life.Stop()
		hb := time.NewTicker(cfg.Heartbeat)
		defer hb.Stop()
		ch := make(chan batch)
		quit := make(chan struct{})
		defer close(quit)
		go func() {
			for {
				evs, more := sub.Next(ctx)
				select {
				case ch <- batch{evs, more}:
				case <-quit:
					return
				}
				if !more {
					return
				}
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-life.C:
				// Tell the client this is a planned close so it reconnects
				// immediately without counting a failure.
				write("event: bye\ndata: {\"reason\":\"lifetime\"}\n\n")
				return
			case <-hb.C:
				if cfg.ValidateSession != nil {
					if _, ok := cfg.ValidateSession(r); !ok {
						return
					}
				}
				if !write(": hb\n\n") {
					return
				}
			case b := <-ch:
				for _, ev := range b.evs {
					if !write("event: %s\ndata: %s\n\n", ev.Topic, ev.Data) {
						return
					}
				}
				if !b.ok {
					return
				}
			}
		}
	})
}

func sessionKey(r *http.Request) string {
	c, err := r.Cookie("duckway_session")
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(c.Value))
	return hex.EncodeToString(sum[:8])
}
