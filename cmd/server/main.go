package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/server"
	"github.com/hackerduck/duckway/internal/version"
	"github.com/hackerduck/duckway/web"
)

func main() {
	port := flag.Int("port", 0, "Listen port (overrides DUCKWAY_LISTEN)")
	addr := flag.String("listen", "", "Listen address, e.g. :8080 (overrides DUCKWAY_LISTEN)")
	dataDir := flag.String("data", "", "Data directory (overrides DUCKWAY_DATA_DIR)")
	resetPassword := flag.Bool("reset-password", false, "Reset admin password to a fresh random value and exit")
	resetUsername := flag.String("reset-username", "duckway", "Username to reset (with --reset-password)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("duckway-server", version.Get())
		return
	}

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

	if *resetPassword {
		newPw, err := server.ResetAdminPassword(db, *resetUsername)
		if err != nil {
			log.Fatalf("Reset failed: %v", err)
		}
		fmt.Println("========================================")
		fmt.Printf("  Admin password reset for: %s\n", *resetUsername)
		fmt.Printf("  New password: %s\n", newPw)
		fmt.Println("  (shown once — save it now)")
		fmt.Println("========================================")
		return
	}

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

	httpSrv := &http.Server{Addr: config.ListenAddr, Handler: srv}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		log.Printf("[server] SIGTERM received — draining connections (30s max)...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("[server] Shutdown error: %v", err)
		}
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
	log.Printf("[server] Stopped.")
}
