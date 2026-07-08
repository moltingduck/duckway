package web

import (
	"strings"
	"testing"
)

func TestPlaceholdersTemplateSupportsGitHubRepoScopedAssignments(t *testing.T) {
	body, err := Content.ReadFile("templates/placeholders.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`data-name="{{.Name}}"`,
		`id="add-github-repo-scope"`,
		`id="add-github-repos"`,
		`id="add-github-mode"`,
		`selectedAddServiceName() === 'github'`,
		`body.permission_config = JSON.stringify(gitACLForRepos(repos, document.getElementById('add-github-mode').value || 'deploy'));`,
		`function gitACLForRepos(repos, mode)`,
		`function parseGitRepoList(value)`,
		`'.git/git-upload-pack'`,
		`'.git/git-receive-pack'`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("placeholders template missing GitHub repo-scoped assignment contract: %s", want)
		}
	}
}

func TestDocsExplainGitHubAppRepoScopeAssignment(t *testing.T) {
	body, err := Content.ReadFile("templates/docs.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		"GitHub App Repo Scope",
		"uploaded once under API Keys",
		"assigned per client when generating the GitHub phantom token",
		"permission_config",
		"short-lived installation token scoped only to the repository",
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("docs missing GitHub App repo-scope assignment contract: %s", want)
		}
	}
}
