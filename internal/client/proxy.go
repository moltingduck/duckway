package client

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// RunProxy starts a local HTTP proxy that forwards requests to the Duckway server.
// Agents set HTTP_PROXY/HTTPS_PROXY to this local proxy.
//
// Request flow:
//   Agent → http://localhost:18080/proxy/openai/v1/chat/completions
//   → Duckway client proxy → http://duckway-server/proxy/openai/v1/chat/completions
//
// The client proxy injects the X-Duckway-Token header automatically.
func RunProxy(cfg *Config, syncInterval time.Duration) error {
	configDir := DefaultConfigDir()

	// Initial sync
	count, err := SyncKeys(configDir, cfg)
	if err != nil {
		log.Printf("Warning: initial key sync failed: %v", err)
	} else {
		log.Printf("Synced %d placeholder keys", count)
	}

	// Background sync
	if syncInterval > 0 {
		go func() {
			ticker := time.NewTicker(syncInterval)
			defer ticker.Stop()
			for range ticker.C {
				n, err := SyncKeys(configDir, cfg)
				if err != nil {
					log.Printf("Sync error: %v", err)
				} else {
					log.Printf("Synced %d keys", n)
				}
			}
		}()
	}

	handler := &proxyHandler{
		serverURL: cfg.ServerURL,
		token:     cfg.Token,
		client:    &http.Client{Timeout: 120 * time.Second},
	}

	addr := fmt.Sprintf(":%d", cfg.ProxyPort)
	log.Printf("Duckway proxy listening on %s", addr)
	log.Printf("Set HTTP_PROXY=http://localhost:%d for your agents", cfg.ProxyPort)

	return http.ListenAndServe(addr, handler)
}

type proxyHandler struct {
	serverURL string
	token     string
	client    *http.Client
}

func (p *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Forward the request to the Duckway server
	targetURL := strings.TrimRight(p.serverURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, `{"error":"failed to create proxy request"}`, http.StatusInternalServerError)
		return
	}

	// Copy headers
	for key, values := range r.Header {
		for _, v := range values {
			proxyReq.Header.Add(key, v)
		}
	}

	// Inject client token
	proxyReq.Header.Set("X-Duckway-Token", p.token)

	resp, err := p.client.Do(proxyReq)
	if err != nil {
		log.Printf("Upstream error: %v", err)
		http.Error(w, `{"error":"upstream request failed"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// PrintProxyEnv writes shell `export` statements for the local proxy to
// stdout, so they can be applied to the current shell or appended to a shell
// startup file:
//
//	eval "$(duckway env --proxy)"          # apply to the running shell
//	duckway env --proxy >> ~/.bashrc       # persist for future shells
//
// Both upper- and lower-case variants are emitted because different runtimes
// read different casings (Go/curl honor the upper-case set; some libraries
// only read lower-case). NO_PROXY keeps loopback direct so calls to the
// duckway server / proxy itself don't loop back through the proxy — matching
// the values mergeProxySettings writes into ~/.claude/settings.json.
func PrintProxyEnv(port int) {
	proxyURL := fmt.Sprintf("http://localhost:%d", port)
	const noProxy = "localhost,127.0.0.1"
	fmt.Printf("# Duckway proxy env — route agent traffic through the local proxy on port %d\n", port)
	fmt.Println("# Apply with:  eval \"$(duckway env --proxy)\"   or append to your shell startup file.")
	fmt.Printf("export HTTP_PROXY=%s\n", proxyURL)
	fmt.Printf("export HTTPS_PROXY=%s\n", proxyURL)
	fmt.Printf("export http_proxy=%s\n", proxyURL)
	fmt.Printf("export https_proxy=%s\n", proxyURL)
	fmt.Printf("export NO_PROXY=%s\n", noProxy)
	fmt.Printf("export no_proxy=%s\n", noProxy)
}

// WriteProxyEnvScript writes a shell script that sets proxy env vars.
func WriteProxyEnvScript(configDir string, port int) error {
	script := fmt.Sprintf(`#!/bin/sh
# Source this file to route traffic through Duckway proxy
export HTTP_PROXY=http://localhost:%d
export HTTPS_PROXY=http://localhost:%d
export http_proxy=http://localhost:%d
export https_proxy=http://localhost:%d
echo "Duckway proxy configured on port %d"
`, port, port, port, port, port)

	path := configDir + "/proxy-env.sh"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		return err
	}
	return nil
}
