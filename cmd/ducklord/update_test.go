package main

import (
	"net/url"
	"testing"
)

func TestUpdateOriginAndLoopbackRules(t *testing.T) {
	base, _ := url.Parse("https://duckway.example")
	for _, tc := range []struct {
		url            string
		same, loopback bool
	}{
		{"https://duckway.example/download/a", true, false},
		{"https://other.example/download/a", false, false},
		{"http://localhost:8080", false, true},
		{"http://10.0.0.2", false, false},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameOrigin(base, u); got != tc.same {
			t.Errorf("sameOrigin(%q)=%v", tc.url, got)
		}
		if got := isLoopbackHost(u.Hostname()); got != tc.loopback {
			t.Errorf("isLoopbackHost(%q)=%v", tc.url, got)
		}
	}
}
