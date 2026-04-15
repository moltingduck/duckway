package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/client"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	configDir := client.DefaultConfigDir()

	switch os.Args[1] {
	case "init":
		cmdInit(configDir)
	case "sync":
		cmdSync(configDir)
	case "env":
		cmdEnv(configDir)
	case "proxy":
		cmdProxy(configDir)
	case "status":
		cmdStatus(configDir)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`duckway — API proxy client for AI agents

Usage:
  duckway init     Register this machine with a Duckway server
  duckway sync     Fetch placeholder keys from server
  duckway env      Print keys as shell export statements
  duckway proxy    Start local proxy (forwards to server)
  duckway status   Show connection status

Config directory: ~/.duckway/`)
}

func cmdInit(configDir string) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Duckway server URL (e.g., http://192.168.1.100:8080): ")
	serverURL, _ := reader.ReadString('\n')
	serverURL = strings.TrimSpace(serverURL)

	fmt.Print("Client name (e.g., my-laptop): ")
	clientName, _ := reader.ReadString('\n')
	clientName = strings.TrimSpace(clientName)

	fmt.Println("\nChoose authentication method:")
	fmt.Println("  1. Enter a pre-shared token (admin already created this client)")
	fmt.Println("  2. Login as admin to register this client")
	fmt.Print("Choice [1/2]: ")
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)

	var token string

	switch choice {
	case "2":
		fmt.Print("Admin username: ")
		username, _ := reader.ReadString('\n')
		username = strings.TrimSpace(username)

		fmt.Print("Admin password: ")
		password, _ := reader.ReadString('\n')
		password = strings.TrimSpace(password)

		session, err := client.AdminLogin(serverURL, username, password)
		if err != nil {
			log.Fatalf("Login failed: %v", err)
		}

		_, tok, err := client.RegisterClient(serverURL, session, clientName)
		if err != nil {
			log.Fatalf("Registration failed: %v", err)
		}
		token = tok
		fmt.Printf("Client registered successfully!\n")

	default:
		fmt.Print("Client token: ")
		token, _ = reader.ReadString('\n')
		token = strings.TrimSpace(token)
	}

	cfg := &client.Config{
		ServerURL:  serverURL,
		ClientName: clientName,
		Token:      token,
		ProxyPort:  18080,
	}

	// Verify connection
	api := client.NewAPIClient(serverURL, token)
	if err := api.Ping(); err != nil {
		log.Fatalf("Server connection failed: %v", err)
	}

	if err := client.SaveConfig(configDir, cfg); err != nil {
		log.Fatalf("Failed to save config: %v", err)
	}

	// Write proxy env script
	client.WriteProxyEnvScript(configDir, cfg.ProxyPort)

	// Download CA cert for HTTPS proxy
	if err := api.DownloadCA(configDir); err != nil {
		log.Printf("Warning: CA cert download failed: %v", err)
		log.Printf("HTTPS proxy MITM will not work — only HTTP proxy mode available")
	} else {
		fmt.Println("CA certificate downloaded for HTTPS proxy")
		// Try to install to system trust store
		if err := client.InstallCACert(configDir); err != nil {
			log.Printf("Warning: could not install CA to system trust store: %v", err)
			fmt.Printf("Manually install: sudo cp %s/ca.pem /usr/local/share/ca-certificates/duckway.crt && sudo update-ca-certificates\n", configDir)
		} else {
			fmt.Println("CA certificate installed to system trust store")
		}
	}

	// Initial sync
	count, err := client.SyncKeys(configDir, cfg)
	if err != nil {
		log.Printf("Warning: initial sync failed: %v", err)
	} else {
		fmt.Printf("Synced %d placeholder keys\n", count)
	}

	fmt.Printf("\nConfig saved to %s/config.yaml\n", configDir)
	fmt.Println("\nNext steps:")
	fmt.Println("  duckway proxy              — start HTTPS proxy")
	fmt.Printf("  export HTTPS_PROXY=http://localhost:%d\n", cfg.ProxyPort)
	fmt.Printf("  export HTTP_PROXY=http://localhost:%d\n", cfg.ProxyPort)
}

func cmdSync(configDir string) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}

	count, err := client.SyncKeys(configDir, cfg)
	if err != nil {
		log.Fatalf("Sync failed: %v", err)
	}

	fmt.Printf("Synced %d placeholder keys to %s\n", count, client.KeysEnvPath(configDir))
}

func cmdEnv(configDir string) {
	if err := client.PrintEnv(configDir); err != nil {
		log.Fatal(err)
	}
}

func cmdProxy(configDir string) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}

	// Parse port from args if provided
	for i, arg := range os.Args {
		if arg == "--port" && i+1 < len(os.Args) {
			fmt.Sscanf(os.Args[i+1], "%d", &cfg.ProxyPort)
		}
	}

	syncInterval := 5 * time.Minute
	if err := client.RunHTTPSProxy(cfg, syncInterval); err != nil {
		log.Fatal(err)
	}
}

func cmdStatus(configDir string) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		fmt.Println("Status: not initialized")
		fmt.Println("Run 'duckway init' to set up")
		return
	}

	fmt.Printf("Server:      %s\n", cfg.ServerURL)
	fmt.Printf("Client name: %s\n", cfg.ClientName)
	fmt.Printf("Proxy port:  %d\n", cfg.ProxyPort)

	api := client.NewAPIClient(cfg.ServerURL, cfg.Token)
	if err := api.Ping(); err != nil {
		fmt.Printf("Connection:  FAILED (%v)\n", err)
		return
	}
	fmt.Println("Connection:  OK")

	keys, err := api.FetchKeys()
	if err != nil {
		fmt.Printf("Keys:        error (%v)\n", err)
	} else {
		fmt.Printf("Keys:        %d placeholder keys assigned\n", len(keys))
		for _, k := range keys {
			if k.EnvName == "DUCKWAY_HEARTBEAT" {
				continue // don't show heartbeat in key list
			}
			fmt.Printf("  %s (%s) = %s...%s\n", k.EnvName, k.ServiceName, k.Placeholder[:12], k.Placeholder[len(k.Placeholder)-4:])
		}
	}

	// Test heartbeat proxy
	hbResult := api.Heartbeat()
	if hbResult == nil {
		fmt.Println("Heartbeat:   OK (proxy reachable)")
	} else {
		fmt.Printf("Heartbeat:   FAILED (%v)\n", hbResult)
	}

	// Check if local proxy is running
	proxyURL := fmt.Sprintf("http://localhost:%d", cfg.ProxyPort)
	proxyRunning := false
	resp, err := http.Get(proxyURL + "/proxy/heartbeat/ping")
	if err == nil {
		resp.Body.Close()
		// The proxy doesn't handle plain GET without token, but if we get a response it's alive
		proxyRunning = true
	}

	if proxyRunning {
		fmt.Printf("Local proxy: RUNNING on %s\n", proxyURL)
		fmt.Printf("  export HTTPS_PROXY=%s\n", proxyURL)
		fmt.Printf("  export HTTP_PROXY=%s\n", proxyURL)
	} else {
		fmt.Printf("Local proxy: NOT RUNNING (start with: duckway proxy)\n")
		fmt.Printf("  Will listen on %s\n", proxyURL)
	}

	// Check CA cert
	caPath := configDir + "/ca.pem"
	if _, err := os.Stat(caPath); err == nil {
		fmt.Println("CA cert:     installed")
	} else {
		fmt.Println("CA cert:     MISSING (run duckway init to download)")
	}
}
