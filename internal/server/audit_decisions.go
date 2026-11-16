package server

import (
	"context"
	"fmt"

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

// auditDecisionHook records every committed approval decision that did not
// come from the web console. Web decisions (via "web") are recorded by auditMux
// from the same request, so skipping them here avoids a double entry. Entries
// follow the web convention: resource_type placeholder (the token the decision
// unlocks), action approval.approve|reject|expire, actor and via from the
// decider.
func auditDecisionHook(rec *audit.Recorder, snaps *handlers.AuditSnapshots) services.DecisionHook {
	return func(ctx context.Context, approvalID, by, via, status string) {
		if via == services.ViaWeb {
			return
		}
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
		ictx := audit.WithIdentity(audit.WithRecorder(context.WithoutCancel(ctx), rec), audit.Identity{Actor: by, Via: via})
		rec.Record(ictx, audit.Entry{
			Action: apvT + "." + verb, ResourceType: phT, ResourceID: placeholderID, ResourceName: name,
			Before: before, After: after, Summary: approvalSummary(verb, approvalID, name),
		})
	}
}
