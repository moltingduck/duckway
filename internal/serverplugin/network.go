// Package serverplugin manages approved plugin releases without executing them.
package serverplugin

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/clientplugin"
)

var errNetwork = errors.New("plugin source is unavailable or disallowed")

// NetworkPolicy is operator configuration, never derived from an API request.
// AllowedOrigins permits private HTTPS and private/loopback HTTP for exact origins.
type NetworkPolicy struct {
	allowed  map[string]bool
	lookupIP func(context.Context, string, string) ([]netip.Addr, error)
}

func NewNetworkPolicy(origins string) (*NetworkPolicy, error) {
	p := &NetworkPolicy{allowed: make(map[string]bool), lookupIP: net.DefaultResolver.LookupNetIP}
	for _, raw := range strings.Split(origins, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := clientplugin.SourceURL(raw)
		if err != nil || u.Path != "/" {
			return nil, errors.New("invalid plugin allowed origin")
		}
		p.allowed[origin(u)] = true
	}
	return p, nil
}
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// Deny special/metadata ranges even when an origin is explicitly allowed.
var forbiddenPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("168.63.129.16/32"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("fd00:ec2::254/128"), netip.MustParsePrefix("fec0::/10"), netip.MustParsePrefix("2002::/16"),
}

func permittedIP(ip netip.Addr, allowPrivate bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	for _, prefix := range forbiddenPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	if ip.IsPrivate() || ip.IsLoopback() {
		return allowPrivate
	}
	return ip.IsGlobalUnicast()
}

func (p *NetworkPolicy) client(source *url.URL) (*http.Client, error) {
	allowed := p.allowed[origin(source)]
	if source.Scheme != "https" && (source.Scheme != "http" || !allowed) {
		return nil, errNetwork
	}
	tr := &http.Transport{
		Proxy: nil, DisableCompression: true, MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errNetwork
			}
			ips, err := p.lookupIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, errNetwork
			}
			// Validate every answer, then dial the validated literal; DNS cannot rebind between checks and connect.
			for _, ip := range ips {
				if !permittedIP(ip, allowed) || (source.Scheme == "http" && !ip.IsPrivate() && !ip.IsLoopback()) {
					return nil, errNetwork
				}
			}
			dialer := net.Dialer{Timeout: 10 * time.Second}
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
			}
			return nil, errNetwork
		},
	}
	return &http.Client{Transport: tr, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return errNetwork }}, nil
}
