package ducklord

import "testing"

func TestParseDucklionProbeOutput(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		available bool
		command   string
		listOK    bool
		sessions  int
		os        string
		arch      string
	}{
		{name: "ducklion", out: "ducklion\tducklion v1\nlist-ok\t[{\"name\":\"a\"},{\"name\":\"b\"}]\n", available: true, command: "ducklion", listOK: true, sessions: 2},
		{name: "platform is probed", out: "ducklion\tducklion v1\nplatform\tlinux\tarm64\n", available: true, command: "ducklion", os: "linux", arch: "arm64"},
		{name: "duckway subcommand", out: "duckway-ducklion\tducklion v1\n", available: true, command: "duckway ducklion"},
		{name: "configured path", out: "configured:/home/duck/.local/bin/ducklion\tducklion v1\nlist-ok\t[]\n", available: true, command: "/home/duck/.local/bin/ducklion", listOK: true},
		{name: "missing", out: "missing\nplatform\tdarwin\tamd64\n", available: false, os: "darwin", arch: "amd64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDucklionProbeOutput(tt.out)
			if err != nil {
				t.Fatal(err)
			}
			if got.Available != tt.available || got.Command != tt.command || got.ListOK != tt.listOK || got.Sessions != tt.sessions || got.OS != tt.os || got.Arch != tt.arch {
				t.Fatalf("probe = %+v", got)
			}
		})
	}
}
