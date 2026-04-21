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
	port := flag.Int("port", 0, "Listen port (overrides DUCKWAY_LISTEN)")
	addr := flag.String("listen", "", "Listen address, e.g. :8080 (overrides DUCKWAY_LISTEN)")
	dataDir := flag.String("data", "", "Data directory (overrides DUCKWAY_DATA_DIR)")
	flag.Parse()

	config := server.DefaultConfig()

	if *port > 0 {
		config.ListenAddr = fmt.Sprintf(":%d", *port)
	}
	if *addr != "" {
		config.ListenAddr = *addr
	}
	if *dataDir != "" {
		config.DataDir = *dataDir
	}

	if err := config.Init(); err != nil {
		log.Fatalf("Failed to initialize config: %v", err)
	}

	db, err := database.Open(config.DataDir)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// In dev mode, serve templates + static from disk (live reload on refresh)
	// In production, use embedded FS (single binary)
	var contentFS fs.FS
	webDir := os.Getenv("DUCKWAY_WEB_DIR")
	if webDir != "" {
		contentFS = os.DirFS(webDir)
		log.Printf("Dev mode: serving web assets from %s (live reload)", webDir)
	} else {
		contentFS = web.Content
	}

	srv, err := server.New(config, db, contentFS)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	log.Printf("Duckway server listening on %s", config.ListenAddr)
	log.Printf("Data directory: %s", config.DataDir)
	log.Printf("Admin panel: http://localhost%s/admin/", config.ListenAddr)

	if err := http.ListenAndServe(config.ListenAddr, srv); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
