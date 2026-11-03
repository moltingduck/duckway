package handlers

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hackerduck/duckway/internal/server/middleware"
)

func TestBuildCrumbs(t *testing.T) {
	cases := []struct {
		name string
		page string
		d    pageData
		want []Crumb
	}{
		{"section page", "credentials", pageData{Title: "Credentials", Active: "credentials"},
			[]Crumb{{Label: "Credentials", Current: true}}},
		{"overview", "dashboard", pageData{Title: "Overview", Active: "dashboard"},
			[]Crumb{{Label: "Overview", Current: true}}},
		{"access", "phantom_tokens", pageData{Title: "Phantom tokens", Active: "placeholders"},
			[]Crumb{{Label: "Access"}, {Label: "Phantom tokens", Current: true}}},
		{"detail", "phantom_token_detail", pageData{Title: "NAME", Active: "placeholders"},
			[]Crumb{{Label: "Access"}, {Label: "Phantom tokens", Href: "/admin/phantom-tokens"}, {Label: "NAME", Current: true}}},
		{"client detail", "client_detail", pageData{Title: "box-1", Active: "clients"},
			[]Crumb{{Label: "Fleet"}, {Label: "Clients", Href: "/admin/clients"}, {Label: "box-1", Current: true}}},
		{"explicit crumb", "docs", pageData{Title: "Documentation", Active: "docs", Crumb: "Release notes"},
			[]Crumb{{Label: "System"}, {Label: "Docs", Href: "/admin/docs"}, {Label: "Release notes", Current: true}}},
		{"no active", "moved", pageData{Title: "Page moved"}, []Crumb{{Label: "Page moved", Current: true}}},
	}
	for _, c := range cases {
		if got := buildCrumbs(c.page, c.d); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

func TestShortVersionAndHost(t *testing.T) {
	for in, want := range map[string]string{
		"commit abc1234 (built 2026-04-30, go1.25)": "abc1234",
		"commit abc1234-dirty (built x)":            "abc1234-dirty",
		"v1.2.3 (commit abc1234, built x)":          "v1.2.3",
		"dev (go1.25)":                              "dev",
		"":                                          "dev",
	} {
		if got := shortVersion(in); got != want {
			t.Errorf("shortVersion(%q) = %q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"http://duckway-gw.tailnet:8080": "duckway-gw.tailnet",
		"https://gw.example.com/":        "gw.example.com",
		"gw.example.com:8443":            "gw.example.com",
		"":                               "",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q want %q", in, got, want)
		}
	}
}

func TestPrepareShellUsesAdminAndRequestHost(t *testing.T) {
	h := &AdminHandler{}
	r := httptest.NewRequest("GET", "http://console.example:9000/admin/", nil)
	r = r.WithContext(context.WithValue(r.Context(), middleware.AdminUserKey, "alice"))
	d := pageData{Title: "Overview", Active: "dashboard"}
	h.prepareShell(r, "dashboard", &d)
	if d.Shell.AdminUser != "alice" || d.Shell.Initial != "A" || d.Shell.GatewayHost != "console.example" || d.Shell.Version == "" {
		t.Errorf("shell = %+v", d.Shell)
	}
}

func TestOnboardingCrumbIsFleetClientsAddClient(t *testing.T) {
	got := buildCrumbs("onboarding", pageData{Title: "Add client", Active: "clients", Crumb: "Add client"})
	want := []Crumb{{Label: "Fleet"}, {Label: "Clients", Href: "/admin/clients"}, {Label: "Add client", Current: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
