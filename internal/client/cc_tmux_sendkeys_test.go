package client

import (
	"reflect"
	"testing"
)

func TestNormalizeLiteralArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"dash-leading literal gets separator", []string{"-l", "---"}, []string{"-l", "--", "---"}},
		{"plain literal text gets separator too", []string{"-l", "hello"}, []string{"-l", "--", "hello"}},
		{"backslash soft-newline literal", []string{"-l", "\\"}, []string{"-l", "--", "\\"}},
		{"named key untouched", []string{"Enter"}, []string{"Enter"}},
		{"named key Down untouched", []string{"Down"}, []string{"Down"}},
		{"already separated is idempotent", []string{"-l", "--", "---"}, []string{"-l", "--", "---"}},
		{"lone -l untouched", []string{"-l"}, []string{"-l"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeLiteralArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normalizeLiteralArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
