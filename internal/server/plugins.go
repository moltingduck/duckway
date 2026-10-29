package server

import (
	"log"
	"os"

	"github.com/hackerduck/duckway/internal/serverplugin"
)

func (s *Server) startPluginChecker() {
	store, err := serverplugin.New(s.config.DataDir, os.Getenv("DUCKWAY_PLUGIN_ALLOWED_ORIGINS"))
	if err != nil {
		log.Print("Plugin source checks disabled: invalid server configuration")
		return
	}
	s.register(serverplugin.StartChecker(store, s.stopCh))
}
