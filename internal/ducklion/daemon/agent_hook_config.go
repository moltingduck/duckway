package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"golang.org/x/sys/unix"
)

const maxAgentSettingsBytes = 1 << 20

// Host settings edits are rare; a single lock also prevents simultaneous
// Codex/Claude edits from interleaving inside this daemon process.
var agentHookSettingsMu sync.Mutex

func configureAgentHook(agent, action string) (protocol.HostAgentHookConfigResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return protocol.HostAgentHookConfigResult{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return protocol.HostAgentHookConfigResult{}, err
	}
	return configureAgentHookInHome(home, executable, agent, action)
}

// configureAgentHook edits only Ducklion's own hook entries. home and executable
// must be supplied by the daemon, never by the remote caller.
func configureAgentHookInHome(home, executable, agent, action string) (protocol.HostAgentHookConfigResult, error) {
	agentHookSettingsMu.Lock()
	defer agentHookSettingsMu.Unlock()
	result := protocol.HostAgentHookConfigResult{Agent: agent}
	if action != "install" && action != "remove" {
		return result, errors.New("invalid hook action")
	}
	if !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\n\r") {
		return result, errors.New("invalid daemon executable")
	}
	var relative string
	var events []string
	switch agent {
	case "codex":
		relative, events = filepath.Join(".codex", "hooks.json"), []string{"Stop"}
	case "claude":
		relative, events = filepath.Join(".claude", "settings.json"), []string{"Stop", "StopFailure"}
	default:
		return result, errors.New("invalid agent type")
	}
	dir := filepath.Join(home, filepath.Dir(relative))
	if action == "remove" {
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			return result, nil
		} else if err != nil {
			return result, err
		}
	}
	dirfd, err := openPrivateSettingsDir(dir)
	if err != nil {
		return result, err
	}
	defer unix.Close(dirfd)
	filename := filepath.Base(relative)
	old, info, err := readPrivateSettingsAt(dirfd, filename)
	if err != nil {
		return result, err
	}
	if info == nil && action == "remove" {
		return result, nil
	}
	if old == nil {
		old = []byte("{}")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(old, &document); err != nil || document == nil {
		return result, errors.New("invalid agent settings JSON object")
	}
	var hooks map[string]json.RawMessage
	if raw := document["hooks"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &hooks); err != nil || hooks == nil {
			return result, errors.New("invalid agent hooks JSON object")
		}
	} else {
		hooks = make(map[string]json.RawMessage)
	}
	command := shellQuoteHook(executable) + " __ducklion_agent_hook_v1 " + agent
	removedAny := false
	for _, event := range events {
		var groups []json.RawMessage
		if raw := hooks[event]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &groups); err != nil || groups == nil {
				return result, fmt.Errorf("invalid %s hook list", event)
			}
		}
		owned, err := ownedHookGroup(agent, command)
		if err != nil {
			return result, err
		}
		found := false
		kept := make([]json.RawMessage, 0, len(groups)+1)
		for _, group := range groups {
			if isOwnedHookGroup(group, agent) {
				found = true
				removedAny = true
				continue
			}
			kept = append(kept, group)
		}
		if action == "install" {
			kept = append(kept, owned)
			result.Installed = true
		} else if !found {
			continue
		}
		encoded, _ := json.Marshal(kept)
		hooks[event] = encoded
	}
	if action == "install" {
		result.Activation = "pending"
	} else if !removedAny {
		return result, nil
	}
	encodedHooks, _ := json.Marshal(hooks)
	document["hooks"] = encodedHooks
	next, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return result, err
	}
	next = append(next, '\n')
	if semanticJSONEqual(old, next) {
		return result, nil
	}
	if err := writePrivateSettingsAt(dirfd, filename, old, info, next); err != nil {
		return protocol.HostAgentHookConfigResult{}, err
	}
	result.Changed = true
	result.BackupCreated = info != nil
	return result, nil
}

