package ducklord

import (
	"fmt"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

// NotificationClass is an operator-facing event group. The Ducklion category
// remains the authoritative event identity and cursor; grouping only selects
// Ducklord-local presentation policy.
type NotificationClass string

const (
	NotificationCompleted    NotificationClass = "task_completed"
	NotificationFailed       NotificationClass = "task_failed"
	NotificationActionNeeded NotificationClass = "action_needed"
	NotificationAttention    NotificationClass = "terminal_attention"
)

type NotificationLevel string

const (
	NotificationOff       NotificationLevel = "off"
	NotificationIndicator NotificationLevel = "indicator"
	NotificationSound     NotificationLevel = "sound"
	NotificationSystem    NotificationLevel = "system"
)

func (c NotificationClass) Validate() error {
	switch c {
	case NotificationCompleted, NotificationFailed, NotificationActionNeeded, NotificationAttention:
		return nil
	default:
		return fmt.Errorf("invalid notification class %q", c)
	}
}

func (l NotificationLevel) Validate() error {
	switch l {
	case NotificationOff, NotificationIndicator, NotificationSound, NotificationSystem:
		return nil
	default:
		return fmt.Errorf("invalid notification level %q", l)
	}
}

func (l NotificationLevel) Rank() int {
	switch l {
	case NotificationIndicator:
		return 1
	case NotificationSound:
		return 2
	case NotificationSystem:
		return 3
	default:
		return 0
	}
}

func NotificationClassFor(category model.NotificationCategory) (NotificationClass, bool) {
	switch category {
	case model.NotificationTaskCompleted:
		return NotificationCompleted, true
	case model.NotificationTaskFailed, model.NotificationTaskTimeout, model.NotificationUnexpectedProcessExit, model.NotificationTaskCancelled:
		return NotificationFailed, true
	case model.NotificationApprovalRequired, model.NotificationAgentNeedsInput:
		return NotificationActionNeeded, true
	case model.NotificationTerminalAttention:
		return NotificationAttention, true
	default:
		return "", false
	}
}

// ResolveNotificationLevel follows global -> Host -> Session inheritance. A
// missing entry means inherit, while an explicit off entry is authoritative.
func ResolveNotificationLevel(class NotificationClass, global, host, session map[NotificationClass]NotificationLevel) NotificationLevel {
	level := NotificationIndicator
	if value, ok := global[class]; ok {
		level = value
	}
	if value, ok := host[class]; ok {
		level = value
	}
	if value, ok := session[class]; ok {
		level = value
	}
	return level
}

// ShouldDeliver applies Project focus after Host/Session policy resolution.
// Focus suppression leaves unread cursor handling to the caller; it suppresses
// immediate presentation and promotion only.
func ShouldDeliver(level NotificationLevel, focusActive, inFocusedProject bool, threshold NotificationLevel) bool {
	if level == NotificationOff {
		return false
	}
	return !focusActive || inFocusedProject || level.Rank() >= threshold.Rank()
}

func ValidateNotificationLevels(levels map[NotificationClass]NotificationLevel) error {
	for class, level := range levels {
		if err := class.Validate(); err != nil {
			return err
		}
		if err := level.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func cloneNotificationLevels(source map[NotificationClass]NotificationLevel) map[NotificationClass]NotificationLevel {
	if source == nil {
		return nil
	}
	clone := make(map[NotificationClass]NotificationLevel, len(source))
	for class, level := range source {
		clone[class] = level
	}
	return clone
}
