package model

import "fmt"

type NotificationCategory string

const (
	NotificationTerminalAttention     NotificationCategory = "terminal_attention"
	NotificationTaskCompleted         NotificationCategory = "task_completed"
	NotificationTaskFailed            NotificationCategory = "task_failed"
	NotificationTaskCancelled         NotificationCategory = "task_cancelled"
	NotificationTaskTimeout           NotificationCategory = "task_timeout"
	NotificationApprovalRequired      NotificationCategory = "approval_required"
	NotificationAgentNeedsInput       NotificationCategory = "agent_needs_input"
	NotificationUnexpectedProcessExit NotificationCategory = "unexpected_process_exit"
)

func NotificationCategories() []NotificationCategory {
	return []NotificationCategory{
		NotificationTerminalAttention,
		NotificationTaskCompleted,
		NotificationTaskFailed,
		NotificationTaskCancelled,
		NotificationTaskTimeout,
		NotificationApprovalRequired,
		NotificationAgentNeedsInput,
		NotificationUnexpectedProcessExit,
	}
}

func (c NotificationCategory) Validate() error {
	switch c {
	case NotificationTerminalAttention, NotificationTaskCompleted, NotificationTaskFailed, NotificationTaskCancelled,
		NotificationTaskTimeout, NotificationApprovalRequired, NotificationAgentNeedsInput, NotificationUnexpectedProcessExit:
		return nil
	default:
		return fmt.Errorf("invalid notification category %q", c)
	}
}