func shellQuoteHook(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func ownedHookGroup(agent, command string) (json.RawMessage, error) {
	hook := map[string]any{"type": "command", "command": command, "timeout": 5}
	group := map[string]any{"hooks": []any{hook}}
	if agent == "claude" {
		group["matcher"] = "*"
	}
	return json.Marshal(group)
}

// The command suffix identifies Ducklion's generated single-command group
// across executable upgrades. Extra group/handler fields mean it is no longer
// ours to remove and are deliberately preserved.
func isOwnedHookGroup(raw json.RawMessage, agent string) bool {
	var group map[string]json.RawMessage
	if json.Unmarshal(raw, &group) != nil || (len(group) != 1 && (agent != "claude" || len(group) != 2)) {
		return false
	}
	if agent == "claude" {
		var matcher string
		if json.Unmarshal(group["matcher"], &matcher) != nil || matcher != "*" {
			return false
		}
	}
	var handlers []map[string]json.RawMessage
	if json.Unmarshal(group["hooks"], &handlers) != nil || len(handlers) != 1 || len(handlers[0]) != 3 {
		return false
	}
	var kind, command string
	var timeout int
	if json.Unmarshal(handlers[0]["type"], &kind) != nil || kind != "command" ||
		json.Unmarshal(handlers[0]["command"], &command) != nil ||
		json.Unmarshal(handlers[0]["timeout"], &timeout) != nil || timeout != 5 {
		return false
	}
	suffix := " __ducklion_agent_hook_v1 " + agent
	return strings.HasPrefix(command, "'") && strings.HasSuffix(command, suffix) &&
		strings.HasSuffix(strings.TrimSuffix(command, suffix), "'")
}

func compactHookJSON(raw []byte) []byte {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	compact, _ := json.Marshal(v)
	return compact
}

func semanticJSONEqual(a, b []byte) bool { return bytes.Equal(compactHookJSON(a), compactHookJSON(b)) }

func openPrivateSettingsDir(dir string) (int, error) {
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return -1, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return -1, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return -1, errors.New("agent settings directory is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return -1, errors.New("agent settings directory owner differs")
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil || opened.Dev != uint64(stat.Dev) || opened.Ino != stat.Ino || opened.Uid != uint32(os.Getuid()) || opened.Mode&0022 != 0 {
		unix.Close(fd)
		return -1, errors.New("agent settings directory changed")
	}
	return fd, nil
}

func readPrivateSettingsAt(dirfd int, filename string) ([]byte, os.FileInfo, error) {
	var entry unix.Stat_t
	err := unix.Fstatat(dirfd, filename, &entry, unix.AT_SYMLINK_NOFOLLOW)
	if err == unix.ENOENT {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	fd, err := unix.Openat(dirfd, filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), filename)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameStatIdentity(&entry, opened) {
		return nil, nil, errors.New("agent settings changed during read")
	}
	if err := validateSettingsFile(opened); err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAgentSettingsBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxAgentSettingsBytes {
		return nil, nil, errors.New("agent settings too large")
	}
	return data, opened, nil
}

func sameStatIdentity(entry *unix.Stat_t, info os.FileInfo) bool {
	opened, ok := info.Sys().(*syscall.Stat_t)
	return ok && entry.Dev == uint64(opened.Dev) && entry.Ino == opened.Ino
}

func validateSettingsFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || info.Mode().Perm()&0077 != 0 {
		return errors.New("agent settings file is unsafe")
	}
	return nil
}

func writePrivateSettingsAt(dirfd int, filename string, old []byte, original os.FileInfo, next []byte) error {
	current, info, err := readPrivateSettingsAt(dirfd, filename)
	if original == nil && info == nil {
		current = []byte("{}")
	}
	if err != nil || !bytes.Equal(current, old) || (original == nil) != (info == nil) || (original != nil && !os.SameFile(original, info)) {
		return errors.New("agent settings changed before write")
	}
	if original != nil {
		backup := ".ducklion-hook-backup-" + uuid.NewString() + ".json"
		if err := writeNewPrivateFileAt(dirfd, backup, old); err != nil {
			return err
		}
	}
	tmp := ".ducklion-hook-temp-" + uuid.NewString() + ".json"
	if err := writeNewPrivateFileAt(dirfd, tmp, next); err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(dirfd, tmp, 0) }()
	current, info, err = readPrivateSettingsAt(dirfd, filename)
	if original == nil && info == nil {
		current = []byte("{}")
	}
	if err != nil || !bytes.Equal(current, old) || (original == nil) != (info == nil) || (original != nil && !os.SameFile(original, info)) {
		return errors.New("agent settings changed before replace")
	}
	if err := unix.Renameat(dirfd, tmp, dirfd, filename); err != nil {
		return err
	}
	return unix.Fsync(dirfd)
}

func writeNewPrivateFileAt(dirfd int, filename string, data []byte) error {
	fd, err := unix.Openat(dirfd, filename, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), filename)
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
