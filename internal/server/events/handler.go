package events

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// HandlerConfig tunes the SSE handler.
type HandlerConfig struct {
	Heartbeat    time.Duration // default 15s, capped at 15s
	Retry        time.Duration // client reconnect hint, default 3s
	WriteTimeout time.Duration // per-write deadline extension, default 20s
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		rc := http.NewResponseController(w)
		if !ok {
			if err := rc.Flush(); err != nil {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
		}
		sub, err := s.Hub.Subscribe(sessionKey(r))
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, ErrSessionLimit) {
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
			if ok {
				fl.Flush()
			} else if rc.Flush() != nil {
				return false
			}
			return true
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
			case <-hb.C:
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
