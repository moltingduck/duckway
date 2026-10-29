package client

import (
	"context"
	"fmt"

	"github.com/hackerduck/duckway/internal/clientplugin"
)

// ReportPluginInventory reuses the existing sync/control heartbeat lifecycle.
// Report failures are observational and must not fail core sync or update jobs.
func ReportPluginInventory(ctx context.Context, configDir string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("duckway configuration is required for plugin reporting")
	}
	return (clientplugin.Host{ConfigDir: configDir, ServerURL: cfg.ServerURL, Token: cfg.Token}).Report(ctx)
}
