package version

import (
	"strings"
	"testing"
)

func TestGetUsesEmbeddedVersion(t *testing.T) {
	old := Embedded
	t.Cleanup(func() { Embedded = old })
	Embedded = "v-test"

	got := Get()
	if !strings.HasPrefix(got, "v-test") {
		t.Fatalf("Get() = %q, want embedded version prefix", got)
	}
	if !strings.Contains(got, "go") {
		t.Fatalf("Get() = %q, want Go version suffix", got)
	}
}
