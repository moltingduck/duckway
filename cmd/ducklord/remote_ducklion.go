package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	clientapi "github.com/hackerduck/duckway/internal/client"
	"github.com/hackerduck/duckway/internal/ducklord"
)

const maxRemoteDucklionSize = 128 << 20

// ducklionServerURL resolves the server managed for this Ducklord executable.
// DUCKWAY_SERVER_URL is an explicit override for unmanaged installs.
func ducklionServerURL() (string, error) {
	if server := strings.TrimSpace(os.Getenv("DUCKWAY_SERVER_URL")); server != "" {
		return server, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if os.Getenv("SUDO_USER") != "" && os.Geteuid() == 0 {
		if u, e := user.Lookup(os.Getenv("SUDO_USER")); e == nil {
			home = u.HomeDir
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(updateMetadataPath(home, exe))
	if err != nil {
		return "", errors.New("duckway server URL unavailable; set DUCKWAY_SERVER_URL or install Ducklord through Duckway")
	}
	var metadata struct {
		Managed bool   `json:"managed"`
		Path    string `json:"executable_path"`
		Server  string `json:"server_url"`
	}
	if json.Unmarshal(b, &metadata) != nil || !metadata.Managed || metadata.Path != filepath.Clean(exe) || strings.TrimSpace(metadata.Server) == "" {
		return "", errors.New("ducklord update metadata is invalid; set DUCKWAY_SERVER_URL")
	}
	return metadata.Server, nil
}

func fetchRemoteDucklion(ctx context.Context, server, osName, arch string, client *http.Client) ([]byte, error) {
	if (osName != "linux" && osName != "darwin") || (arch != "amd64" && arch != "arm64") {
		return nil, fmt.Errorf("unsupported remote platform %s/%s", osName, arch)
	}
	base, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil || !validDucklordUpdateBaseURL(base) || !secureDucklionServer(base) {
		return nil, errors.New("invalid Duckway server URL")
	}
	manifestURL := *base
	manifestURL.Path += "/client/update-info"
	q := manifestURL.Query()
	q.Set("os", osName)
	q.Set("arch", arch)
	q.Set("version", "")
	manifestURL.RawQuery = q.Encode()
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !sameOrigin(base, req.URL) {
				return errors.New("redirect rejected")
			}
			return nil
		}}
	} else {
		// Preserve injected transport and timeout while enforcing the same-origin
		// boundary for callers that provide a client without redirect policy.
		copy := *client
		prior := client.CheckRedirect
		copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !sameOrigin(base, req.URL) {
				return errors.New("redirect rejected")
			}
			if prior != nil {
				return prior(req, via)
			}
			return nil
		}
		client = &copy
	}
	get := func(u string) (*http.Response, error) {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if e != nil {
			return nil, e
		}
		return client.Do(req)
	}
	resp, err := get(manifestURL.String())
	if err != nil {
		return nil, fmt.Errorf("fetch Ducklion manifest: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("duckway server returned %d for Ducklion manifest", resp.StatusCode)
	}
	var m clientapi.UpdateInfo
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m)
	resp.Body.Close()
	want := "ducklion-" + osName + "-" + arch
	if decodeErr != nil || m.OS != osName || m.Arch != arch || m.DucklionBinary != want || m.DucklionDownloadURL != "/download/"+want || m.DucklionSize < 1<<20 || m.DucklionSize > maxRemoteDucklionSize || len(m.DucklionSHA256) != 64 {
		return nil, errors.New("duckway server returned an invalid Ducklion manifest")
	}
	if _, err := hex.DecodeString(m.DucklionSHA256); err != nil {
		return nil, errors.New("duckway server returned an invalid Ducklion SHA-256")
	}
	downloadURL := *base
	downloadURL.Path += m.DucklionDownloadURL
	artifact, err := get(downloadURL.String())
	if err != nil {
		return nil, fmt.Errorf("download Ducklion: %w", err)
	}
	defer artifact.Body.Close()
	if artifact.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckway artifact download returned %d", artifact.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(artifact.Body, m.DucklionSize+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != m.DucklionSize || hex.EncodeToString(sum[:]) != strings.ToLower(m.DucklionSHA256) {
		return nil, errors.New("downloaded Ducklion failed size or SHA-256 verification")
	}
	return data, nil
}

func secureDucklionServer(base *url.URL) bool {
	if base == nil {
		return false
	}
	if base.Scheme == "https" {
		return true
	}
	if base.Scheme != "http" {
		return false
	}
	host := strings.ToLower(base.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

type remoteDucklionProvider interface {
	DownloadDucklion(context.Context, string, string) (remoteDucklionArtifact, error)
}

// remoteDucklionArtifact is the internal downloader contract used by test
// runners. Every field is revalidated before its bytes can be installed.
type remoteDucklionArtifact struct {
	OS, Arch, Binary string
	Size             int64
	SHA256           string
	Bytes            []byte
}

func (a remoteDucklionArtifact) verifiedBytes(osName, arch string) ([]byte, error) {
	want := "ducklion-" + osName + "-" + arch
	if a.OS != osName || a.Arch != arch || a.Binary != want || a.Size < 1<<20 || a.Size > maxRemoteDucklionSize || int64(len(a.Bytes)) != a.Size || len(a.SHA256) != 64 {
		return nil, errors.New("duckway server returned an invalid Ducklion artifact")
	}
	if _, err := hex.DecodeString(a.SHA256); err != nil {
		return nil, errors.New("duckway server returned an invalid Ducklion SHA-256")
	}
	sum := sha256.Sum256(a.Bytes)
	if hex.EncodeToString(sum[:]) != strings.ToLower(a.SHA256) {
		return nil, errors.New("downloaded Ducklion failed size or SHA-256 verification")
	}
	return a.Bytes, nil
}

func downloadDucklion(ctx context.Context, runner remoteRunner, osName, arch string) ([]byte, error) {
	if provider, ok := runner.(remoteDucklionProvider); ok {
		artifact, err := provider.DownloadDucklion(ctx, osName, arch)
		if err != nil {
			return nil, err
		}
		return artifact.verifiedBytes(osName, arch)
	}
	server, err := ducklionServerURL()
	if err != nil {
		return nil, err
	}
	return fetchRemoteDucklion(ctx, server, osName, arch, nil)
}

type remoteDucklionInstaller interface {
	InstallDucklionBytes(context.Context, ducklord.Client, []byte, string) (string, error)
}
