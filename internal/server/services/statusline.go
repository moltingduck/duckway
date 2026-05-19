package services

import (
	_ "embed"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// DefaultStatuslineScript is the bash script seeded into the
// agent_statusline_script setting on first server start. It reads
// Claude Code's status JSON from stdin and prints one line with the
// working folder, 5-hour usage, and 7-day usage. Admins can replace
// it from /admin/settings → Agent Statusline.
//
//go:embed statusline_default.sh
var DefaultStatuslineScript string

// settingStatuslineSeeded is the sentinel that records "we already
// installed the default once". Without this, an admin clearing the
// textarea (to disable the statusline) would have their empty value
// re-seeded back to the default on the next server restart.
const settingStatuslineSeeded = "agent_statusline_seeded"

// SeedDefaultStatusline installs DefaultStatuslineScript into the
// agent_statusline_script setting on first run. Idempotent: a sentinel
// setting records whether we've already seeded, so any subsequent
// admin edit (including clearing the textarea to disable the feature)
// sticks across restarts.
//
// Safe to call from every server entrypoint — New / NewAdmin /
// NewGateway all funnel through here so a split deployment seeds once
// regardless of which process boots first.
func SeedDefaultStatusline(q *queries.SettingsQueries) {
	if q.Get(settingStatuslineSeeded) == "1" {
		return
	}
	_ = q.Set(queries.SettingAgentStatuslineScript, DefaultStatuslineScript)
	_ = q.Set(settingStatuslineSeeded, "1")
}
