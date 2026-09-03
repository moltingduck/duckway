package client

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// ccAgentProgress is an allowlisted semantic update from an agent runner. It
// deliberately carries no command output, prompts, paths, or environment data.
type ccAgentProgress struct {
	Summary   string
	SessionID string
}

type ccAgentProgressSink func(ccAgentProgress)
type ccAgentProgressKey struct{}

func withCCAgentProgress(ctx context.Context, sink ccAgentProgressSink) context.Context {
	return context.WithValue(ctx, ccAgentProgressKey{}, sink)
}

func emitCCAgentProgress(ctx context.Context, event ccAgentProgress) {
	if sink, ok := ctx.Value(ccAgentProgressKey{}).(ccAgentProgressSink); ok && sink != nil {
		sink(event)
	}
}

// dedupeCCAgentProgress prevents PTY tail polling from repeatedly persisting a
// session or editing Discord with the same status.
func dedupeCCAgentProgress(sink ccAgentProgressSink) ccAgentProgressSink {
	var mu sync.Mutex
	lastSummary, lastSession := "", ""
	return func(event ccAgentProgress) {
		mu.Lock()
		newSummary, newSession := lastSummary, lastSession
		if event.Summary != "" {
			newSummary = event.Summary
		}
		if event.SessionID != "" {
			newSession = event.SessionID
		}
		if newSummary == lastSummary && newSession == lastSession {
			mu.Unlock()
			return
		}
		lastSummary, lastSession = newSummary, newSession
		sink(event)
		mu.Unlock()
	}
}

func validCodexThreadID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}
