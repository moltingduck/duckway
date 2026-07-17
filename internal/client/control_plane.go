package client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/controlplane"
	"github.com/hackerduck/duckway/internal/version"
)

const (
	controlLeaderRetry = 30 * time.Second
	controlMinInterval = 15 * time.Second
	controlMaxInterval = 15 * time.Minute
)

type managedControlState struct {
	Command controlplane.Command `json:"command"`
	Status  string               `json:"status"`
	Error   string               `json:"error,omitempty"`
}

type controlStateStore struct {
	mu   sync.Mutex
	path string
}

var startManagedUpdateProcess = startManagedUpdateProcessDefault

func StartControlPlaneLoop(ctx context.Context, configDir string, cfg *Config, component string) {
	if cfg == nil || cfg.ServerURL == "" || cfg.Token == "" {
		return
	}
	go func() {
		for ctx.Err() == nil {
			lock, acquired, err := tryControlLeaderLock(configDir)
			if err != nil {
				log.Printf("[%s] control leader lock: %v", component, err)
			} else if acquired {
				runControlPlaneLeader(ctx, configDir, cfg, component)
				releaseControlLeaderLock(lock)
				return
			}
			if !sleepContext(ctx, controlLeaderRetry) {
				return
			}
		}
	}()
}

func tryControlLeaderLock(configDir string) (*os.File, bool, error) {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(configDir, "control.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

func releaseControlLeaderLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func runControlPlaneLeader(ctx context.Context, configDir string, cfg *Config, component string) {
	trustedURL := trustedManagedUpdateURL(cfg.ServerURL)
	if !trustedURL {
		log.Printf("[%s] managed update execution disabled: server URL must use HTTPS (or set DUCKWAY_ALLOW_INSECURE_MANAGED_UPDATES=1)", component)
	}
	store := &controlStateStore{path: filepath.Join(configDir, "control-state.json")}
	bootID := uuid.NewString()
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	interval := time.Second
	for {
		if !sleepContext(ctx, interval) {
			return
		}
		next, err := controlHeartbeatOnce(ctx, configDir, cfg, component, bootID, api, store, trustedURL)
		if err != nil {
			log.Printf("[%s] control heartbeat: %v", component, err)
			next = 60 * time.Second
		}
		interval = clampControlInterval(next)
	}
}

func controlHeartbeatOnce(ctx context.Context, configDir string, cfg *Config, component, bootID string, api *APIClient, store *controlStateStore, trustedURL bool) (time.Duration, error) {
	state, err := store.load()
	if err != nil {
		return 0, fmt.Errorf("load state: %w", err)
	}
	if state != nil && state.Status == controlplane.JobRunning && version.Get() == state.Command.TargetVersion {
		state.Status = controlplane.JobHealthy
		if err := store.save(state); err != nil {
			return 0, err
		}
	}
	exe, _ := os.Executable()
	heartbeat := controlplane.HeartbeatRequest{
		ProtocolVersion: controlplane.ProtocolVersion,
		Version:         version.Get(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		BootID: bootID, InstallPath: exe, InstallWritable: executableDirWritable(exe),
		Components: runningComponents(configDir, component),
	}
	if trustedURL {
		heartbeat.Capabilities = []string{controlplane.CapabilityManagedUpdate}
	}
	if state != nil {
		heartbeat.CurrentJob = &controlplane.CurrentJob{
			ID: state.Command.ID, LeaseToken: state.Command.LeaseToken,
			Status: state.Status, RunningVersion: version.Get(), Error: state.Error,
		}
	}
	heartbeatCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	response, err := api.ControlHeartbeat(heartbeatCtx, &heartbeat)
	cancel()
	if err != nil {
		return 0, err
	}
	if state != nil && (state.Status == controlplane.JobHealthy || state.Status == controlplane.JobFailed) {
		if response.Command == nil || response.Command.ID != state.Command.ID {
			if err := store.clear(); err != nil {
				return 0, err
			}
			state = nil
		}
	}
	if response.Command != nil {
		if err := validateControlCommand(*response.Command); err != nil {
			return 0, err
		}
		if state == nil {
			state = &managedControlState{Command: *response.Command, Status: controlplane.JobRunning}
			if err := store.save(state); err != nil {
				return 0, err
			}
			statusCtx, statusCancel := context.WithTimeout(ctx, 10*time.Second)
			err := api.ReportControlJob(statusCtx, state.Command.ID, &controlplane.JobStatusRequest{
				LeaseToken: state.Command.LeaseToken, Status: controlplane.JobRunning, RunningVersion: version.Get(),
			})
			statusCancel()
			if err != nil {
				state.Status, state.Error = controlplane.JobFailed, "server did not accept job: "+err.Error()
				_ = store.save(state)
				return 0, err
			}
			if err := startManagedUpdateProcess(configDir, cfg.ServerURL, state.Command, store); err != nil {
				state.Status, state.Error = controlplane.JobFailed, err.Error()
				_ = store.save(state)
				return 0, err
			}
		} else if state.Command.ID != response.Command.ID || state.Command.Attempt != response.Command.Attempt {
			return 0, fmt.Errorf("received a different job while %s is active", state.Command.ID)
		}
	}
	return time.Duration(response.NextHeartbeatSeconds) * time.Second, nil
}

func validateControlCommand(command controlplane.Command) error {
	if command.Type != controlplane.CommandUpdateRestart || command.ID == "" || command.LeaseToken == "" || command.TargetVersion == "" {
		return fmt.Errorf("invalid managed update command")
	}
	wantBinary := fmt.Sprintf("duckway-client-%s-%s", runtime.GOOS, runtime.GOARCH)
	if command.Binary != wantBinary || len(command.SHA256) != 64 || command.Size < 1024*1024 || command.Attempt < 1 {
		return fmt.Errorf("invalid managed update artifact")
	}
	if _, err := hex.DecodeString(command.SHA256); err != nil {
		return fmt.Errorf("invalid managed update sha256")
	}
	return nil
}

func startManagedUpdateProcessDefault(configDir, serverURL string, command controlplane.Command, store *controlStateStore) error {
	if !trustedManagedUpdateURL(serverURL) {
		return fmt.Errorf("managed update requires HTTPS")
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	logPath := filepath.Join(configDir, "control-update.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		_ = logFile.Close()
		return err
	}
	cmd := exec.Command(exe, "update", "--server", serverURL, "--restart",
		"--expected-version", command.TargetVersion, "--expected-binary", command.Binary,
		"--expected-sha256", command.SHA256, "--expected-size", strconv.FormatInt(command.Size, 10))
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = append(os.Environ(), "DUCKWAY_CONFIG_DIR="+configDir, "DUCKWAY_MANAGED_UPDATE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		if err != nil {
			state, loadErr := store.load()
			if loadErr == nil && state != nil && state.Command.ID == command.ID {
				state.Status, state.Error = controlplane.JobFailed, "managed updater failed: "+err.Error()
				_ = store.save(state)
			}
		}
	}()
	return nil
}

func trustedManagedUpdateURL(raw string) bool {
	if os.Getenv("DUCKWAY_ALLOW_INSECURE_MANAGED_UPDATES") == "1" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

func executableDirWritable(exe string) bool {
	if exe == "" {
		return false
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".duckway-write-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func runningComponents(configDir, current string) map[string]string {
	components := map[string]string{current: "running"}
	for name, file := range map[string]string{"proxy": "proxy.pid", "cc_watch": "cc-watch.pid"} {
		data, err := os.ReadFile(filepath.Join(configDir, file))
		if err == nil && strings.TrimSpace(string(data)) != "" {
			components[name] = "running"
		}
	}
	return components
}

func clampControlInterval(interval time.Duration) time.Duration {
	if interval < controlMinInterval {
		return controlMinInterval
	}
	if interval > controlMaxInterval {
		return controlMaxInterval
	}
	return interval
}

func (s *controlStateStore) load() (*managedControlState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state managedControlState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("control state is corrupt; manual recovery required: %w", err)
	}
	return &state, nil
}

func (s *controlStateStore) save(state *managedControlState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".control-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}

func (s *controlStateStore) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
