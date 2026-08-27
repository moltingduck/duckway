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
		"data-days=\"90\"",
		"usage-heatmap",
		"unpriced_requests",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("usage template missing %q", want)
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
		{"templates/api_keys.html", []string{"LLM Tokens (90d)", "View usage details", "/api/usage/detail?key_id="}},
		{"templates/oauth.html", []string{"LLM Tokens (90d)", "View usage details", "/api/usage/detail?key_id="}},
		{"templates/services.html", []string{"Model Pricing", "USD / 1M", "/pricing", "usage_metering"}},
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
