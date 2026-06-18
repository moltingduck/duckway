package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/server"
	"github.com/hackerduck/duckway/internal/version"
)

func main() {
	port := flag.Int("port", 0, "Listen port (default: 8080 or DUCKWAY_GATEWAY_LISTEN)")
	dataDir := flag.String("data", "", "Data directory")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("duckway-gateway", version.Get())
		return
	}

	config := server.DefaultConfig()
	config.ListenAddr = envOrDefault("DUCKWAY_GATEWAY_LISTEN", ":8080")

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

	srv, err := server.NewGateway(config, db)
	if err != nil {
		log.Fatalf("Server: %v", err)
	}

	log.Printf("Duckway GATEWAY listening on %s", config.ListenAddr)
	log.Printf("Proxy: /proxy/{service}/... | Client API: /client/* | Install: /install.sh")

	httpSrv := &http.Server{Addr: config.ListenAddr, Handler: srv}

	// Graceful shutdown: drain in-flight requests before exiting.
	// Docker sends SIGTERM on container stop; without this the process ignores
	// it and is SIGKILL'd after the stop timeout, cutting all active connections.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		log.Printf("[gateway] SIGTERM received — draining connections (30s max)...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("[gateway] Shutdown error: %v", err)
		}
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Error: %v", err)
	}
	log.Printf("[gateway] Stopped.")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
