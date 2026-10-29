package clientplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Server requests must not loop through the local Duckway proxy. Never inherit
// an ambient proxy when sending the Duckway client token to its configured host.
var managedTransport = &http.Transport{
	Proxy:               nil,
	DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout: 10 * time.Second,
	IdleConnTimeout:     90 * time.Second,
}

// ServerOrigin normalizes a configured Duckway origin. A plugin operation never
// accepts another server, a publisher URL, or a server path override.
func ServerOrigin(raw string) (string, error) {
	u, err := SourceURL(raw)
	if err != nil || u.Path != "/" {
		return "", fmt.Errorf("configured Duckway server must be an HTTP(S) origin without credentials, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "http" {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("configured Duckway plugin server requires HTTPS except literal loopback development origins")
		}
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}

func (h Host) remoteOrigin() (string, error) {
	if strings.TrimSpace(h.Token) == "" {
		return "", fmt.Errorf("configured Duckway client token is required; run duckway init")
	}
	return ServerOrigin(h.ServerURL)
}

func managedArtifactPath(m Manifest, a Artifact) string {
	return "/client/plugins/" + m.Name + "/releases/" + m.Version + "/" + a.OS + "/" + a.Arch
}

// ValidateManagedManifest additionally requires every artifact to use the exact
// authenticated server release route, never an upstream publisher or proxy route.
func ValidateManagedManifest(m Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	for _, a := range m.Artifacts {
		if a.URL != managedArtifactPath(m, a) {
			return fmt.Errorf("plugin artifact is outside its managed server release route")
		}
	}
	return nil
}

// Catalog reads only approved cached server releases. Requests never cause an
// upstream publisher fetch; that is a separate administrator import operation.
func (h Host) Catalog(ctx context.Context) ([]Manifest, error) {
	resp, err := h.get(ctx, "/client/plugins")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	const maxCatalogSize = 16 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogSize+1))
	if err != nil || len(data) > maxCatalogSize {
		return nil, fmt.Errorf("plugin catalog read failed or exceeds size limit")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var plugins []Manifest
	if err := dec.Decode(&plugins); err != nil {
		return nil, fmt.Errorf("invalid managed plugin catalog")
	}
	if err := dec.Decode(new(any)); err != io.EOF || plugins == nil || len(plugins) > 256 {
		return nil, fmt.Errorf("invalid managed plugin catalog")
	}
	seen := make(map[string]bool)
	for _, m := range plugins {
		if err := ValidateManagedManifest(m); err != nil {
			return nil, err
		}
		if seen[m.Name] {
			return nil, fmt.Errorf("duplicate managed plugin name")
		}
		seen[m.Name] = true
	}
	return plugins, nil
}

func (h Host) managedClient() http.Client {
	c := http.Client{Timeout: 2 * time.Minute, Transport: managedTransport}
	if h.HTTPClient != nil {
		c = *h.HTTPClient
	}
	c.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return fmt.Errorf("plugin redirects are forbidden") }
	if c.Timeout == 0 {
		c.Timeout = 2 * time.Minute
	}
	return c
}
