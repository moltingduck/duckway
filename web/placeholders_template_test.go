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
		`body.permission_config = JSON.stringify(gitACLForRepos(repos, document.getElementById('add-github-mode').value || 'deploy'`,
		`Repository for clone example`,
		`Granted repositories`,
		`function gitGrantsFromACL(configJSON)`,
		`function applyEditGitHubRepoScope()`,
		`function gitACLForRepos(repos, mode, workflows, refs, environments)`,
		`function parseGitRepoList(value)`,
		`.replace(/\.git$/, '')`,
		`duckway git list`,
		`duckway git setup`,
		`# Pick a repo from ` + "`" + `duckway git list` + "`" + `:`,
		`duckway git clone ' + repo`,
		`version:'2',provider:'github',repositories:repositories`,
		`workflow_dispatch`,
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
		`onclick="rotateClientToken()"`,
		`/rotate-token`,
		`function showClientToken(data, rotated)`,
		`Reconfigure the Duckway client`,
		`isRefreshable:{{.IsRefreshable}}`,
		`isMintable:{{.IsMintable}}`,
		`permissionConfig:"{{if .PermissionConfig}}{{deref .PermissionConfig}}{{end}}"`,
		`id="assign-github-repo-scope"`,
		`id="eph-github-repo-scope"`,
		`function loadAssignGitHubRepos()`,
		`assignGitHubRepoLoadSeq`,
		`/api/keys/' + encodeURIComponent(key.id) + '/github-app/repositories`,
		`assign-github-repo-enabled`,
		`data-capability`,
		`github-policy-preset`,
		`github-workflows`,
		`github-refs`,
		`installation_permissions`,
		`function githubCapabilityRequirement(capability)`,
		`function githubPresetMissingPermissions(preset, permissions)`,
		`Read only</strong> needs`,
		`Developer</strong> needs`,
		`CI/CD operator</strong> needs`,
		`repo-grant-permission-note`,
		`GitHub App installation permission is insufficient`,
		`repo-grant-header`,
		`max-height:52vh`,
		`title="' + escHTML(name) + '"`,
		`function onGitHubGrantToggle(input)`,
		`row.querySelector('.github-workflows').value='ci.yml'`,
		`row.querySelector('.github-refs').value='main'`,
		`function selectedAssignGitHubRepoGrants()`,
		`function selectedGitHubRepoGrants(scopeSelector)`,
		`body.permission_config = JSON.stringify(gitACLForRepoGrants(grants));`,
		`showToast('Enable at least one repository', 'error');`,
		`badges.push('Refreshable')`,
		`Mintable`,
		`function existingMintableAssignment(clientId, apiKeyId)`,
		`Overwrite existing assignment?`,
		`Granted repositories`,
		`function gitGrantsFromACL(configJSON)`,
		`function loadEditGitHubRepos()`,
		`.replace(/\.git$/, '')`,
		`Repository for clone example`,
		`duckway git list`,
		`duckway git setup`,
		`# Pick a repo from ` + "`" + `duckway git list` + "`" + `:`,
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
