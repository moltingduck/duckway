package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/client"
	"github.com/hackerduck/duckway/internal/clientplugin"
)

const pluginUsage = "usage: duckway plugin install NAME | install-local --dir PATH | list | installed | update NAME | run NAME -- [ARGS...]"

func runPluginCommand(ctx context.Context, configDir string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	host := clientplugin.Host{ConfigDir: configDir}
	if len(args) == 0 {
		return 2, fmt.Errorf("%s", pluginUsage)
	}
	switch args[0] {
	case "install":
		if len(args) != 2 {
			return 2, fmt.Errorf("%s", pluginUsage)
		}
		host, err := configuredPluginHost(configDir)
		if err != nil {
			return 1, err
		}
		installed, err := host.Install(ctx, args[1])
		if err == nil {
			fmt.Fprintf(stdout, "Installed %s %s\n", installed.Manifest.Name, installed.Manifest.Version)
		}
		reportPluginOperation(ctx, host, args[1], "install", err, stderr)
		if err != nil {
			return 1, err
		}
		return 0, nil
	case "install-local":
		if len(args) != 3 || args[1] != "--dir" || args[2] == "" {
			return 2, fmt.Errorf("usage: duckway plugin install-local --dir PATH")
		}
		installed, err := host.InstallLocal(ctx, args[2])
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(stdout, "Installed local plugin %s %s\n", installed.Manifest.Name, installed.Manifest.Version)
		return 0, nil
	case "list":
		if len(args) != 1 {
			return 2, fmt.Errorf("%s", pluginUsage)
		}
		host, err := configuredPluginHost(configDir)
		if err != nil {
			return 1, err
		}
		plugins, err := host.Catalog(ctx)
		if err != nil {
			return 1, err
		}
		for _, p := range plugins {
			fmt.Fprintf(stdout, "%s\t%s\n", p.Name, p.Version)
		}
		return 0, nil
	case "installed":
		if len(args) != 1 {
			return 2, fmt.Errorf("%s", pluginUsage)
		}
		plugins, err := host.List()
		if err != nil {
			return 1, err
		}
		for _, p := range plugins {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", p.Manifest.Name, p.Manifest.Version, p.Source)
		}
		return 0, nil
	case "update":
		if len(args) != 2 {
			return 2, fmt.Errorf("%s", pluginUsage)
		}
		host, err := configuredPluginHost(configDir)
		if err != nil {
			return 1, err
		}
		installed, err := host.Update(ctx, args[1])
		if err == nil {
			fmt.Fprintf(stdout, "Updated %s %s\n", installed.Manifest.Name, installed.Manifest.Version)
		}
		reportPluginOperation(ctx, host, args[1], "update", err, stderr)
		if err != nil {
			return 1, err
		}
		return 0, nil
	case "run":
		if len(args) < 3 || args[2] != "--" {
			return 2, fmt.Errorf("%s", pluginUsage)
		}
		return runInstalledPlugin(ctx, configDir, args[1], args[3:], stdin, stdout, stderr)
	default:
		return 2, fmt.Errorf("%s", pluginUsage)
	}
}

func configuredPluginHost(configDir string) (clientplugin.Host, error) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		return clientplugin.Host{}, fmt.Errorf("duckway configuration is required; run duckway init")
	}
	return clientplugin.Host{ConfigDir: configDir, ServerURL: cfg.ServerURL, Token: cfg.Token}, nil
}

func runInstalledPlugin(ctx context.Context, configDir, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	env := os.Environ()
	// Reuse proxy exec's routing and CA bundle. A previously installed plugin
	// can run without remote access; absent config preserves the inherited env.
	if _, err := os.Stat(filepath.Join(configDir, "config.yaml")); err == nil {
		cfg, err := client.LoadConfig(configDir)
		if err != nil {
			return 1, fmt.Errorf("load Duckway proxy configuration")
		}
		env = proxyExecEnv(env, cfg.ProxyPort, configDir)
	} else if !os.IsNotExist(err) {
		return 1, fmt.Errorf("inspect Duckway proxy configuration")
	}
	return (clientplugin.Host{ConfigDir: configDir}).Run(ctx, name, args, clientplugin.RunOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr, Env: env})
}

func cmdPlugin(configDir string, args []string, fallback bool) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	var code int
	var err error
	if fallback {
		code, err = runInstalledPlugin(ctx, configDir, args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
	} else {
		code, err = runPluginCommand(ctx, configDir, args, os.Stdin, os.Stdout, os.Stderr)
	}
	cancel()
	if err != nil {
		if errors.Is(err, clientplugin.ErrNotInstalled) {
			fmt.Fprintf(os.Stderr, "Unknown command or uninstalled plugin: %s\n", args[0])
		} else if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "duckway plugin:", err)
		}
	}
	if code != 0 {
		os.Exit(code)
	}
}

func reportPluginOperation(ctx context.Context, host clientplugin.Host, name, operation string, operationErr error, stderr io.Writer) {
	if errors.Is(operationErr, clientplugin.ErrLocalInstall) {
		return
	}
	if clientplugin.ValidateName(name) != nil {
		return
	}
	if operationErr != nil && ctx.Err() != nil {
		operationErr = ctx.Err()
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	err := host.RecordOperation(recordCtx, name, operation, operationErr)
	cancel()
	if err != nil {
		fmt.Fprintln(stderr, "Warning: plugin operation result could not be saved locally for reporting.")
	}
	if err := host.Report(ctx); err != nil {
		fmt.Fprintln(stderr, "Warning: plugin status was not reported to the server; it will retry on the next sync or control heartbeat.")
	}
}
