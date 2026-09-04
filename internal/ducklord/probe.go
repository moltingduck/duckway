package ducklord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type DucklionProbe struct {
	Available bool
	Command   string
	Version   string
	ListOK    bool
	Sessions  int
	ListError string
}

const ducklionProbeScript = `
probe_list() {
  cmd="$1"
  if out=$($cmd list --json 2>&1); then
    printf 'list-ok\t'
    printf '%s' "$out" | tr -d '\n' | sed 's/\t/ /g'
    printf '\n'
  else
    printf 'list-error\t'
    printf '%s' "$out" | tr -d '\n' | sed 's/\t/ /g'
    printf '\n'
  fi
}
candidate="$1"
if [ -n "$candidate" ] && $candidate version >/dev/null 2>&1; then
  printf 'configured:%s\t' "$candidate"
  $candidate version
  probe_list "$candidate"
elif command -v ducklion >/dev/null 2>&1; then
  printf 'ducklion\t'
  ducklion version
  probe_list ducklion
elif command -v duckway >/dev/null 2>&1 && duckway ducklion version >/dev/null 2>&1; then
  printf 'duckway-ducklion\t'
  duckway ducklion version
  probe_list "duckway ducklion"
else
  printf 'missing\n'
fi
`

func (*Runner) ProbeDucklion(ctx context.Context, c Client) (DucklionProbe, error) {
	out, err := sshOutputRaw(ctx, c, "sh", "-lc", ducklionProbeScript, "ducklord-probe-ducklion", c.Ducklion)
	if err != nil {
		return DucklionProbe{}, err
	}
	return ParseDucklionProbeOutput(string(out))
}

func ParseDucklionProbeOutput(out string) (DucklionProbe, error) {
	lines := nonEmptyLines(out)
	if len(lines) == 0 || lines[0] == "missing" {
		return DucklionProbe{Available: false}, nil
	}
	kind, versionText, ok := strings.Cut(lines[0], "\t")
	if !ok {
		return DucklionProbe{}, fmt.Errorf("invalid ducklion probe output: %q", lines[0])
	}
	versionText = strings.TrimSpace(versionText)
	probe := DucklionProbe{Available: true, Version: versionText}
	if configured, ok := strings.CutPrefix(kind, "configured:"); ok {
		probe.Command = strings.TrimSpace(configured)
		if probe.Command == "" {
			return DucklionProbe{}, fmt.Errorf("invalid configured ducklion probe output: %q", lines[0])
		}
	} else {
		switch kind {
		case "ducklion":
			probe.Command = "ducklion"
		case "duckway-ducklion":
			probe.Command = "duckway ducklion"
		default:
			return DucklionProbe{}, fmt.Errorf("unknown ducklion probe kind: %q", kind)
		}
	}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch key {
		case "list-ok":
			probe.ListOK = true
			probe.Sessions = countJSONListItems(value)
		case "list-error":
			probe.ListError = strings.TrimSpace(value)
		}
	}
	return probe, nil
}

func nonEmptyLines(out string) []string {
	raw := strings.Split(out, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func countJSONListItems(s string) int {
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &items); err != nil {
		return 0
	}
	return len(items)
}
