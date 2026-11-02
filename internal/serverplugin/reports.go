package serverplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/clientplugin"
)

const maxReportSize = 256 << 10
const ReportFreshness = 24 * time.Hour

var versionPattern = regexp.MustCompile(`^sha256-[a-f0-9]{64}$`)
var attemptPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var reportErrors = map[string]bool{"download_failed": true, "verification_failed": true, "server_changed": true, "local_error": true, "canceled": true, "operation_failed": true}

type UpdateResult struct {
	ID        string `json:"id"`
	Operation string `json:"operation"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code"`
}
type PluginStatus struct {
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	LastUpdate *UpdateResult `json:"last_update,omitempty"`
}
type Report struct {
	APIVersion int            `json:"api_version"`
	Plugins    []PluginStatus `json:"plugins"`
}

func (r Report) Validate() error {
	invalid := errors.New("invalid plugin report")
	if r.APIVersion != clientplugin.APIVersion || r.Plugins == nil || len(r.Plugins) > maxPlugins {
		return invalid
	}
	seen := make(map[string]bool)
	for _, p := range r.Plugins {
		if clientplugin.ValidateName(p.Name) != nil || seen[p.Name] {
			return invalid
		}
		seen[p.Name] = true
		if p.Version != "" && !versionPattern.MatchString(p.Version) {
			return invalid
		}
		u := p.LastUpdate
		if p.Version == "" && (u == nil || u.Operation != "install" || u.Status != "failed") {
			return invalid
		}
		if u != nil {
			if !attemptPattern.MatchString(u.ID) || (u.Operation != "install" && u.Operation != "update") || (u.Status != "succeeded" && u.Status != "failed") {
				return invalid
			}
			if (u.Status == "succeeded" && u.ErrorCode != "") || (u.Status == "failed" && !reportErrors[u.ErrorCode]) {
				return invalid
			}
		}
	}
	return nil
}

type savedStatus struct {
	PluginStatus
	LastUpdateAt string `json:"last_update_at,omitempty"`
}
type savedReport struct {
	ClientID string        `json:"client_id"`
	SeenAt   string        `json:"seen_at"`
	Plugins  []savedStatus `json:"plugins"`
}

func reportPath(clientID string) string {
	digest := sha256.Sum256([]byte(clientID))
	return path.Join("reports", hex.EncodeToString(digest[:])+".json")
}
func readReport(root *os.Root, clientID string) (savedReport, error) {
	var report savedReport
	fileName := reportPath(clientID)
	if _, err := root.Lstat(fileName); os.IsNotExist(err) {
		return report, nil
	}
	f, err := regularOpen(root, fileName)
	if err != nil {
		return report, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReportSize+1))
	if err != nil || len(data) > maxReportSize {
		return report, errStorage
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if dec.Decode(&report) != nil || dec.Decode(new(any)) != io.EOF || report.ClientID != clientID {
		return report, errStorage
	}
	return report, nil
}
func (s *Store) ReceiveReport(ctx context.Context, clientID string, report Report) error {
	if clientID == "" || len(clientID) > 512 || report.Validate() != nil {
		return errors.New("invalid plugin report")
	}
	root, done, err := s.locked(ctx, true)
	if err != nil {
		return err
	}
	defer done()
	if mkdir(root, "reports") != nil {
		return errStorage
	}
	previous, err := readReport(root, clientID)
	if err != nil {
		return err
	}
	old := make(map[string]savedStatus)
	for _, p := range previous.Plugins {
		old[p.Name] = p
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	saved := savedReport{ClientID: clientID, SeenAt: now, Plugins: make([]savedStatus, 0, len(report.Plugins))}
	for _, p := range report.Plugins {
		status := savedStatus{PluginStatus: p}
		if p.LastUpdate != nil {
			prior := old[p.Name]
			if prior.LastUpdate != nil && prior.LastUpdate.ID == p.LastUpdate.ID {
				if !reflect.DeepEqual(prior.LastUpdate, p.LastUpdate) {
					return errors.New("plugin report attempt changed")
				}
				status.LastUpdateAt = prior.LastUpdateAt
			} else {
				status.LastUpdateAt = now
			}
		}
		saved.Plugins = append(saved.Plugins, status)
	}
	data, err := json.Marshal(saved)
	if err != nil || len(data) > maxReportSize {
		return errors.New("plugin report too large")
	}
	tmp := path.Join("reports", ".report-"+randomName())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errStorage
	}
	defer root.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errStorage
	}
	if root.Rename(tmp, reportPath(clientID)) != nil || syncDirectory(root, "reports") != nil || syncDirectory(root, ".") != nil {
		return errStorage
	}
	return nil
}

type ClientIdentity struct{ ID, Name string }
type ClientState struct {
	ClientID         string        `json:"client_id"`
	ClientName       string        `json:"client_name"`
	SeenAt           string        `json:"seen_at"`
	LastUpdateAt     string        `json:"last_update_at"`
	InstalledVersion string        `json:"installed_version"`
	PublishedVersion string        `json:"published_version"`
	Status           string        `json:"status"`
	LastUpdate       *UpdateResult `json:"last_update,omitempty"`
}

func (s *Store) ClientStates(ctx context.Context, name string, clients []ClientIdentity, now time.Time) ([]ClientState, error) {
	if clientplugin.ValidateName(name) != nil {
		return nil, ErrNotFound
	}
	root, done, err := s.locked(ctx, false)
	if err != nil {
		return nil, err
	}
	defer done()
	c, err := readCatalog(root)
	if err != nil {
		return nil, err
	}
	e, ok := c.Plugins[name]
	if !ok {
		return nil, ErrNotFound
	}
	states := make([]ClientState, 0, len(clients))
	for _, client := range clients {
		row := ClientState{ClientID: client.ID, ClientName: client.Name, PublishedVersion: e.Current, Status: "unknown"}
		report, err := readReport(root, client.ID)
		if err != nil {
			return nil, err
		}
		if report.ClientID != "" {
			row.SeenAt = report.SeenAt
			for _, p := range report.Plugins {
				if p.Name == name {
					row.InstalledVersion = p.Version
					row.LastUpdate = p.LastUpdate
					row.LastUpdateAt = p.LastUpdateAt
					break
				}
			}
			seen, err := time.Parse(time.RFC3339Nano, row.SeenAt)
			switch {
			case err != nil || seen.After(now.Add(time.Minute)) || now.Sub(seen) > ReportFreshness:
				row.Status = "stale"
			case row.LastUpdate != nil && row.LastUpdate.Status == "failed":
				row.Status = "update_failed"
			case row.InstalledVersion == "":
				row.Status = "not_installed"
			case e.Current == "":
				row.Status = "unpublished"
			case row.InstalledVersion == e.Current:
				row.Status = "up_to_date"
			default:
				row.Status = "different"
			}
		}
		states = append(states, row)
	}
	return states, nil
}
