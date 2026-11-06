// Regenerates the JSON the admin UI serializer emits, consumed by the Go test
// internal/server/services/github_branch_ui_contract_test.go.
// Every config goes through the page's real serializer: the client dialog's
// gitACLForRepoGrants (extracted from client_detail.html), phantom-common.js
// presets and phantom-acl.js builder. The Go wrapper web/gbr_js_test.go
// regenerates into a temp dir and byte-compares with the committed fixtures.
// Usage: node gen_gbr_fixtures.js <web/static dir> <out dir> <client_detail.html>
'use strict';
const fs = require('fs');
const vm = require('vm');
const path = require('path');
const [, , staticDir, outDir, clientTpl] = process.argv;
const load = (name, win) => { const sb = { window: win }; vm.createContext(sb); vm.runInContext(fs.readFileSync(path.join(staticDir, name), 'utf8'), sb); return sb.window; };
const G = load('github-branch-rules.js', {}).GBR;
const PT = load('phantom-common.js', { GBR: G }).PT;
const A = load('phantom-acl.js', {}).PTACL;
// Pull named top-level functions out of the client detail template script.
function extractFn(src, name) {
  const at = src.indexOf('function ' + name + '(');
  if (at < 0) throw new Error('function ' + name + ' not found in ' + clientTpl);
  let i = src.indexOf('{', at), depth = 0;
  for (let j = i; j < src.length; j++) {
    if (src[j] === '{') depth++;
    else if (src[j] === '}' && --depth === 0) return src.slice(at, j + 1);
  }
  throw new Error('unbalanced function ' + name);
}
const tplSrc = fs.readFileSync(clientTpl, 'utf8');
const clientSb = {};
vm.createContext(clientSb);
vm.runInContext(['githubPresetCapabilities', 'gitACLForRepoGrants'].map((n) => extractFn(tplSrc, n)).join('\n'), clientSb);
const clientACL = (grants) => clientSb.gitACLForRepoGrants(grants);
const plain = (v) => JSON.parse(JSON.stringify(v));
const write = (name, v) => fs.writeFileSync(path.join(outDir, name), JSON.stringify(plain(v), null, 2) + '\n');

// 1. New v2 grant with the agent template (client dialog: push + PR write, no dispatch).
const agent = G.defaultValue({ push: true, prWrite: true, dispatch: false });
const agentCaps = { clone: true, push: true, issues_read: true, issues_write: true, pull_requests_read: true, pull_requests_write: true, releases_read: true, releases_write: false, actions_read: true, workflow_dispatch: false, workflow_rerun: false, workflow_cancel: false, pull_requests_merge: agent.pull_requests_merge, pull_requests_approve: agent.pull_requests_approve };
write('v2_agent_template.json', clientACL([{ repo: 'acme/api', capabilities: agentCaps, workflow_allowlist: [], environment_allowlist: [], branch_policy: agent.branch_policy }]));

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

// 4. Converted legacy grant (push + dispatch, ref_allowlist main, release/*): the editor state after "Convert", serialized by the client dialog.
const ctx = { push: true, prWrite: true, dispatch: true, requireRun: true, workflowCount: 1 };
const conv = G.convertLegacy({ push: true, dispatch: true, refs: ['main', 'release/*'], prWrite: true, builder: false });
const cv = G.stateValue(conv, ctx);
const convCaps = Object.assign({}, agentCaps, { workflow_dispatch: true, workflow_rerun: true, workflow_cancel: true, pull_requests_merge: cv.pull_requests_merge, pull_requests_approve: cv.pull_requests_approve });
write('v2_converted_legacy.json', clientACL([{ repo: 'acme/api', capabilities: convCaps, workflow_allowlist: ['ci.yml'], environment_allowlist: [], branch_policy: cv.branch_policy }]));

// 5. Pattern validation as the UI judges and serializes it. `serialized` is what the
// editor emits for that raw row text (trimmed, no refs/heads/), or null when the UI refuses it.
const cands = ['', ' ', '/x', 'a..b', 'a b', 'a~b', 'a^b', 'a:b', 'a?b', 'a[b', 'a\\b', 'a\tb', 'a\u007fb', 'a//b', 'a@{b', 'x'.repeat(200), 'x'.repeat(201), '@default', 'refs/heads/agent/**', 'release/*', 'feature-1', 'v1.2', '**', 'a/*/b', ' main', 'main ', '\tmain\n', ' refs/heads/dev '];
function serialized(raw) {
  const st = G.newState([{ branches: 'x', role: 'agent', run_workflows: false }], {});
  st.rules[1].branches = raw; // the edit path: raw text, not the trimmed load path
  return G.stateValue(st, {}).branch_policy.rules[1].branches;
}
write('pattern_validation.json', cands.map((p) => ({ pattern: p, branch_error: !!G.patternError(p, false), branch_error_default_allowed: !!G.patternError(p, true), tag_error: !!G.tagPatternError(p), serialized: G.patternError(p, false) ? null : serialized(p) })));
