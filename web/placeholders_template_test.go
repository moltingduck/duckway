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
		`.replace(/\.git$/, '')`,
		`duckway git list`,
		`duckway git setup`,
		`duckway git clone ' + repo`,
		`'.git/git-upload-pack'`,
		`'.git/git-receive-pack'`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("placeholders template missing GitHub repo-scoped assignment contract: %s", want)
		}
	}
}

func TestClientsTemplateSupportsMintableGitHubRepoAssignment(t *testing.T) {
	body, err := Content.ReadFile("templates/clients.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`isRefreshable:{{.IsRefreshable}}`,
		`isMintable:{{.IsMintable}}`,
		`id="assign-github-repo-scope"`,
		`function loadAssignGitHubRepos()`,
		`assignGitHubRepoLoadSeq`,
		`/api/keys/' + encodeURIComponent(key.id) + '/github-app/repositories`,
		`function selectedAssignGitHubRepos()`,
		`body.permission_config = JSON.stringify(gitACLForRepos(repos, document.getElementById('assign-github-mode').value || 'deploy'));`,
		`showToast('Select or enter at least one allowed repository', 'error');`,
		`badges.push('Refreshable')`,
		`Mintable`,
		`.replace(/\.git$/, '')`,
		`duckway git list`,
		`duckway git setup`,
		`duckway git clone ' + repo`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("clients template missing mintable GitHub repo assignment contract: %s", want)
		}
	}
}

func TestAPIKeysTemplateShowsMintableBadge(t *testing.T) {
	body, err := Content.ReadFile("templates/api_keys.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`{{if .IsRefreshable}}<span class="badge badge-green">Refreshable</span>{{end}}`,
		`{{if .IsMintable}}<span class="badge badge-purple">Mintable</span>{{end}}`,
		`id="d-mintable"`,
		`k.is_mintable ? '<span class="badge badge-purple">Yes</span>' : 'No'`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("api keys template missing mintable badge contract: %s", want)
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
		"shown as <strong>Mintable</strong>",
		"Clients</strong> → <strong>Assign Key",
		"repo selector",
		"permission_config",
		"short-lived installation token scoped only to the repository",
		"/api/keys/{id}/github-app/repositories",
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("docs missing GitHub App repo-scope assignment contract: %s", want)
		}
	}
}
