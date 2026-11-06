package web

import (
	"strings"
	"testing"
)

func readContent(t *testing.T, name string) string {
	t.Helper()
	b, err := Content.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Both admin pages load the one shared component and wire it to their serialisers.
func TestBranchRulesComponentSharedByBothPages(t *testing.T) {
	for _, page := range []string{"templates/client_detail.html", "templates/phantom_token_detail.html", "templates/phantom_tokens.html"} {
		if !strings.Contains(readContent(t, page), `/static/github-branch-rules.js`) {
			t.Errorf("%s does not load github-branch-rules.js", page)
		}
	}
	client := readContent(t, "templates/client_detail.html")
	for _, want := range []string{
		`GBR.create(`, `GBR.mount(`, `GBR.value(inst)`, `grant.branch_policy = value.branch_policy`,
		`capabilities.pull_requests_merge = value.pull_requests_merge`, `capabilities.pull_requests_approve = value.pull_requests_approve`,
		`grant.ref_allowlist = inst.legacy.refs.slice()`, `id="assign-github-problem"`, `id="assign-submit"`, `id="eph-github-problem"`,
	} {
		if !strings.Contains(client, want) {
			t.Errorf("client_detail.html missing %q", want)
		}
	}
	// branch_policy and ref_allowlist are mutually exclusive in the serialiser.
	if !strings.Contains(client, "if (grant.branch_policy) repositories[grant.repo].branch_policy = grant.branch_policy;\n\t\telse if (grant.ref_allowlist") {
		t.Error("client_detail.html must emit ref_allowlist only when there is no branch_policy")
	}
	detail := readContent(t, "static/phantom-token-detail.js")
	for _, want := range []string{`GBR.create(`, `GBR.mount(`, `GBR.defaultValue(`, `data-gbr-repo`} {
		if !strings.Contains(detail, want) {
			t.Errorf("phantom-token-detail.js missing %q", want)
		}
	}
	if !strings.Contains(readContent(t, "static/phantom-acl.js"), `github_branch_policies`) {
		t.Error("phantom-acl.js must serialise github_branch_policies")
	}
}

// The shared component renders only through DW.html and carries the contract copy.
func TestBranchRulesComponentContract(t *testing.T) {
	src := readContent(t, "static/github-branch-rules.js")
	for _, bad := range []string{".innerHTML", "insertAdjacentHTML", "alert(", "confirm(", "prompt(", ` style="`, ".style."} {
		if strings.Contains(src, bad) {
			t.Errorf("github-branch-rules.js contains forbidden %q", bad)
		}
	}
	for _, want := range []string{
		"Agent works on its own branches", "CI operator", "Read only", "All other branches", "Run workflows here", "Try a branch",
		"No new tags", "May create tags matching", "Merge pull requests", "Submit approving reviews", "Convert to branch rules",
		"Add branch rule", "Tags and releases", `role="radiogroup"`,
		"The agent writes this branch and can start workflows on it",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("github-branch-rules.js missing %q", want)
		}
	}
	css := readContent(t, "static/style.css")
	for _, want := range []string{".gbr-seg-on", ".gbr-warn", ".gbr-problems", "@container (max-width: 34rem)"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %q", want)
		}
	}
}

// New presets use branch_policy; the cicd preset no longer injects ref_allowlist ['main'].
func TestPhantomCommonPresetsUseBranchPolicy(t *testing.T) {
	src := readContent(t, "static/phantom-common.js")
	if strings.Contains(src, "['main']") {
		t.Error("phantom-common.js still injects a default ref_allowlist")
	}
	if strings.Contains(src, "repositories[grant.repo].ref_allowlist") {
		t.Error("phantom-common.js must not emit ref_allowlist for new configs")
	}
	if !strings.Contains(src, "presetBranchPolicy") {
		t.Error("phantom-common.js missing presetBranchPolicy")
	}
}
