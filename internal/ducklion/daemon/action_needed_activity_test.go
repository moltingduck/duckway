package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/supervisor"
)

func TestActionNeededActivityCapabilityBoundary(t *testing.T) {
	for _, category := range []model.NotificationCategory{model.NotificationApprovalRequired, model.NotificationAgentNeedsInput} {
		for _, agentActivity := range []bool{false, true} {
			client := &SupervisorActivityClient{supportsAgentActivity: agentActivity}
			// A nil transport deliberately proves unsupported categories are
			// rejected before any request is written to an older daemon.
			if err := client.ReportActivity(context.Background(), category, 0, 1); !errors.Is(err, errUnsupportedActionNeededActivity) {
				t.Fatalf("category=%s agent_activity=%v: %v", category, agentActivity, err)
			}
		}
	}
}

func TestPendingForwardableActivityDowngrade(t *testing.T) {
	for _, test := range []struct {
		name   string
		client *SupervisorClient
		want   []model.NotificationCategory
	}{
		{"no_attention", &SupervisorClient{}, []model.NotificationCategory{model.NotificationTaskCompleted}},
		{"no_agent_activity", &SupervisorClient{supportsAttention: true}, []model.NotificationCategory{model.NotificationTaskCompleted}},
		{"old_agent_activity", &SupervisorClient{supportsAttention: true, supportsAgentActivity: true}, []model.NotificationCategory{model.NotificationTaskCompleted}},
		{"new_agent_activity", &SupervisorClient{supportsAttention: true, supportsAgentActivity: true, supportsActionNeededActivity: true}, []model.NotificationCategory{model.NotificationApprovalRequired, model.NotificationAgentNeedsInput, model.NotificationTaskCompleted}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, err := supervisor.Start(supervisor.Options{SessionID: "ABC123", RuntimeGeneration: 1, OwnershipEpoch: 1,
				AgentType: "fixture", CWD: t.TempDir(), OutputCapacity: 1024,
				Command: []string{"sh", "-c", `printf '%s\n' '{"kind":"approval_required"}' '{"kind":"agent_needs_input"}' '{"kind":"completed"}' >&3`}})
			if err != nil {
				t.Fatal(err)
			}
			// Wait joins the adapter reader, so all three callbacks are queued
			// before exercising the same drain used during live delivery and exit.
			if err := session.Wait(); err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				category, offset, id, _, pending := pendingForwardableActivity(test.client, session)
				if !pending || category != want {
					t.Fatalf("got category=%s pending=%v, want %s", category, pending, want)
				}
				if category == model.NotificationTaskCompleted && id != 3 {
					t.Fatalf("downgrade rewrote completion identity: %d", id)
				}
				session.AckActivity(category, offset, id)
			}
			if _, _, _, _, pending := pendingForwardableActivity(test.client, session); pending {
				t.Fatal("pending activity would prevent exit reporting")
			}
		})
	}
}
