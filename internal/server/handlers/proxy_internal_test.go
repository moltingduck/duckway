package handlers

import (
	"net/http"
	"testing"
)

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

func TestProxyACLRequestMapsGitDiscoveryToPackPermission(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		query      string
		wantMethod string
		wantPath   string
	}{
		{
			name:       "upload-pack discovery",
			method:     http.MethodGet,
			path:       "/OWNER/REPO.git/info/refs",
			query:      "service=git-upload-pack",
			wantMethod: http.MethodPost,
			wantPath:   "/OWNER/REPO.git/git-upload-pack",
		},
		{
			name:       "receive-pack discovery",
			method:     http.MethodGet,
			path:       "/OWNER/REPO.git/info/refs",
			query:      "service=git-receive-pack",
			wantMethod: http.MethodPost,
			wantPath:   "/OWNER/REPO.git/git-receive-pack",
		},
		{
			name:       "plain info refs unchanged",
			method:     http.MethodGet,
			path:       "/OWNER/REPO.git/info/refs",
			query:      "",
			wantMethod: http.MethodGet,
			wantPath:   "/OWNER/REPO.git/info/refs",
		},
		{
			name:       "post unchanged",
			method:     http.MethodPost,
			path:       "/OWNER/REPO.git/git-receive-pack",
			query:      "",
			wantMethod: http.MethodPost,
			wantPath:   "/OWNER/REPO.git/git-receive-pack",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			gotMethod, gotPath := proxyACLRequest(tt.method, tt.path, tt.query)
			if gotMethod != tt.wantMethod || gotPath != tt.wantPath {
				t.Fatalf("proxyACLRequest = (%q, %q), want (%q, %q)", gotMethod, gotPath, tt.wantMethod, tt.wantPath)
			}
		})
	}
}
