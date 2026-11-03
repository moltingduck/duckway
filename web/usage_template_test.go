package web

import (
	"strings"
	"testing"
)

func TestUsageTemplateUsesMeteredDetailContract(t *testing.T) {
	body, err := Content.ReadFile("templates/usage.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, want := range []string{
		"/api/usage/detail?",
		"key_group_id",
		"us-heat",
		"range=",
		"role=\"tablist\"",
		"Refreshable",
		"unpriced_requests",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("usage template missing %q", want)
		}
	}
	for _, bad := range []string{"<style", " style=", ">OAuth<", "innerHTML", "alert(", "confirm("} {
		if strings.Contains(page, bad) {
			t.Errorf("usage template must not contain %q", bad)
		}
	}
	if strings.Contains(page, "?group_id=") {
		t.Error("usage template uses obsolete group_id detail parameter")
	}
}

func TestTokenAndServiceTemplatesExposeUsageAndPricing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wants []string
	}{
		{"templates/credentials.html", []string{"LLM Tokens (90d)", "View usage details", "/api/usage/detail?key_id="}},
		{"templates/services.html", []string{"Model pricing", "USD / 1M", "/pricing", "usage_metering"}},
	} {
		body, err := Content.ReadFile(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s missing %q", tc.name, want)
			}
		}
	}
}
