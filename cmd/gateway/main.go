package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/server"
)

func main() {
	port := flag.Int("port", 0, "Listen port (default: 8080 or DUCKWAY_GATEWAY_LISTEN)")
	dataDir := flag.String("data", "", "Data directory")
	flag.Parse()

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
