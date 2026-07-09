package handlers

import "testing"

func TestEffectiveProxyUpstreamBaseURLGitHubGitPathsUseGitHubHost(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "info refs", path: "/OWNER/REPO.git/info/refs", want: "https://github.com"},
		{name: "upload pack", path: "/OWNER/REPO.git/git-upload-pack", want: "https://github.com"},
		{name: "receive pack", path: "/OWNER/REPO.git/git-receive-pack", want: "https://github.com"},
		{name: "rest api", path: "/repos/OWNER/REPO", want: "https://api.github.com"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveProxyUpstreamBaseURL("github", "https://api.github.com", tt.path)
			if got != tt.want {
				t.Fatalf("effectiveProxyUpstreamBaseURL = %q, want %q", got, tt.want)
			}
		})
	}
	if got := effectiveProxyUpstreamBaseURL("github", "https://github.example.test", "/OWNER/REPO.git/info/refs"); got != "https://github.example.test" {
		t.Fatalf("custom github upstream = %q", got)
	}
}
