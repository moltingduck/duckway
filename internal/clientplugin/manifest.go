// Package clientplugin installs and runs explicit, out-of-process client plugins.
package clientplugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
)

const APIVersion = 1
const MaxArtifactSize int64 = 512 << 20
const maxManifestSize = 1 << 20

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var platformPattern = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// reservedNames includes public Duckway entry points; the name grammar also
// rejects flag aliases and internal names beginning with underscores.
var reservedNames = map[string]bool{
	"init": true, "sync": true, "env": true, "proxy": true, "status": true,
	"doctor": true, "config": true, "logs": true, "install-ca": true, "update": true,
	"mcp": true, "cc": true, "session": true, "ducklion": true, "projects": true,
	"git": true, "start": true, "stop": true, "restart": true, "integrate": true,
	"version": true, "help": true, "plugin": true,
	"completion": true,
}

type Manifest struct {
	APIVersion  int        `json:"api_version"`
	Name        string     `json:"name"`
	Version     string     `json:"version"`
	Description string     `json:"description,omitempty"`
	Artifacts   []Artifact `json:"artifacts"`
}

type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func ValidateName(name string) error {
	if !namePattern.MatchString(name) || reservedNames[name] {
		return fmt.Errorf("invalid or reserved plugin name")
	}
	return nil
}

// SourceURL permits only an explicit HTTP(S) origin and absolute path. Query
// parameters and credentials must never enter the durable registry or errors.
func SourceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "\r\n\\#?") || u.RawPath != "" {
		return nil, fmt.Errorf("manifest URL must be HTTP(S), without credentials, query or fragment")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if !strings.HasPrefix(u.Path, "/") || path.Clean(u.Path) != u.Path {
		return nil, fmt.Errorf("manifest URL path must be canonical")
	}
	return u, nil
}

func DecodeManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	data, err := io.ReadAll(io.LimitReader(r, maxManifestSize+1))
	if err != nil || len(data) > maxManifestSize {
		return m, fmt.Errorf("manifest read failed or exceeds size limit")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("invalid plugin manifest JSON")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return m, fmt.Errorf("manifest must contain one JSON object")
	}
	return m, m.Validate()
}

func (m Manifest) Validate() error {
	if m.APIVersion != APIVersion {
		return fmt.Errorf("unsupported plugin API version")
	}
	if err := ValidateName(m.Name); err != nil {
		return err
	}
	if !strings.HasPrefix(m.Version, "sha256-") || !digestPattern.MatchString(strings.TrimPrefix(m.Version, "sha256-")) {
		return fmt.Errorf("invalid plugin version")
	}
	if len(m.Description) > 4096 || strings.ContainsAny(m.Description, "\x00\r\n\x1b") {
		return fmt.Errorf("invalid plugin description")
	}
	if len(m.Artifacts) == 0 || len(m.Artifacts) > 128 {
		return fmt.Errorf("manifest requires 1..128 artifacts")
	}
	seen := make(map[string]bool)
	for _, a := range m.Artifacts {
		platform := a.OS + "/" + a.Arch
		if !platformPattern.MatchString(a.OS) || !platformPattern.MatchString(a.Arch) || seen[platform] {
			return fmt.Errorf("invalid or duplicate artifact platform")
		}
		seen[platform] = true
		if !digestPattern.MatchString(a.SHA256) || a.Size <= 0 || a.Size > MaxArtifactSize {
			return fmt.Errorf("invalid artifact digest or size")
		}
		if _, err := artifactURL(&url.URL{Scheme: "https", Host: "example.invalid"}, a.URL); err != nil {
			return err
		}
	}
	return nil
}

func artifactURL(source *url.URL, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n\x00?#") || path.Clean(u.Path) != u.Path || u.Path == "/" {
		return nil, fmt.Errorf("artifact URL must be a canonical same-origin absolute path")
	}
	return source.ResolveReference(u), nil
}
