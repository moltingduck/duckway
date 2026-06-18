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
	port := flag.Int("port", 0, "Listen port (default: 9090 or DUCKWAY_ADMIN_LISTEN)")
	dataDir := flag.String("data", "", "Data directory")
	resetPassword := flag.Bool("reset-password", false, "Reset admin password to a fresh random value and exit")
	resetUsername := flag.String("reset-username", "duckway", "Username to reset (with --reset-password)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("duckway-admin", version.Get())
		return
	}

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

	httpSrv := &http.Server{Addr: config.ListenAddr, Handler: srv}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		log.Printf("[admin] SIGTERM received — draining connections (30s max)...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("[admin] Shutdown error: %v", err)
		}
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Error: %v", err)
	}
	srv.Shutdown()
	log.Printf("[admin] Stopped.")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
