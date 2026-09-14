package supervisor

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ForegroundAgent is an advisory label for a shell-first PTY. It must never
// authorize input, yield, or an exact task-completion notification. An empty
// result means that the process identity could not be established safely.
func (s *Session) ForegroundAgent() string {
	if s == nil || !s.shellFirstHooks || s.rootProcessID <= 0 || s.rootProcessStart == 0 || s.pty == nil {
		return ""
	}
	group, err := unix.IoctlGetInt(int(s.pty.Fd()), unix.TIOCGPGRP)
	if err != nil || group <= 0 {
		return ""
	}
	root, ok := readForegroundProcStat(s.rootProcessID)
	if !ok || root.start != s.rootProcessStart || root.tty == 0 || root.state == 'Z' || root.state == 'X' {
		return ""
	}
	proc, err := os.Open("/proc")
	if err != nil {
		return ""
	}
	defer proc.Close()
	// A saturated process table is uncertainty, not permission to perform an
	// unbounded scan in a frequently polled supervisor path.
	names, err := proc.Readdirnames(maxForegroundProcEntries + 1)
	if err != nil && err != io.EOF || len(names) > maxForegroundProcEntries {
		return ""
	}
	result := ""
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		before, ok := readForegroundProcStat(pid)
		if !ok || before.group != group || before.tty != root.tty || before.state == 'Z' || before.state == 'X' {
			continue
		}
		agent := foregroundAgentProcess(pid)
		if agent == "" {
			continue
		}
		after, ok := readForegroundProcStat(pid)
		if !ok || before != after {
			return ""
		}
		if result != "" && result != agent {
			return ""
		}
		result = agent
	}
	finalRoot, ok := readForegroundProcStat(s.rootProcessID)
	if !ok || finalRoot != root {
		return ""
	}
	finalGroup, err := unix.IoctlGetInt(int(s.pty.Fd()), unix.TIOCGPGRP)
	if err != nil || finalGroup != group {
		return ""
	}
	return result
}

const maxForegroundProcEntries = 4096

type foregroundProcStat struct {
	state  byte
	parent int
	group  int
	tty    int64
	start  uint64
}

func readForegroundProcStat(pid int) (foregroundProcStat, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return foregroundProcStat{}, false
	}
	return parseForegroundProcStat(data)
}

func parseForegroundProcStat(data []byte) (foregroundProcStat, bool) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return foregroundProcStat{}, false
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return foregroundProcStat{}, false
	}
	if len(fields[0]) != 1 {
		return foregroundProcStat{}, false
	}
	parent, parentErr := strconv.Atoi(fields[1])             // ppid, field 4
	group, groupErr := strconv.Atoi(fields[2])               // pgrp, field 5
	tty, ttyErr := strconv.ParseInt(fields[4], 10, 64)       // tty_nr, field 7
	start, startErr := strconv.ParseUint(fields[19], 10, 64) // starttime, field 22
	if parentErr != nil || groupErr != nil || ttyErr != nil || startErr != nil || group <= 0 || start == 0 {
		return foregroundProcStat{}, false
	}
	return foregroundProcStat{state: fields[0][0], parent: parent, group: group, tty: tty, start: start}, true
}

func foregroundAgentProcess(pid int) string {
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(cmdline) == 0 || len(cmdline) > 64<<10 {
		return ""
	}
	args := bytes.Split(cmdline, []byte{0})
	if len(args) == 0 || len(args[0]) == 0 {
		return ""
	}
	exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return ""
	}
	return classifyForegroundAgent(exe, args)
}

// argv is process metadata only. It is never returned, logged, or forwarded.
func classifyForegroundAgent(exe string, args [][]byte) string {
	if len(args) == 0 || len(args[0]) == 0 {
		return ""
	}
	argv0 := filepath.Base(string(args[0]))
	exeBase := filepath.Base(strings.TrimSuffix(exe, " (deleted)"))
	switch {
	case argv0 == "codex" && (exeBase == "codex" || exeBase == "node"):
		return "codex"
	case argv0 == "claude" && (exeBase == "claude" || exeBase == "node" ||
		strings.Contains(exe, "/claude/versions/") && versionedClaudeExecutable(exeBase)):
		return "claude"
	default:
		return ""
	}
}

func versionedClaudeExecutable(name string) bool {
	parts := strings.Split(name, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}
