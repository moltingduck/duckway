package ducklord

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ApproverCredentialsFile is the file name, next to the Ducklord config, that
// holds the Duckway server URL and the approver token ("dwa_...").
//
// The token authorizes approving held requests, so it is a local-operator
// secret. It lives only in this file: it is never part of config.yaml, never
// exported through the environment, and never sent to Ducklion hosts, agent
// containers or session environments. TestApproverTokenIsNeverPropagated
// guards that.
const ApproverCredentialsFile = "approver.json"

// ErrApproverNotLoggedIn means no approver credentials are stored.
var ErrApproverNotLoggedIn = errors.New("not logged in as an approver; run `ducklord approver login`")

// ApproverCredentials is the stored approver login.
type ApproverCredentials struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// String never includes the token.
func (c ApproverCredentials) String() string { return "approver@" + c.Server }

// GoString never includes the token.
func (c ApproverCredentials) GoString() string { return c.String() }

// ApproverCredentialsPath returns the credentials file beside the active
// Ducklord config file.
func ApproverCredentialsPath() string {
	return filepath.Join(filepath.Dir(DefaultConfigPath()), ApproverCredentialsFile)
}

// NormalizeApproverServer validates and canonicalizes a server URL.
func NormalizeApproverServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("server must be an http(s) URL such as https://duckway.example.com")
	}
	if u.User != nil {
		return "", fmt.Errorf("server URL must not contain credentials")
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

// LoadApproverCredentials reads the credentials. It refuses a file that other
// users can read, so a copied or mis-chmodded file is noticed.
func LoadApproverCredentials(path string) (*ApproverCredentials, error) {
	if path == "" {
		path = ApproverCredentialsPath()
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrApproverNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("read approver credentials: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read approver credentials: %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("approver credentials %s are readable by others (mode %o); run `chmod 600` or log in again", path, info.Mode().Perm())
	}
	if info.Size() > 64<<10 {
		return nil, fmt.Errorf("read approver credentials: file too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read approver credentials: %w", err)
	}
	var creds ApproverCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("read approver credentials: %w", err)
	}
	if creds.Token == "" || creds.Server == "" {
		return nil, ErrApproverNotLoggedIn
	}
	return &creds, nil
}

// SaveApproverCredentials atomically writes the credentials with mode 0600.
func SaveApproverCredentials(path string, creds ApproverCredentials) error {
	if path == "" {
		path = ApproverCredentialsPath()
	}
	server, err := NormalizeApproverServer(creds.Server)
	if err != nil {
		return err
	}
	creds.Server = server
	if !strings.HasPrefix(creds.Token, "dwa_") {
		return fmt.Errorf("approver tokens start with dwa_")
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+ApproverCredentialsFile+"."+hex.EncodeToString(suffix[:])+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(data, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}

// DeleteApproverCredentials removes the stored login; a missing file is not an error.
func DeleteApproverCredentials(path string) error {
	if path == "" {
		path = ApproverCredentialsPath()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
