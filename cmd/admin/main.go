package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/server"
	"github.com/hackerduck/duckway/web"
)

func main() {
	port := flag.Int("port", 0, "Listen port (default: 9090 or DUCKWAY_ADMIN_LISTEN)")
	dataDir := flag.String("data", "", "Data directory")
	flag.Parse()

	config := server.DefaultConfig()
	config.ListenAddr = envOrDefault("DUCKWAY_ADMIN_LISTEN", ":9090")

	if *port > 0 {
		config.ListenAddr = fmt.Sprintf(":%d", *port)
	}
	if *dataDir != "" {
		config.DataDir = *dataDir
	}

	if err := config.Init(); err != nil {
		log.Fatalf("Config: %v", err)
	}

	db, err := database.Open(config.DataDir)
	if err != nil {
		log.Fatalf("Database: %v", err)
	}
	defer db.Close()

	var contentFS fs.FS
	if webDir := os.Getenv("DUCKWAY_WEB_DIR"); webDir != "" {
		contentFS = os.DirFS(webDir)
		log.Printf("Dev mode: live reload from %s", webDir)
	} else {
		contentFS = web.Content
	}

	srv, err := server.NewAdmin(config, db, contentFS)
	if err != nil {
		log.Fatalf("Server: %v", err)
	}

	log.Printf("Duckway ADMIN listening on %s", config.ListenAddr)
	log.Printf("Admin panel: http://localhost%s/admin/", config.ListenAddr)

	if err := http.ListenAndServe(config.ListenAddr, srv); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
