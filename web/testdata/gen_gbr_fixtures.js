// Regenerates the JSON the admin UI serializer emits, consumed by the Go test
// internal/server/services/github_branch_ui_contract_test.go.
// Usage: node gen_gbr_fixtures.js <web/static dir> <out dir>
'use strict';
const fs = require('fs');
const vm = require('vm');
const path = require('path');
const [, , staticDir, outDir] = process.argv;
const load = (name, win) => { const sb = { window: win }; vm.createContext(sb); vm.runInContext(fs.readFileSync(path.join(staticDir, name), 'utf8'), sb); return sb.window; };
const G = load('github-branch-rules.js', {}).GBR;
const PT = load('phantom-common.js', { GBR: G }).PT;
const A = load('phantom-acl.js', {}).PTACL;
const plain = (v) => JSON.parse(JSON.stringify(v));
const write = (name, v) => fs.writeFileSync(path.join(outDir, name), JSON.stringify(plain(v), null, 2) + '\n');

// 1. New v2 grant with the agent template (client dialog, push + PR write, no dispatch).
const agent = G.defaultValue({ push: true, prWrite: true, dispatch: false });
write('v2_agent_template.json', { version: '2', provider: 'github', repositories: { 'acme/api': {
  capabilities: { clone: true, push: true, pull_requests_read: true, pull_requests_write: true, pull_requests_merge: agent.pull_requests_merge, pull_requests_approve: agent.pull_requests_approve },
  branch_policy: agent.branch_policy } } });

// 2. cicd preset from the shared preset serializer.
write('v2_cicd_preset.json', PT.gitACLForRepos(['acme/ci'], 'cicd', 'ci.yml', 'main, release/*', ''));

// 3. Builder rule carrying github_branch_policies.
A.setGitHubTable([
  { key: 'contents', label: 'Contents', read: [{ method: 'GET', path: '/repos/{repo}/contents/*' }], write: [{ method: 'PUT', path: '/repos/{repo}/contents/*' }] },
  { key: 'pull_requests', label: 'Pull requests', read: [{ method: 'GET', path: '/repos/{repo}/pulls/*' }], write: [{ method: 'POST', path: '/repos/{repo}/pulls' }] }]);
const g = A.newGitHubRule();
g.caps['Acme/Api'] = { contents: 'write', pull_requests: 'write' };
const bv = G.defaultValue({ push: true, prWrite: true, dispatch: false });
bv.pull_requests_merge = true;
g.bp['acme/api'] = bv;
write('builder_branch_policies.json', JSON.parse(A.build({ version: '1', provider: 'github', rules: [g], extra: {}, v2: null })));

// 4. Converted legacy grant (push + dispatch, ref_allowlist main, release/*).
const ctx = { push: true, prWrite: true, dispatch: true, requireRun: true, workflowCount: 1 };
const conv = G.convertLegacy({ push: true, dispatch: true, refs: ['main', 'release/*'], prWrite: true, builder: false });
const cv = G.stateValue(conv, ctx);
write('v2_converted_legacy.json', { version: '2', provider: 'github', repositories: { 'acme/api': {
  capabilities: { clone: true, push: true, pull_requests_read: true, pull_requests_write: true, actions_read: true, workflow_dispatch: true, pull_requests_merge: cv.pull_requests_merge, pull_requests_approve: cv.pull_requests_approve },
  workflow_allowlist: ['ci.yml'], branch_policy: cv.branch_policy } } });

// 5. Pattern validation as the UI judges it.
const cands = ['', '/x', 'a..b', 'a b', 'a~b', 'a^b', 'a:b', 'a?b', 'a[b', 'a\\b', 'a\tb', 'a\u007fb', 'a//b', 'a@{b', 'x'.repeat(200), 'x'.repeat(201), '@default', 'refs/heads/agent/**', 'release/*', 'feature-1', 'v1.2', '**', 'a/*/b'];
write('pattern_validation.json', cands.map((p) => ({ pattern: p, branch_error: !!G.patternError(p, false), branch_error_default_allowed: !!G.patternError(p, true), tag_error: !!G.tagPatternError(p) })));
