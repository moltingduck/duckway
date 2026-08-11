package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDoctorReportsMissingConfigForCurrentClient(t *testing.T) {
	report := RunDoctor(t.TempDir())
	got := report.FormatText()
	for _, want := range []string{
		"Duckway doctor",
		"[MISSING] config",
		"run `duckway init`",
		"ducklion",
		"cc runner mode",
		"agent binaries",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor report missing %q:\n%s", want, got)
		}
	}
}

func TestRunDoctorReportsLocalProjectRegistry(t *testing.T) {
	dir := t.TempDir()
	if err := SaveConfig(dir, &Config{ServerURL: "http://127.0.0.1:1", ClientName: "local", Token: "tok", ProxyPort: 18080}); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(dir, "repo")
	if err := os.Mkdir(projectPath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCCProjectStore(dir).Add([]string{projectPath}, "repo"); err != nil {
		t.Fatal(err)
	}
	got := RunDoctor(dir).FormatText()
	if !strings.Contains(got, "[OK] projects: 1 saved project(s)") {
		t.Fatalf("doctor report =\n%s", got)
	}
	if !strings.Contains(got, "Client: local") {
		t.Fatalf("doctor report missing client name:\n%s", got)
	}
}
