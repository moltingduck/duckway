package client

import (
	"context"
	"hash/fnv"
	"log"
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
		checkAndLogUpdate(ctx, cfg.ServerURL, component, logger)
		next := interval + deterministicJitter(jitterSeed(cfg, component, time.Now().UTC().Format("2006-01-02T15")), jitterWindow)
		if !sleepContext(ctx, next) {
			return
		}
	}
}

func checkAndLogUpdate(ctx context.Context, serverURL, component string, logger func(string, ...interface{})) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	info, err := CheckUpdateInfo(serverURL, version.Get())
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
	case info.UpdateRecommended:
		if info.Reason != "" {
			logger("[%s] duckway client update available: current=%s target=%s reason=%s; run `duckway update && duckway restart`", component, version.Get(), info.ClientRecommendedVersion, info.Reason)
		} else {
			logger("[%s] duckway client update available: current=%s target=%s; run `duckway update && duckway restart`", component, version.Get(), info.ClientRecommendedVersion)
		}
	}
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
