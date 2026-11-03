package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
)

// The self-hosted fonts must be exactly the files documented in
// static/fonts/README (Fontsource 5.1.0), so a swapped or corrupted binary is caught.
func TestFontChecksums(t *testing.T) {
	readme, err := fs.ReadFile(Content, "static/fonts/README")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(string(readme), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && len(f[0]) == 64 && strings.HasSuffix(f[1], ".woff2") {
			want[f[1]] = f[0]
		}
	}
	files, err := fs.Glob(Content, "static/fonts/*.woff2")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fonts found: %v", err)
	}
	if len(want) != len(files) {
		t.Fatalf("README lists %d fonts, %d shipped", len(want), len(files))
	}
	for _, f := range files {
		b, err := fs.ReadFile(Content, f)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		name := f[strings.LastIndex(f, "/")+1:]
		if got := hex.EncodeToString(sum[:]); got != want[name] {
			t.Errorf("%s: sha256 %s, README says %s", name, got, want[name])
		}
	}
	if !strings.Contains(string(readme), "@fontsource/ibm-plex-sans@5.1.0") || !strings.Contains(string(readme), "cdn.jsdelivr.net") {
		t.Error("README must name the source package, version and jsDelivr URLs")
	}
}
