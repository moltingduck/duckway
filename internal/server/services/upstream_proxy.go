package services

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

const encryptedUpstreamProxyPrefix = "enc:"

// UpstreamProxyClientCache returns immutable HTTP clients keyed by outbound
// proxy URL. Transports are never mutated after first use.
type UpstreamProxyClientCache struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

func NewUpstreamProxyClientCache() *UpstreamProxyClientCache {
	return &UpstreamProxyClientCache{clients: make(map[string]*http.Client)}
}

func (c *UpstreamProxyClientCache) Client(rawURL string) (*http.Client, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, nil
	}
	if err := ValidateUpstreamProxyURL(rawURL); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.clients[rawURL]; existing != nil {
		return existing, nil
	}
	transport, err := transportForUpstreamProxy(rawURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport}
	c.clients[rawURL] = client
	return client, nil
}

func ValidateUpstreamProxyURL(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid upstream_proxy_url")
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("upstream_proxy_url must include scheme and host")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return fmt.Errorf("upstream_proxy_url scheme must be http, https, socks4, socks4a, or socks5")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("upstream_proxy_url must not include query or fragment")
	}
	return nil
}

func EncryptUpstreamProxyURL(c *Crypto, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil
	}
	if err := ValidateUpstreamProxyURL(rawURL); err != nil {
		return "", err
	}
	if c == nil {
		return "", fmt.Errorf("crypto service unavailable")
	}
	encrypted, err := c.Encrypt(rawURL)
	if err != nil {
		return "", err
	}
	return encryptedUpstreamProxyPrefix + encrypted, nil
}

func DecryptUpstreamProxyURL(c *Crypto, stored string) (string, error) {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, encryptedUpstreamProxyPrefix) {
		return stored, nil
	}
	if c == nil {
		return "", fmt.Errorf("crypto service unavailable")
	}
	return c.Decrypt(strings.TrimPrefix(stored, encryptedUpstreamProxyPrefix))
}

func RedactProxyURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.User == nil {
		return rawURL
	}
	if username := u.User.Username(); username != "" {
		u.User = url.User(username)
	} else {
		u.User = nil
	}
	return u.String()
}

func RedactProxyError(proxyURL string, err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	redacted := RedactProxyURL(proxyURL)
	if proxyURL != "" && proxyURL != redacted {
		msg = strings.ReplaceAll(msg, proxyURL, redacted)
	}
	if u, parseErr := url.Parse(strings.TrimSpace(proxyURL)); parseErr == nil && u.User != nil {
		if pass, ok := u.User.Password(); ok && pass != "" {
			msg = strings.ReplaceAll(msg, pass, "[redacted]")
		}
		if user := u.User.Username(); user != "" {
			msg = strings.ReplaceAll(msg, user+":[redacted]@", user+"@")
		}
	}
	return msg
}

func transportForUpstreamProxy(rawURL string) (*http.Transport, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 2 * time.Minute
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		dialer, err := socks5Dialer(u)
		if err != nil {
			return nil, err
		}
		transport.DialContext = dialer.DialContext
	case "socks4", "socks4a":
		userID := ""
		if u.User != nil {
			userID = u.User.Username()
		}
		transport.DialContext = (&socks4Dialer{
			proxyAddress: u.Host,
			userID:       userID,
			resolveNames: strings.EqualFold(u.Scheme, "socks4a"),
		}).DialContext
	default:
		return nil, fmt.Errorf("unsupported upstream proxy scheme")
	}
	return transport, nil
}

func socks5Dialer(u *url.URL) (proxy.ContextDialer, error) {
	var auth *proxy.Auth
	if u.User != nil {
		auth = &proxy.Auth{User: u.User.Username()}
		if pass, ok := u.User.Password(); ok {
			auth.Password = pass
		}
	}
	dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil, err
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks5 dialer does not support context cancellation")
	}
	return contextDialer, nil
}

type socks4Dialer struct {
	proxyAddress string
	userID       string
	resolveNames bool
}

func (d *socks4Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("socks4 only supports tcp")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		return nil, err
	}
	reqHost := host
	ip := net.ParseIP(host).To4()
	if ip == nil && !d.resolveNames {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			if v4 := addr.IP.To4(); v4 != nil {
				ip = v4
				break
			}
		}
		if ip == nil {
			return nil, fmt.Errorf("socks4 requires an IPv4 target")
		}
	}
	if ip == nil {
		ip = net.IPv4(0, 0, 0, 1)
	}

	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", d.proxyAddress)
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetDeadline(deadline)
	}
	defer func() {
		_ = conn.SetDeadline(time.Time{})
	}()

	req := []byte{0x04, 0x01, 0, 0, ip[0], ip[1], ip[2], ip[3]}
	binary.BigEndian.PutUint16(req[2:4], uint16(port))
	req = append(req, []byte(d.userID)...)
	req = append(req, 0)
	if d.resolveNames {
		req = append(req, []byte(reqHost)...)
		req = append(req, 0)
	}
	if _, err := conn.Write(req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp := make([]byte, 8)
	if _, err := ioReadFull(ctx, conn, resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp[1] != 0x5a {
		_ = conn.Close()
		return nil, fmt.Errorf("socks4 proxy rejected request with status 0x%02x", resp[1])
	}
	return conn, nil
}

func ioReadFull(ctx context.Context, conn net.Conn, buf []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n := 0
		for n < len(buf) {
			m, err := conn.Read(buf[n:])
			n += m
			if err != nil {
				ch <- result{n: n, err: err}
				return
			}
		}
		ch <- result{n: n}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-ctx.Done():
		_ = conn.Close()
		return 0, ctx.Err()
	}
}
