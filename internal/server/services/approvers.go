package services

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// ErrNotApprover is returned for a chat decision from someone who is not on the
// channel's approver list (an empty list means nobody is).
var ErrNotApprover = errors.New("not an approver")

// Notification channel config keys that hold the approver allowlist. They are
// plain comma/space separated ID strings because channel configs hold only
// plain values (see handlers.validateNotificationConfig).
const (
	ApproverUsersKey = "approver_user_ids" // Discord or Telegram numeric user IDs
	ApproverRolesKey = "approver_role_ids" // Discord role IDs (discord_bot only)
)

// IsApproverConfigKey reports whether a config key belongs to the allowlist.
func IsApproverConfigKey(k string) bool { return k == ApproverUsersKey || k == ApproverRolesKey }

var idPattern = regexp.MustCompile(`^[0-9]{1,24}$`)

// ValidApproverID reports whether s looks like a Discord or Telegram numeric ID.
func ValidApproverID(s string) bool { return idPattern.MatchString(s) }

// SplitApproverIDs splits a comma/space/semicolon/newline separated list and
// drops empties and duplicates, preserving order.
func SplitApproverIDs(raw string) []string {
	f := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' || r == '\r' })
	seen := map[string]bool{}
	out := []string{}
	for _, s := range f {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ApproverList is a channel's allowlist.
type ApproverList struct {
	Users []string
	Roles []string
}

// ParseApprovers reads the allowlist from a channel config JSON document.
// Unparseable config yields an empty list: fail closed.
func ParseApprovers(configJSON string) ApproverList {
	var m map[string]any
	if json.Unmarshal([]byte(configJSON), &m) != nil {
		return ApproverList{}
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	return ApproverList{Users: SplitApproverIDs(str(ApproverUsersKey)), Roles: SplitApproverIDs(str(ApproverRolesKey))}
}

// Empty reports whether nobody may decide.
func (l ApproverList) Empty() bool { return len(l.Users) == 0 && len(l.Roles) == 0 }

// Allows reports whether actor is a listed user or holds a listed role. Roles
// only count for Discord actors.
func (l ApproverList) Allows(a ChatActor) bool {
	if a.UserID == "" {
		return false
	}
	for _, u := range l.Users {
		if u == a.UserID {
			return true
		}
	}
	if a.Kind == "discord" {
		for _, want := range l.Roles {
			for _, have := range a.RoleIDs {
				if want == have {
					return true
				}
			}
		}
	}
	return false
}
