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
