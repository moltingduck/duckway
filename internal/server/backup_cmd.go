package server

import (
	"io"
	"os"

	"github.com/hackerduck/duckway/internal/database"
)

// RunBackupIfRequested handles `<binary> backup ...` for duckway-server and
// duckway-admin. It reports whether args selected the subcommand; when it
// did, the returned code is the process exit status and no server is started.
func RunBackupIfRequested(args []string, stdout, stderr io.Writer) (int, bool) {
	if len(args) < 2 || args[1] != "backup" {
		return 0, false
	}
	def := os.Getenv("DUCKWAY_DATA_DIR")
	if def == "" {
		def = DefaultConfig().DataDir
	}
	return database.RunBackupCommand(args[2:], def, stdout, stderr), true
}
