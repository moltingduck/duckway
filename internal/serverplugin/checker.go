package serverplugin

import (
	"context"
	"time"
)

const maxCheckPollInterval = time.Minute

// Poll at most one minute apart, independently of a source's configured check
// interval. A source registered just after startup must not wait another full
// six-hour tick after its persisted due time.
func checkPollInterval(interval time.Duration) time.Duration {
	if interval < maxCheckPollInterval {
		return interval
	}
	return maxCheckPollInterval
}

type Checker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func runChecker(ctx context.Context, store *Store, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			_ = store.CheckDue(ctx, now)
		}
	}
}
func StartChecker(store *Store, stop <-chan struct{}) *Checker {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Checker{cancel: cancel, done: make(chan struct{})}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	go func() {
		defer close(c.done)
		defer func() { cancel(); <-watchDone }()
		ticker := time.NewTicker(checkPollInterval(store.interval))
		defer ticker.Stop()
		runChecker(ctx, store, ticker.C)
	}()
	return c
}
func (c *Checker) Stop() { c.cancel(); <-c.done }
