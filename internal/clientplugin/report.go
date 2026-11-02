package clientplugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxReportPlugins = 256
const maxReportStateSize = 1 << 20

type Report struct {
	APIVersion int            `json:"api_version"`
	Plugins    []PluginStatus `json:"plugins"`
}

type PluginStatus struct {
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	LastUpdate *UpdateResult `json:"last_update,omitempty"`
}

// UpdateResult has a stable random identifier for idempotent replay. All times
// belong to the receiving server; client clocks never claim operation timestamps.
type UpdateResult struct {
	ID        string `json:"id"`
	Operation string `json:"operation"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}

type reportRecord struct {
	ServerOrigin string       `json:"server_origin"`
	LastUpdate   UpdateResult `json:"last_update"`
}

type reportState struct {
	Plugins map[string]reportRecord `json:"plugins"`
}

func (r UpdateResult) validate() error {
	id, err := hex.DecodeString(r.ID)
	if err != nil || len(id) != 16 || strings.ToLower(r.ID) != r.ID || (r.Operation != "install" && r.Operation != "update") {
		return fmt.Errorf("invalid plugin update result")
	}
	if r.Status == "succeeded" && r.ErrorCode == "" {
		return nil
	}
	if r.Status != "failed" {
		return fmt.Errorf("invalid plugin update status")
	}
	switch r.ErrorCode {
	case "download_failed", "verification_failed", "server_changed", "local_error", "canceled", "operation_failed":
		return nil
	}
	return fmt.Errorf("invalid plugin update error classification")
}

func (r Report) Validate() error {
	if r.APIVersion != APIVersion || r.Plugins == nil || len(r.Plugins) > maxReportPlugins {
		return fmt.Errorf("invalid plugin report")
	}
	seen := make(map[string]bool)
	for _, p := range r.Plugins {
		if err := ValidateName(p.Name); err != nil {
			return err
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate plugin report name")
		}
		seen[p.Name] = true
		if p.LastUpdate != nil {
			if err := p.LastUpdate.validate(); err != nil {
				return err
			}
		}
		if p.Version == "" {
			if p.LastUpdate == nil || p.LastUpdate.Operation != "install" || p.LastUpdate.Status != "failed" {
				return fmt.Errorf("empty version requires failed first install")
			}
		} else if !strings.HasPrefix(p.Version, "sha256-") || !digestPattern.MatchString(strings.TrimPrefix(p.Version, "sha256-")) {
			return fmt.Errorf("invalid reported plugin version")
		}
	}
	return nil
}

func classifyOperationError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, ErrServerChanged):
		return "server_changed"
	case errors.Is(err, ErrVerificationFailed):
		return "verification_failed"
	case errors.Is(err, ErrDownloadFailed):
		return "download_failed"
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return "local_error"
	}
	return "operation_failed"
}

func (h Host) readReportState() (reportState, error) {
	state := reportState{Plugins: make(map[string]reportRecord)}
	path := filepath.Join(h.root(), ".report-state.json")
	if err := regularFile(path); os.IsNotExist(err) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	f, err := os.Open(path)
	if err != nil {
		return state, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReportStateSize+1))
	if err != nil || len(data) > maxReportStateSize {
		return state, fmt.Errorf("plugin report state read failed or exceeds size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return state, fmt.Errorf("invalid local plugin report state")
	}
	if err := dec.Decode(new(any)); err != io.EOF || state.Plugins == nil || len(state.Plugins) > maxReportPlugins {
		return state, fmt.Errorf("invalid local plugin report state")
	}
	for name, record := range state.Plugins {
		if err := ValidateName(name); err != nil {
			return state, err
		}
		origin, err := ServerOrigin(record.ServerOrigin)
		if err != nil || origin != record.ServerOrigin {
			return state, fmt.Errorf("invalid plugin report origin")
		}
		if err := record.LastUpdate.validate(); err != nil {
			return state, err
		}
	}
	return state, nil
}

// RecordOperation persists the latest manual command outcome separately from
// activation. Failure here cannot roll back or change an installed executable.
func (h Host) RecordOperation(ctx context.Context, name, operation string, operationErr error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if operation != "install" && operation != "update" {
		return fmt.Errorf("invalid plugin operation")
	}
	origin, err := h.remoteOrigin()
	if err != nil {
		return err
	}
	lock, err := h.lock(ctx, true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	installed, err := h.read(name)
	if err != nil && !errors.Is(err, ErrNotInstalled) {
		return err
	}
	if err == nil && installed.Source != SourceManaged {
		return ErrLocalInstall
	}
	if err == nil && installed.ServerOrigin != origin {
		return ErrServerChanged
	}
	state, err := h.readReportState()
	if err != nil {
		return err
	}
	if _, exists := state.Plugins[name]; !exists && len(state.Plugins) >= maxReportPlugins {
		return fmt.Errorf("plugin result count exceeds limit")
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	result := UpdateResult{ID: hex.EncodeToString(id), Operation: operation, Status: "succeeded"}
	if operationErr != nil {
		result.Status = "failed"
		result.ErrorCode = classifyOperationError(operationErr)
	}
	state.Plugins[name] = reportRecord{ServerOrigin: origin, LastUpdate: result}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicWrite(h.root(), ".report-state.json", data)
}

func (h Host) snapshot(ctx context.Context) (Report, bool, error) {
	report := Report{APIVersion: APIVersion, Plugins: make([]PluginStatus, 0)}
	origin, err := h.remoteOrigin()
	if err != nil {
		return report, false, err
	}
	lock, err := h.lock(ctx, false)
	if err != nil {
		return report, false, err
	}
	defer unlock(lock)
	state, err := h.readReportState()
	if err != nil {
		return report, false, err
	}
	entries, err := os.ReadDir(h.root())
	if err != nil {
		return report, false, err
	}
	seen := make(map[string]bool)
	localNames := make(map[string]bool)
	managedHistory := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		p, err := h.read(entry.Name())
		if errors.Is(err, ErrNotInstalled) {
			continue
		}
		if err != nil {
			return report, false, err
		}
		if p.Source == SourceLocal {
			localNames[p.Manifest.Name] = true
			continue
		}
		if p.Source != SourceManaged || p.ServerOrigin != origin {
			continue
		}
		executable := filepath.Join(h.root(), p.Manifest.Name, "releases", p.Release, "executable")
		if err := regularFile(executable); err != nil {
			return report, false, err
		}
		info, err := os.Stat(executable)
		if err != nil {
			return report, false, err
		}
		if info.Size() != p.Artifact.Size {
			return report, false, fmt.Errorf("installed plugin size differs from activation record")
		}
		status := PluginStatus{Name: p.Manifest.Name, Version: p.Manifest.Version}
		if record, ok := state.Plugins[status.Name]; ok && record.ServerOrigin == origin {
			result := record.LastUpdate
			status.LastUpdate = &result
		}
		report.Plugins = append(report.Plugins, status)
		seen[status.Name] = true
	}
	for name, record := range state.Plugins {
		if record.ServerOrigin == origin && !localNames[name] {
			managedHistory = true
		}
		if seen[name] || localNames[name] || record.ServerOrigin != origin || record.LastUpdate.Operation != "install" || record.LastUpdate.Status != "failed" {
			continue
		}
		result := record.LastUpdate
		report.Plugins = append(report.Plugins, PluginStatus{Name: name, LastUpdate: &result})
	}
	sort.Slice(report.Plugins, func(i, j int) bool { return report.Plugins[i].Name < report.Plugins[j].Name })
	return report, managedHistory, report.Validate()
}

// Report sends a full snapshot. A separate send lock orders CLI and periodic
// reporters; the registry lock is released before network I/O, so reporting never
// holds executable activation or plugin runtime hostage to server availability.
func (h Host) Report(ctx context.Context) error {
	origin, err := h.remoteOrigin()
	if err != nil {
		return err
	}
	reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sendLock, err := h.lockNamed(reportCtx, ".report-send.lock", true)
	if err != nil {
		return err
	}
	defer unlock(sendLock)
	report, managedHistory, err := h.snapshot(reportCtx)
	if err != nil {
		return err
	}
	if len(report.Plugins) == 0 && !managedHistory {
		// Local-only installs and empty registries have no server-managed state
		// to report. Avoid probing a server that may predate the plugin API. If
		// managed history exists, an empty snapshot still clears stale inventory.
		return nil
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(reportCtx, http.MethodPost, origin+"/client/plugins/report", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create plugin report request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Duckway-Token", h.Token)
	client := h.managedClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("plugin status was not reported to server")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("plugin status was not reported to server: HTTP %d", resp.StatusCode)
	}
	return nil
}
