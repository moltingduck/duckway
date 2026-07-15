package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/hackerduck/duckway/internal/version"
)

const (
	updateCheckInterval = 6 * time.Hour
	updateCheckJitter   = 30 * time.Minute
)

func StartUpdateCheckLoop(ctx context.Context, cfg *Config, component string) {
	if cfg == nil || cfg.ServerURL == "" {
		return
	}
	go updateCheckLoop(ctx, cfg, component, updateCheckInterval, updateCheckJitter, log.Printf)
}

func updateCheckLoop(ctx context.Context, cfg *Config, component string, interval, jitterWindow time.Duration, logger func(string, ...interface{})) {
	if interval <= 0 {
		return
	}
	delay := deterministicJitter(jitterSeed(cfg, component, version.Get()), jitterWindow)
	if !sleepContext(ctx, delay) {
		return
	}
	for {
		checkAndLogUpdate(ctx, DefaultConfigDir(), cfg, component, logger)
		next := interval + deterministicJitter(jitterSeed(cfg, component, time.Now().UTC().Format("2006-01-02T15")), jitterWindow)
		if !sleepContext(ctx, next) {
			return
		}
	}
}

func checkAndLogUpdate(ctx context.Context, configDir string, cfg *Config, component string, logger func(string, ...interface{})) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	if cfg == nil {
		return
	}
	info, err := CheckUpdateInfo(cfg.ServerURL, version.Get())
	if err != nil {
		logger("[%s] update check failed: %v", component, err)
		return
	}
	switch {
	case info.UpdateRequired:
		if info.Reason != "" {
			logger("[%s] duckway client update REQUIRED: current=%s target=%s reason=%s; run `duckway update && duckway restart`", component, version.Get(), info.ClientRecommendedVersion, info.Reason)
		} else {
			logger("[%s] duckway client update REQUIRED: current=%s target=%s; run `duckway update && duckway restart`", component, version.Get(), info.ClientRecommendedVersion)
		}
		notifyCCUpdateIfNeeded(ctx, configDir, cfg, component, info, logger)
	case info.UpdateRecommended:
		if info.Reason != "" {
			logger("[%s] duckway client update available: current=%s target=%s reason=%s; run `duckway update && duckway restart`", component, version.Get(), info.ClientRecommendedVersion, info.Reason)
		} else {
			logger("[%s] duckway client update available: current=%s target=%s; run `duckway update && duckway restart`", component, version.Get(), info.ClientRecommendedVersion)
		}
		notifyCCUpdateIfNeeded(ctx, configDir, cfg, component, info, logger)
	}
}

func notifyCCUpdateIfNeeded(ctx context.Context, configDir string, cfg *Config, component string, info *UpdateInfo, logger func(string, ...interface{})) {
	if cfg == nil || cfg.ServerURL == "" || cfg.Token == "" || info == nil {
		return
	}
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	ccCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	assignments, err := api.FetchCCContext(ccCtx)
	if err != nil {
		logger("[%s] update cc notification skipped: fetch cc: %v", component, err)
		return
	}
	if len(assignments) == 0 || assignments[0].ManagementHandle == "" {
		return
	}

	key := updateNotificationKey(version.Get(), info)
	claimed, done, err := claimUpdateNotification(configDir, key)
	if err != nil {
		logger("[%s] update cc notification skipped: claim: %v", component, err)
		return
	}
	if !claimed {
		return
	}

	msg := formatCCUpdateNotification(cfg, component, info)
	err = api.PostCC(ccCtx, assignments[0].ManagementHandle, msg)
	done(err == nil)
	if err != nil {
		logger("[%s] update cc notification failed: %v", component, err)
		return
	}
	logger("[%s] update notification posted to control channel %s", component, assignments[0].ManagementHandle)
}

func updateNotificationKey(current string, info *UpdateInfo) string {
	if info == nil {
		return current
	}
	return fmt.Sprintf("%s|%s|required=%t|recommended=%t|reason=%s", current, info.ClientRecommendedVersion, info.UpdateRequired, info.UpdateRecommended, info.Reason)
}

func claimUpdateNotification(configDir, key string) (bool, func(success bool), error) {
	if configDir == "" {
		configDir = DefaultConfigDir()
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return false, nil, err
	}
	sum := sha256.Sum256([]byte(key))
	path := filepath.Join(configDir, "update-notified-"+hex.EncodeToString(sum[:8])+".claim")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, func(bool) {}, nil
		}
		return false, nil, err
	}
	_, _ = f.WriteString(key + "\n")
	_ = f.Close()
	return true, func(success bool) {
		if !success {
			_ = os.Remove(path)
		}
	}, nil
}

func formatCCUpdateNotification(cfg *Config, component string, info *UpdateInfo) string {
	status := "Duckway client update available"
	if info.UpdateRequired {
		status = "Duckway client update REQUIRED"
	}
	msg := fmt.Sprintf("⚠️ %s\n\nCurrent: %s\nTarget: %s\nDetected by: %s\n\nFrom this management channel, run:\n`!duckway-update --restart`\n\nFrom a shell, run:\n`duckway update --server %s && duckway restart`",
		status, version.Get(), info.ClientRecommendedVersion, component, cfg.ServerURL)
	if info.Reason != "" {
		msg += "\n\nReason: " + info.Reason
	}
	msg += "\n\nIf Duckway is installed in a root-owned path such as `/usr/local/bin`, run the update command with `sudo`, then restart Duckway."
	return msg
}

func jitterSeed(cfg *Config, component, bucket string) string {
	return cfg.ServerURL + "|" + cfg.ClientName + "|" + cfg.Token + "|" + component + "|" + bucket
}

func deterministicJitter(seed string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	return time.Duration(h.Sum64() % uint64(window))
}
