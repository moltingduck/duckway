package web

import (
	"strings"
	"testing"
)

func TestPhantomTokensPagesSupportGitHubRepoScopedAssignments(t *testing.T) {
	var all strings.Builder
	for _, f := range []string{"templates/phantom_tokens.html", "templates/phantom_token_detail.html", "static/phantom-tokens.js", "static/phantom-common.js", "static/phantom-token-detail.js"} {
		body, err := Content.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(body)
	}
	html := all.String()
	required := []string{
		`data-name="{{.Name}}"`,
		`id="pt-add-github"`,
		`id="pt-add-repos"`,
		`id="pt-add-mode"`,
		`selectedServiceName() !== 'github'`,
		`body.permission_config = JSON.stringify(PT.gitACLForRepos(repos`,
		`Client commands for`,
		`function gitGrantsFromACL(configJSON)`,
		`DW.actions.ptGhAdd`,
		`function gitACLForRepos(repos, mode, workflows, refs, environments)`,
		`function parseGitRepoList(value)`,
		`.replace(/\.git$/, '')`,
		`duckway git list`,
		`duckway git setup`,
		"# pick a repository from `duckway git list`:",
		`'duckway git clone ' + repo`,
		`version: '2', provider: 'github', repositories: repositories`,
		`workflow_dispatch`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("phantom token pages missing GitHub repo-scoped assignment contract: %s", want)
		}
	}
}

func TestPhantomTokenPagesHaveNoForbiddenDialogsOrInlineStyle(t *testing.T) {
	for _, f := range []string{"templates/phantom_tokens.html", "templates/phantom_token_detail.html", "static/phantom-tokens.js", "static/phantom-common.js", "static/phantom-token-detail.js", "static/phantom-acl.js"} {
		body, err := Content.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(body)
		for _, bad := range []string{"alert(", "confirm(", "prompt(", "hx-confirm", ` style="`, ".style.display", ".innerHTML"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains forbidden %q", f, bad)
			}
		}
	}
}

func TestClientDetailTemplateSupportsMintableGitHubRepoAssignment(t *testing.T) {
	body, err := Content.ReadFile("templates/client_detail.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`data-action="dzRotate"`,
		`/rotate-token`,
		`function showClientToken(data)`,
		`Reconfigure the Duckway client`,
		`var apiKeys = {{.Keys}};`,
		`permissionConfig: p.permission_config || ''`,
		`id="assign-github-repo-scope"`,
		`id="eph-github-repo-scope"`,
		`function loadAssignGitHubRepos()`,
		`assignGitHubRepoLoadSeq`,
		`/api/keys/' + encodeURIComponent(key.id) + '/github-app/repositories`,
		`assign-github-repo-enabled`,
		`data-capability`,
		`repo-grant-section-label">Capabilities`,
		`github-workflows`,
		`github-refs`,
		`installation_permissions`,
		`function githubCapabilityRequirement(capability)`,
		`GitHub App installation permission is insufficient: requires `,
		`title="${name}"`,
		`function onGitHubGrantToggle(input)`,
		`if (el && !(el.disabled && !value)) el.checked=value;`,
		`repo-grant-controls`,
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
			t.Fatalf("client detail template missing mintable GitHub repo assignment contract: %s", want)
		}
	}
	if strings.Contains(html, `github-policy-preset`) || strings.Contains(html, `<details class="repo-grant-details"`) {
		t.Fatal("mintable GitHub assignment capabilities should be directly visible without presets")
	}
}

func TestClientDetailTemplateDefaultsFirstUseApprovalOffAndKeepsOptIn(t *testing.T) {
	body, err := Content.ReadFile("templates/client_detail.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `<input type="checkbox" id="assign-approval"> Require admin approval on first use`) {
		t.Fatal("first-use admin approval should be unchecked by default")
	}
	if strings.Contains(html, `<input type="checkbox" id="assign-approval" checked`) {
		t.Fatal("first-use admin approval checkbox should not be checked on initial render")
	}
	if !strings.Contains(html, `requires_approval: document.getElementById('assign-approval').checked`) {
		t.Fatal("assignment should preserve explicit first-use approval opt-in")
	}
}

func TestClientDetailTemplateMakesClientTokenRotationDiscoverable(t *testing.T) {
	body, err := Content.ReadFile("templates/client_detail.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`data-action="dzRotate">Rotate client token…</button>`,
		`The machine must re-run <code>duckway init</code> with the new one-time token.`,
		`The current token for <strong>${im.name}</strong> is revoked immediately.`,
		`showClientToken(data)`,
		`/rotate-token`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("client detail template missing client token rotation contract: %s", want)
		}
	}
	if strings.Contains(html, "Reconfigure connection info") || strings.Contains(html, "Reconfigure Connection Info") {
		t.Fatal("rotation was renamed from Reconfigure connection info to Rotate client token")
	}
}

func TestAPIKeysTemplateShowsMintableBadge(t *testing.T) {
	body, err := Content.ReadFile("templates/credentials.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`k.is_mintable ? 'github-app'`,
		`'github-app': 'GitHub App'`,
		`data-tab="github-app"`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("api keys template missing mintable badge contract: %s", want)
		}
	}
}

func TestAPIKeysTemplateExplainsGitHubAppMinterSetup(t *testing.T) {
	body, err := Content.ReadFile("templates/credentials.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`id="cr-guide"`,
		`Set up a GitHub App for minting tokens`,
		`https://github.com/settings/apps/new`,
		`https://github.com/organizations/ORG/settings/apps/new`,
		`Contents: read-only`,
		`Contents: read and write`,
		`App ID</strong> from the App settings page. Use App ID, not Client ID.`,
		`/settings/installations/ID`,
		`Private keys`,
		`BEGIN`,
		`END`,
		`Setup URL</strong> is an optional redirect after installation; it is not a URL to enter in Duckway.`,
		`Duckway stores it server-side and mints short-lived, scoped installation tokens.`,
		`id="cg-load" data-action="loadGitHubRepos">Test load</button>`,
		`/api/keys/github-app/repositories`,
		`id="cg-repo" aria-label="Repository to test mint" disabled`,
		`for="cg-repo-search"`,
		`id="cg-repo-search" placeholder="Search by repository name" disabled data-on-input="renderGitHubRepos"`,
		`function renderGitHubRepos()`,
		`var filtered = gh.repos.filter(function(r) { return r.toLowerCase().indexOf(query) !== -1; });`,
		`(selected; does not match search)`,
		`select.value = selected && gh.repos.indexOf(selected) !== -1 ? selected : '';`,
		`el('cg-repo-search').disabled = repos.length === 0;`,
		`Load repositories with the current credential and select one first.`,
		`function currentMintTest(credential, repo, version)`,
		`if (!currentMintTest(credential, repo, version)) return;`,
		`if (currentMintTest(credential, repo, version)) githubMintAvail();`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("api keys template missing GitHub App setup guide contract: %s", want)
		}
	}
	if strings.Index(html, `>Test load</button>`) > strings.Index(html, `>Test mint</button>`) {
		t.Fatal("Test load must appear before Test mint")
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
		"uploaded once under Credentials",
		"type <strong>GitHub App</strong>",
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
