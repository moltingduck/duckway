package services_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLiveCredentialsAreNotTracked(t *testing.T) {
	root := repoRootFromTest(t)
	out, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Skipf("git ls-files unavailable: %v", err)
	}
	tracked := strings.Fields(string(out))
	secretNameRE := regexp.MustCompile(`(?i)(^|/)(auth\.json|test_auth.*\.json|.*credentials.*\.json|.*secret.*\.json|\.credentials\.json)$`)
	for _, path := range tracked {
		clean := filepath.ToSlash(path)
		if strings.HasPrefix(clean, "live-credentials/") && clean != "live-credentials/.gitkeep" {
			t.Fatalf("live credential file must not be tracked: %s", clean)
		}
		if secretNameRE.MatchString(clean) && !strings.HasPrefix(clean, "testdata/") {
			t.Fatalf("credential-looking file must not be tracked: %s", clean)
		}
	}
}
