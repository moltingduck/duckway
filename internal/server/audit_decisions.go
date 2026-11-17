package server

import (
	"context"
	"fmt"
	"log"

	"github.com/hackerduck/duckway/internal/audit"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/services"
)

// approvalSummary is the one summary format for approval decisions, shared by
// the web wrapper (auditMux) and the decision hook.
func approvalSummary(verb, approvalID, name string) string {
	past := map[string]string{"approve": "approved", "reject": "rejected", "expire": "expired"}[verb]
	return fmt.Sprintf("%s request %s for %s", past, approvalID, name)
}

// auditDecisionHook records every committed approval decision, including the
// web console's (auditMux skips the four approval routes). Because it runs only
// after the decision commits, a request that loses a race records nothing.
// Entries use resource_type placeholder (the token the decision unlocks),
// action approval.approve|reject|expire, and actor and via from the decider.
// It runs on Discord and sweeper goroutines, so it never panics.
func auditDecisionHook(rec *audit.Recorder, snaps *handlers.AuditSnapshots) services.DecisionHook {
	return func(ctx context.Context, approvalID, by, via, status string) {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("audit: decision hook panic: %v", p)
			}
		}()
		verb := map[string]string{"approved": "approve", "rejected": "reject", "ignored": "expire"}[status]
		if verb == "" {
			return
		}
		name, after, ok := snaps.Get(apvT, approvalID)
		var placeholderID string
		var afterMap map[string]any
		if ok {
			if m, _ := after.(map[string]any); m != nil {
				placeholderID, _ = m["placeholder_id"].(string)
				afterMap = m
			}
		}
		var before any
		if afterMap != nil {
			b := map[string]any{}
			for k, v := range afterMap {
				b[k] = v
			}
			b["status"] = "pending"
			delete(b, "approved_at")
			delete(b, "expires_at")
			before = b
		}
		ictx := audit.WithIdentity(audit.WithRecorder(context.WithoutCancel(ctx), rec), audit.Identity{Actor: by, Via: via, RemoteAddr: audit.IdentityFrom(ctx).RemoteAddr})
		rec.Record(ictx, audit.Entry{
			Action: apvT + "." + verb, ResourceType: phT, ResourceID: placeholderID, ResourceName: name,
			Before: before, After: after, Summary: approvalSummary(verb, approvalID, name),
		})
	}
}
