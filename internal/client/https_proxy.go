package client

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

type serviceInfo struct {
	Name        string `json:"name"`
	HostPattern string `json:"host_pattern"`
}

type httpsProxy struct {
	serverURL  string
	token      string
	ca         *services.CAManager
	certCache  sync.Map // hostname -> *tls.Certificate
	hostMap    map[string]string // hostname -> service name
	httpClient *http.Client
}

// RunHTTPSProxy starts the proxy that handles both HTTP and HTTPS CONNECT.
func RunHTTPSProxy(cfg *Config, syncInterval time.Duration) error {
	configDir := DefaultConfigDir()

	// Initial sync
	count, err := SyncKeys(configDir, cfg)
	if err != nil {
		log.Printf("Warning: initial key sync failed: %v", err)
	} else {
		log.Printf("Synced %d placeholder keys", count)
	}

	// Load CA cert + key for MITM
	caDir := filepath.Dir(KeysEnvPath(configDir))
	ca, err := loadClientCA(caDir)
	if err != nil {
		log.Printf("Warning: no CA cert — HTTPS MITM disabled (%v)", err)
		log.Printf("Run 'duckway init' to download the CA cert from the server")
	}

	// Fetch service host map
	hostMap := fetchServiceHosts(cfg.ServerURL, cfg.Token)
	if len(hostMap) > 0 {
		log.Printf("HTTPS MITM enabled for %d services:", len(hostMap))
		for host, svc := range hostMap {
			log.Printf("  %s → /proxy/%s/", host, svc)
		}
	}

	// Background sync
	if syncInterval > 0 {
		go func() {
			ticker := time.NewTicker(syncInterval)
			defer ticker.Stop()
			for range ticker.C {
				n, _ := SyncKeys(configDir, cfg)
				log.Printf("Synced %d keys", n)
				// Refresh host map
				if newMap := fetchServiceHosts(cfg.ServerURL, cfg.Token); len(newMap) > 0 {
					hostMap = newMap
				}
			}
		}()
	}

	proxy := &httpsProxy{
		serverURL:  cfg.ServerURL,
		token:      cfg.Token,
		ca:         ca,
		hostMap:    hostMap,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}

	addr := fmt.Sprintf(":%d", cfg.ProxyPort)
	log.Printf("Duckway proxy listening on %s (HTTP + HTTPS CONNECT)", addr)
	log.Printf("Configure agents with:")
	log.Printf("  export HTTPS_PROXY=http://localhost:%d", cfg.ProxyPort)
	log.Printf("  export HTTP_PROXY=http://localhost:%d", cfg.ProxyPort)

	return http.ListenAndServe(addr, proxy)
}

func (p *httpsProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// Regular HTTP proxy (existing behavior)
	p.handleHTTP(w, r)
}

// handleHTTP forwards regular HTTP requests to the Duckway server.
func (p *httpsProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	targetURL := strings.TrimRight(p.serverURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}

	for key, values := range r.Header {
		for _, v := range values {
			proxyReq.Header.Add(key, v)
		}
	}
	proxyReq.Header.Set("X-Duckway-Token", p.token)

	resp, err := p.httpClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleConnect handles HTTPS CONNECT tunnels.
// For known service hosts: MITM, decrypt, forward via /proxy/{svc}/.
// For unknown hosts: transparent TCP tunnel.
func (p *httpsProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	svcName, isMITM := p.hostMap[host]
	if !isMITM || p.ca == nil {
		// Unknown host or no CA: transparent tunnel
		p.tunnelConnect(w, r)
		return
	}

	// MITM: intercept, decrypt, forward via /proxy/{svc}/
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	// Tell client the tunnel is established
	w.WriteHeader(http.StatusOK)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Hijack error: %v", err)
		return
	}
	defer clientConn.Close()

	// Get or create TLS cert for this host
	tlsCert := p.getCert(host)
	if tlsCert == nil {
		log.Printf("Failed to create cert for %s", host)
		return
	}

	// TLS handshake with the client (pretending to be the target host)
	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*tlsCert},
	})
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("TLS handshake error for %s: %v", host, err)
		return
	}
	defer tlsConn.Close()

	// Read decrypted HTTP requests from the client
	reader := bufio.NewReader(tlsConn)
	for {
		tlsConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		req, err := http.ReadRequest(reader)
		if err != nil {
			break
		}

		// Forward via /proxy/{svc}/{path}
		p.forwardMITM(tlsConn, req, svcName, host)
	}
}

