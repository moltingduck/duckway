package client

import "testing"

func TestValidateUpdateInfoRejectsUnsafeManifest(t *testing.T) {
	valid := &UpdateInfo{
		ClientRecommendedVersion: "v1",
		OS:                       "linux",
		Arch:                     "amd64",
		Binary:                   "duckway-client-linux-amd64",
		DownloadURL:              "/download/duckway-client-linux-amd64",
		SHA256:                   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		DucklionBinary:           "ducklion-linux-amd64",
		DucklionDownloadURL:      "/download/ducklion-linux-amd64",
		DucklionSHA256:           "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
	}
	if err := validateUpdateInfo(valid, "linux", "amd64"); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	cases := []struct {
		name string
		info *UpdateInfo
	}{
		{
			name: "wrong binary",
			info: &UpdateInfo{
				ClientRecommendedVersion: "v1",
				OS:                       "linux",
				Arch:                     "amd64",
				Binary:                   "duckway-client-darwin-amd64",
				DownloadURL:              "/download/duckway-client-darwin-amd64",
				SHA256:                   valid.SHA256,
			},
		},
		{
			name: "absolute external url",
			info: &UpdateInfo{
				ClientRecommendedVersion: "v1",
				OS:                       "linux",
				Arch:                     "amd64",
				Binary:                   "duckway-client-linux-amd64",
				DownloadURL:              "https://evil.example/duckway",
				SHA256:                   valid.SHA256,
			},
		},
		{
			name: "bad sha",
			info: &UpdateInfo{
				ClientRecommendedVersion: "v1",
				OS:                       "linux",
				Arch:                     "amd64",
				Binary:                   "duckway-client-linux-amd64",
				DownloadURL:              "/download/duckway-client-linux-amd64",
				SHA256:                   "not-a-sha",
			},
		},
		{
			name: "bad ducklion url",
			info: &UpdateInfo{
				ClientRecommendedVersion: "v1",
				OS:                       "linux",
				Arch:                     "amd64",
				Binary:                   "duckway-client-linux-amd64",
				DownloadURL:              "/download/duckway-client-linux-amd64",
				SHA256:                   valid.SHA256,
				DucklionBinary:           "ducklion-linux-amd64",
				DucklionDownloadURL:      "https://evil.example/ducklion",
				DucklionSHA256:           valid.DucklionSHA256,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateUpdateInfo(tc.info, "linux", "amd64"); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
}

func TestSafeDownloadURLKeepsServerOrigin(t *testing.T) {
	got, err := safeDownloadURL("https://duckway.example/base", "/download/duckway-client-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://duckway.example/download/duckway-client-linux-amd64" {
		t.Fatalf("download url = %q", got)
	}
	if _, err := safeDownloadURL("https://duckway.example", "https://evil.example/duckway"); err == nil {
		t.Fatal("external download url accepted")
	}
	got, err = safeDownloadURL("https://duckway.example/base", "/download/ducklion-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://duckway.example/download/ducklion-linux-amd64" {
		t.Fatalf("ducklion download url = %q", got)
	}
}
