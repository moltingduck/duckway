package ducklord

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type SSHHost struct {
	Name string `json:"name"`
	File string `json:"file,omitempty"`
}

func DefaultSSHConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "config")
}

func LoadSSHConfigHosts(path string) ([]SSHHost, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultSSHConfigPath()
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return ParseSSHConfigHosts(f, path)
}

func ParseSSHConfigHosts(input interface {
	Read([]byte) (int, error)
}, source string) ([]SSHHost, error) {
	seen := map[string]bool{}
	var out []SSHHost
	sc := bufio.NewScanner(input)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(stripSSHConfigComment(sc.Text()))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "Host") {
			continue
		}
		if len(fields) == 1 {
			return nil, fmt.Errorf("%s:%d: Host requires at least one pattern", source, lineNo)
		}
		for _, host := range fields[1:] {
			if !concreteSSHHost(host) || seen[host] {
				continue
			}
			seen[host] = true
			out = append(out, SSHHost{Name: host, File: source})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func stripSSHConfigComment(line string) string {
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '#' {
			return line[:i]
		}
	}
	return line
}

func concreteSSHHost(host string) bool {
	if strings.TrimSpace(host) == "" || strings.HasPrefix(host, "!") {
		return false
	}
	return !strings.ContainsAny(host, "*?")
}
