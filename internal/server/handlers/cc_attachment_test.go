package handlers

import "testing"

func TestSanitizeAttachmentFilename(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"../../secret.png", "secret.png"},
		{`..\secret.png`, "secret.png"},
		{"", "attachment"},
		{"bad\nname.txt", "bad_name.txt"},
	}
	for _, tc := range cases {
		if got := sanitizeAttachmentFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeAttachmentFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
