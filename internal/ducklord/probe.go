package ducklord

import (
	"context"
	"fmt"
	"strings"
)

type DucklionProbe struct {
	Available bool
	Command   string
	Version   string
}

const ducklionProbeScript = `
if command -v ducklion >/dev/null 2>&1; then
  printf 'ducklion\t'
  ducklion version
elif command -v duckway >/dev/null 2>&1 && duckway ducklion version >/dev/null 2>&1; then
  printf 'duckway-ducklion\t'
  duckway ducklion version
else
  printf 'missing\n'
fi
`

func (Runner) ProbeDucklion(ctx context.Context, c Client) (DucklionProbe, error) {
	out, err := sshOutputRaw(ctx, c, "sh", "-lc", ducklionProbeScript)
	if err != nil {
		return DucklionProbe{}, err
	}
	return ParseDucklionProbeOutput(string(out))
}

func ParseDucklionProbeOutput(out string) (DucklionProbe, error) {
	line := strings.TrimSpace(out)
	if line == "" || line == "missing" {
		return DucklionProbe{Available: false}, nil
	}
	kind, versionText, ok := strings.Cut(line, "\t")
	if !ok {
		return DucklionProbe{}, fmt.Errorf("invalid ducklion probe output: %q", line)
	}
	versionText = strings.TrimSpace(versionText)
	switch kind {
	case "ducklion":
		return DucklionProbe{Available: true, Command: "ducklion", Version: versionText}, nil
	case "duckway-ducklion":
		return DucklionProbe{Available: true, Command: "duckway ducklion", Version: versionText}, nil
	default:
		return DucklionProbe{}, fmt.Errorf("unknown ducklion probe kind: %q", kind)
	}
}
