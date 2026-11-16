package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
	"github.com/hackerduck/duckway/internal/version"
)

func run(args []string, out io.Writer, runner remoteRunner) error {
	globalOwner := ""
	if len(args) > 0 && args[0] == "--name" {
		if len(args) < 3 {
			return fmt.Errorf("--name requires a value and command")
		}
		globalOwner = args[1]
		args = args[2:]
	}
	if len(args) == 0 {
		printUsage(out)
		return fmt.Errorf("command is required")
	}
	switch args[0] {
	case "update":
		return runDucklordUpdate(args[1:], out)
	case "approver":
		return runApproverCommand(args[1:], os.Stdin, out)
	case "ssh-hosts":
		path := ""
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--config-file":
				if i+1 >= len(rest) {
					return fmt.Errorf("--config-file requires a value")
				}
				path = rest[i+1]
				i++
			default:
				return fmt.Errorf("unknown ssh-hosts option: %s", rest[i])
			}
		}
		hosts, err := ducklord.LoadSSHConfigHosts(path)
		if err != nil {
			return err
		}
		return printSSHHosts(out, hosts)
	case "clients":
		cfg, _, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		return printClients(out, cfg)
	case "import-ssh-hosts":
		cfgPath, sshConfig, err := parseImportSSHHostsArgs(args[1:])
		if err != nil {
			return err
		}
		lock, err := ducklord.AcquireInstanceLock()
		if err != nil {
			return err
		}
		defer lock.Close()
		cfg, err := loadOrEmptyConfig(cfgPath)
		if err != nil {
			return err
		}
		hosts, err := ducklord.LoadSSHConfigHosts(sshConfig)
		if err != nil {
			return err
		}
		n := importSSHHosts(cfg, hosts)
		if err := ducklord.SaveConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(out, "Imported %d SSH host(s) into %s\n", n, resolvedConfigPath(cfgPath))
		return nil
	case "sessions":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		clientName, jsonOutput, err := parseSessionsArgs(rest)
		if err != nil {
			return err
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		sessions, err := runner.Sessions(context.Background(), c, 8)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(sessions)
		}
		return printSessions(out, sessions)
	case "bookmarks", "projects":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord bookmarks <client> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		projects, err := runner.Projects(context.Background(), c)
		if err != nil {
			return err
		}
		return printProjects(out, projects)
	case "hook-status":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 || rest[1] != "codex" && rest[1] != "claude" {
			return fmt.Errorf("usage: ducklord hook-status <client> <codex|claude> [--config <path>]")
		}
		client, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		status, err := runner.HostAgentHookStatus(context.Background(), client, rest[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(status)
	case "agents":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord agents <client> <directory-path> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		agents, err := runner.Agents(context.Background(), c, rest[1])
		if err != nil {
			return err
		}
		return printAgents(out, agents)
	case "probe":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord probe <client> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		probe, err := runner.ProbeDucklion(context.Background(), c)
		if err != nil {
			return err
		}
		return printProbe(out, probe)
	case "install-ducklion":
		lock, err := ducklord.AcquireInstanceLock()
		if err != nil {
			return err
		}
		defer lock.Close()
		cfg, cfgPath, rest, source, dest, err := parseInstallDucklionArgs(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord install-ducklion <client> [--source <local-path>] [--dest <remote-path>] [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		var installed string
		if strings.TrimSpace(source) != "" {
			installed, err = runner.InstallDucklion(context.Background(), c, source, dest)
		} else if installer, ok := runner.(remoteDucklionInstaller); ok {
			probe, probeErr := runner.ProbeDucklion(context.Background(), c)
			if probeErr != nil {
				return probeErr
			}
			var binary []byte
			binary, err = downloadDucklion(context.Background(), runner, probe.OS, probe.Arch)
			if err == nil {
				installed, err = installer.InstallDucklionBytes(context.Background(), c, binary, dest)
			}
		} else {
			err = fmt.Errorf("remote Ducklion installer is unavailable")
		}
		if err != nil {
			return err
		}
		c.Ducklion = installed
		replaceClient(cfg, c)
		if err := ducklord.SaveConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(out, "Installed ducklion on %s: %s\n", c.Name, installed)
		return nil
	case "read":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		clientName, sessionName, lines, err := parseReadArgs(rest)
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		text, err := runner.Read(context.Background(), c, sessionName, lines)
		if err != nil {
			return err
		}
		fmt.Fprint(out, text)
		return nil
	case "retained":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord retained <client> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		diagnostics, ok := runner.(interface {
			RetainedShells(context.Context, ducklord.Client) ([]protocol.RetainedShellSummary, error)
		})
		if !ok {
			return fmt.Errorf("retained Shell diagnostics are unavailable")
		}
		items, err := diagnostics.RetainedShells(context.Background(), c)
		if err != nil {
			return err
		}
		for _, item := range items {
			fmt.Fprintf(out, "%s  generation %d  %s  exited %s\n", item.SessionID, item.RuntimeGeneration,
				item.Handle, time.UnixMilli(item.ExitedAtMS).Local().Format(time.RFC3339))
		}
		return nil
	case "read-retained":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 3 {
			return fmt.Errorf("usage: ducklord read-retained <client> <session-id> <generation> [--lines N] [--config <path>]")
		}
		generation, err := strconv.ParseUint(rest[2], 10, 64)
		if err != nil || generation == 0 {
			return fmt.Errorf("invalid retained Shell generation %q", rest[2])
		}
		readArgs := append([]string{rest[0], rest[1]}, rest[3:]...)
		clientName, sessionID, lines, err := parseReadArgs(readArgs)
		if err != nil {
			return err
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		diagnostics, ok := runner.(interface {
			ReadRetainedShell(context.Context, ducklord.Client, string, uint64, int) (string, error)
		})
		if !ok {
			return fmt.Errorf("retained Shell diagnostics are unavailable")
		}
		text, err := diagnostics.ReadRetainedShell(context.Background(), c, sessionID, generation, lines)
		if err != nil {
			return err
		}
		fmt.Fprint(out, text)
		return nil
	case "send":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 3 {
			return fmt.Errorf("usage: ducklord send <client> <session> <text>")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Send(context.Background(), c, rest[1], strings.Join(rest[2:], " "))
	case "start":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 2 {
			return fmt.Errorf("usage: ducklord start <client> --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD [ARGS...]")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		startArgs, err := parseDucklordStartArgs(rest[1:])
		if err != nil {
			return err
		}
		_, err = runner.Start(context.Background(), c, startArgs)
		return err
	case "stop":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord stop <client> <session>")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Stop(context.Background(), c, rest[1])
	case "end", "destroy", "restart":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		operation, mode, clientName, sessionRef, err := parseDucklordLifecycle(args[0], rest)
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		lifecycleCtx, stopLifecycleWait := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stopLifecycleWait()
		result, err := runner.Lifecycle(lifecycleCtx, c, sessionRef, operation, mode)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s (generation %d)\n", map[string]string{"end": "Ended", "destroy": "Destroyed", "restart": "Restarted"}[args[0]], result.SessionID, result.RuntimeGeneration)
		return nil
	case "yield":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		wait := false
		filtered := make([]string, 0, len(rest))
		for _, arg := range rest {
			if arg == "-w" || arg == "--wait" {
				wait = true
			} else {
				filtered = append(filtered, arg)
			}
		}
		if len(filtered) != 2 {
			return fmt.Errorf("usage: ducklord yield <client> <session> [-w|--wait]")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, filtered[0])
		if err != nil {
			return err
		}
		result, err := runner.Yield(context.Background(), c, filtered[1], wait)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Yield %s: %s (epoch %d)\n", result.SessionID, result.Decision, result.OwnershipEpoch)
		return nil
	case "attach":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord attach <client> <session>")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		attach, err := runner.AttachStream(context.Background(), c, rest[1])
		if err != nil {
			return err
		}
		defer attach.Stdin.Close()
		defer attach.Stdout.Close()
		if oldState, rawErr := makeRaw(); rawErr == nil {
			defer restore(oldState)
		}
		resize := func() {
			if attach.Resize == nil {
				return
			}
			cols, rows := terminalSize()
			_ = attach.Resize(uint16(rows), uint16(cols))
		}
		resize()
		resizeSignals := make(chan os.Signal, 1)
		resizeDone := make(chan struct{})
		signal.Notify(resizeSignals, syscall.SIGWINCH)
		defer func() { signal.Stop(resizeSignals); close(resizeDone) }()
		go func() {
			for {
				select {
				case <-resizeSignals:
					resize()
				case <-resizeDone:
					return
				}
			}
		}()
		copyInputDone := make(chan struct{})
		go func() { _, _ = io.Copy(attach.Stdin, os.Stdin); close(copyInputDone) }()
		_, outputErr := io.Copy(out, attach.Stdout)
		select {
		case attachErr := <-attach.Done:
			if attachErr != nil {
				return attachErr
			}
		default:
		}
		return outputErr
	case "attach-host":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord attach-host <client> [--config <path>]")
		}
		hostCfg, err := attachHostConfig(cfg, rest[0])
		if err != nil {
			return err
		}
		return runHostTUI(hostCfg, runner, 2*time.Second, globalOwner)
	case "tui":
		cfgPath, ownerFlag, refresh, err := parseTUIFlags(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadOrEmptyConfig(cfgPath)
		if err != nil {
			return err
		}
		if globalOwner != "" && ownerFlag != "" {
			return fmt.Errorf("--name may only be specified once")
		}
		if ownerFlag == "" {
			ownerFlag = globalOwner
		}
		owner, err := ducklord.ResolveOwnerName(ownerFlag, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		lock, err := ducklord.AcquireInstanceLock()
		if err != nil {
			return err
		}
		defer lock.Close()
		return runTUI(cfg, runner, cfgPath, refresh, owner)
	case "version", "--version", "-v":
		fmt.Fprintln(out, "ducklord", version.Get())
		return nil
	case "help", "--help", "-h":
		printUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown ducklord command: %s", args[0])
	}
}
