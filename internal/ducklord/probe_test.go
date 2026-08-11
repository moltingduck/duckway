package ducklord

import "testing"

func TestParseDucklionProbeOutput(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		available bool
		command   string
	}{
		{name: "ducklion", out: "ducklion\tducklion v1\n", available: true, command: "ducklion"},
		{name: "duckway subcommand", out: "duckway-ducklion\tducklion v1\n", available: true, command: "duckway ducklion"},
		{name: "missing", out: "missing\n", available: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDucklionProbeOutput(tt.out)
			if err != nil {
				t.Fatal(err)
			}
			if got.Available != tt.available || got.Command != tt.command {
				t.Fatalf("probe = %+v", got)
			}
		})
	}
}