func (p *httpsProxy) forwardMITM(tlsConn *tls.Conn, req *http.Request, svcName, host string) {
	path := req.URL.Path
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}

	targetURL := strings.TrimRight(p.serverURL, "/") + "/proxy/" + svcName + path

	var body io.Reader
	if req.Body != nil {
		body = req.Body
	}

	proxyReq, err := http.NewRequest(req.Method, targetURL, body)
	if err != nil {
		writeHTTPError(tlsConn, 502, "proxy error")
		return
	}

	// Copy headers (skip host, connection)
	for key, values := range req.Header {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "connection" || lower == "proxy-connection" {
			continue
		}
		for _, v := range values {
			proxyReq.Header.Add(key, v)
		}
	}
	proxyReq.Header.Set("X-Duckway-Token", p.token)

	resp, err := p.httpClient.Do(proxyReq)
	if err != nil {
		writeHTTPError(tlsConn, 502, "upstream error")
		return
	}
	defer resp.Body.Close()

	resp.Write(tlsConn)
}

// tunnelConnect creates a transparent TCP tunnel for unknown hosts.
func (p *httpsProxy) tunnelConnect(w http.ResponseWriter, r *http.Request) {
	targetConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, "connect failed", http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	go io.Copy(targetConn, clientConn)
	io.Copy(clientConn, targetConn)
}

func (p *httpsProxy) getCert(hostname string) *tls.Certificate {
	if cached, ok := p.certCache.Load(hostname); ok {
		return cached.(*tls.Certificate)
	}

	certPEM, keyPEM, err := p.ca.SignHost(hostname)
	if err != nil {
		log.Printf("Sign host cert error for %s: %v", hostname, err)
		return nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Printf("Parse cert error for %s: %v", hostname, err)
		return nil
	}

	p.certCache.Store(hostname, &cert)
	return &cert
}

func writeHTTPError(w io.Writer, code int, msg string) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\n\r\n%s", code, msg, len(msg), msg)
}

func loadClientCA(configDir string) (*services.CAManager, error) {
	certPath := filepath.Join(configDir, "ca.pem")
	keyPath := filepath.Join(configDir, "ca-key.pem")

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("no CA cert at %s", certPath)
	}

	// Client only needs cert for verification, but for MITM we need the key too.
	// The key is downloaded from the server during init.
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("no CA key at %s", keyPath)
	}

	return parseClientCA(certPEM, keyPEM)
}

func parseClientCA(certPEM, keyPEM []byte) (*services.CAManager, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("failed to decode CA cert")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("failed to decode CA key")
	}

	// Try EC key first, then PKCS8
	ecKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}

	return &services.CAManager{CertPEM: certPEM, KeyPEM: keyPEM, Cert: cert, Key: ecKey}, nil
}

func fetchServiceHosts(serverURL, token string) map[string]string {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", serverURL+"/client/services", nil)
	req.Header.Set("X-Duckway-Token", token)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Warning: failed to fetch service hosts: %v", err)
		return nil
	}
	defer resp.Body.Close()

	var svcs []serviceInfo
	json.NewDecoder(resp.Body).Decode(&svcs)

	hostMap := make(map[string]string)
	for _, s := range svcs {
		hostMap[s.HostPattern] = s.Name
	}
	return hostMap
}
