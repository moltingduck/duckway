package ducklord

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const maxSkillArchiveBytes int64 = MaxSkillBytes + 4<<20

// skillHTTPSClient is a seam for tests. Production calls use the default client.
var skillHTTPSClient = http.DefaultClient

// skillHTTPSLookupIP is injectable so URL policy can be tested without making
// network lookups. Production fetches use the system resolver.
var skillHTTPSLookupIP = net.DefaultResolver.LookupIPAddr

// skillHTTPSNetDial is injectable for tests. Production dialing always goes
// through the address policy check in skillHTTPSDialContext.
var skillHTTPSNetDial = (&net.Dialer{}).DialContext

// FetchHTTPSArchive downloads and imports a public HTTPS tar.gz skill archive.
func FetchHTTPSArchive(ctx context.Context, rawURL string, insecure bool, repository, identifier string) (SkillInfo, error) {
	preview, err := PrepareHTTPSArchive(ctx, rawURL, insecure, repository, identifier)
	if err != nil {
		return SkillInfo{}, err
	}
	return CommitSkillPreview(&preview)
}

// PrepareHTTPSArchive downloads and validates a public HTTPS archive into a
// private staging directory. The managed skill is unchanged until commit.
func PrepareHTTPSArchive(ctx context.Context, rawURL string, insecure bool, repository, identifier string) (SkillPreview, error) {
	u, err := validateSkillHTTPSURL(rawURL)
	if err != nil {
		return SkillPreview{}, err
	}
	if err := validatePublicHTTPSDestination(ctx, u); err != nil {
		return SkillPreview{}, err
	}
	if !SafeIdentifier(identifier) {
		return SkillPreview{}, fmt.Errorf("unsafe skill identifier %q", identifier)
	}
	if err := ensureRepository(repository); err != nil {
		return SkillPreview{}, err
	}
	client := skillHTTPSClient
	if client == nil {
		client = http.DefaultClient
	}
	configured := *client
	configured.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return httpsRedirect(ctx, req, via)
	}
	base := configured.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if transport, ok := base.(*http.Transport); ok && transport != nil {
		clone := transport.Clone()
		// Public skill downloads must connect to the validated destination
		// directly. A proxy would receive the hostname and could redirect the
		// request to an address that DialContext never validates.
		clone.Proxy = nil
		// Validate the address returned by DNS again at the point of dialing.
		// The request URL remains the hostname, so TLS SNI and HTTP Host are
		// preserved while the socket connects to the checked IP address.
		clone.DialContext = skillHTTPSDialContext
		if insecure {
			clone.TLSClientConfig = clone.TLSClientConfig.Clone()
			if clone.TLSClientConfig == nil {
				clone.TLSClientConfig = &tls.Config{}
			}
			clone.TLSClientConfig.InsecureSkipVerify = true // explicit caller opt-in
		}
		configured.Transport = clone
	} else if insecure {
		return SkillPreview{}, fmt.Errorf("skill HTTP transport cannot be cloned")
	}
	client = &configured
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return SkillPreview{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return SkillPreview{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SkillPreview{}, fmt.Errorf("skill archive download: HTTP %s", resp.Status)
	}
	root, err := os.MkdirTemp(repository, ".skill-preview-download-")
	if err != nil {
		return SkillPreview{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(root)
		}
	}()
	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxSkillArchiveBytes+1))
	if err != nil {
		return SkillPreview{}, fmt.Errorf("read skill archive: %w", err)
	}
	if int64(len(archive)) > maxSkillArchiveBytes {
		return SkillPreview{}, fmt.Errorf("skill archive response exceeds %d bytes", maxSkillArchiveBytes)
	}
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return SkillPreview{}, fmt.Errorf("read skill archive: %w", err)
	}
	err = extractHTTPSkillTar(reader, root)
	closeErr := reader.Close()
	if err != nil {
		return SkillPreview{}, err
	}
	if closeErr != nil {
		return SkillPreview{}, fmt.Errorf("read skill archive: %w", closeErr)
	}
	preview, err := PrepareSkillPreview(repository, identifier, root)
	if err != nil {
		return SkillPreview{}, err
	}
	cleanup = false
	_ = os.RemoveAll(root)
	return preview, nil
}

func skillHTTPSDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("skill source address %q is invalid: %w", address, err)
	}
	var addrs []net.IP
	if ip := net.ParseIP(host); ip != nil {
		addrs = []net.IP{ip}
	} else {
		resolved, resolveErr := skillHTTPSLookupIP(ctx, host)
		if resolveErr != nil || len(resolved) == 0 {
			return nil, fmt.Errorf("skill source host %q could not be resolved", host)
		}
		for _, addr := range resolved {
			if !isPublicHTTPSIP(addr.IP) {
				return nil, fmt.Errorf("skill source must resolve to a public address")
			}
			addrs = append(addrs, addr.IP)
		}
	}
	var lastErr error
	for _, ip := range addrs {
		if !isPublicHTTPSIP(ip) {
			return nil, fmt.Errorf("skill source must resolve to a public address")
		}
		conn, dialErr := skillHTTPSNetDial(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable address")
	}
	return nil, lastErr
}

func validateSkillHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("skill source must be an HTTPS URL without userinfo or fragment")
	}
	return u, nil
}

func httpsRedirect(ctx context.Context, req *http.Request, via []*http.Request) error {
	if req.URL == nil || !strings.EqualFold(req.URL.Scheme, "https") || req.URL.Hostname() == "" || req.URL.User != nil || req.URL.Fragment != "" {
		return fmt.Errorf("skill archive redirect must remain a safe HTTPS URL")
	}
	return validatePublicHTTPSDestination(ctx, req.URL)
}

func validatePublicHTTPSDestination(ctx context.Context, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("skill source must be an HTTPS URL")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicHTTPSIP(ip) {
			return fmt.Errorf("skill source must resolve to a public address")
		}
		return nil
	}
	addrs, err := skillHTTPSLookupIP(ctx, host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("skill source host %q could not be resolved", host)
	}
	for _, addr := range addrs {
		if !isPublicHTTPSIP(addr.IP) {
			return fmt.Errorf("skill source must resolve to a public address")
		}
	}
	return nil
}

func isPublicHTTPSIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	// IsPrivate intentionally excludes shared address space and benchmarking
	// networks, both of which must remain unreachable from public downloads.
	for _, network := range []string{"100.64.0.0/10", "198.18.0.0/15"} {
		_, block, _ := net.ParseCIDR(network)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

func extractHTTPSkillTar(r io.Reader, root string) error {
	tr := tar.NewReader(io.LimitReader(r, maxSkillArchiveBytes+1))
	var total int64
	files := 0
	seen := make(map[string]bool)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read skill archive: %w", err)
		}
		name := path.Clean(h.Name)
		if h.Name == "" || name == "." || path.IsAbs(h.Name) || name == ".." || strings.HasPrefix(name, "../") || seen[name] {
			return fmt.Errorf("unsafe skill archive path %q", h.Name)
		}
		seen[name] = true
		dest := filepath.Join(root, filepath.FromSlash(name))
		if rel, _ := filepath.Rel(root, dest); rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe skill archive path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if files >= MaxSkillFiles || h.Size < 0 || h.Size > MaxSkillBytes-total {
				return fmt.Errorf("skill archive exceeds limits")
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
				return err
			}
			f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			_, cpErr := io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if cpErr != nil || closeErr != nil {
				if cpErr != nil {
					return cpErr
				}
				return closeErr
			}
			total += h.Size
			files++
		default:
			return fmt.Errorf("skill archive contains unsupported entry %q", h.Name)
		}
	}
}
