package services

import "testing"

func TestValidateUpstreamProxyURLSchemes(t *testing.T) {
	valid := []string{
		"",
		"http://proxy.example:8080",
		"https://user:secret@proxy.example:8443",
		"socks4://proxy.example:1080",
		"socks4a://proxy.example:1080",
		"socks5://proxy.example:1080",
		"socks5h://proxy.example:1080",
	}
	for _, raw := range valid {
		if err := ValidateUpstreamProxyURL(raw); err != nil {
			t.Fatalf("%q should be valid: %v", raw, err)
		}
	}

	invalid := []string{
		"proxy.example:8080",
		"http://",
		"file:///tmp/proxy",
		"http://proxy.example:8080?x=1",
		"http://proxy.example:8080#frag",
	}
	for _, raw := range invalid {
		if err := ValidateUpstreamProxyURL(raw); err == nil {
			t.Fatalf("%q should be invalid", raw)
		}
	}
}

func TestRedactProxyURLHidesPassword(t *testing.T) {
	got := RedactProxyURL("http://user:secret@proxy.example:8080")
	if got != "http://user@proxy.example:8080" {
		t.Fatalf("redacted URL = %q", got)
	}
}

func TestUpstreamProxyClientCacheAcceptsSOCKS4WithoutUserinfo(t *testing.T) {
	cache := NewUpstreamProxyClientCache()
	if _, err := cache.Client("socks4://proxy.example:1080"); err != nil {
		t.Fatalf("socks4 client: %v", err)
	}
}
