// Runs the shared branch-rule vectors and the pure model checks against
// web/static/github-branch-rules.js. Usage: node gbr_test.js <static-js> <vectors-json>
'use strict';
const fs = require('fs');
const vm = require('vm');
const assert = require('assert');

const [, , srcPath, vecPath, aclPath, commonPath] = process.argv;
const sandbox = { window: {} };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(srcPath, 'utf8'), sandbox, { filename: srcPath });
const G = sandbox.window.GBR;
assert(G, 'GBR not exported');
const vec = JSON.parse(fs.readFileSync(vecPath, 'utf8'));
let n = 0;
// Objects from the vm context have another realm's prototypes, so compare as JSON.
function eq(a, b) { assert.strictEqual(JSON.stringify(a), JSON.stringify(b)); }
function check(name, fn) { try { fn(); n++; } catch (e) { console.error('FAIL ' + name + ': ' + e.message); process.exitCode = 1; } }

vec.glob.forEach((c) => check('glob ' + c.pattern + ' vs ' + c.branch, () => assert.strictEqual(G.globMatch(c.pattern, c.branch), c.match)));
vec.select.forEach((s) => s.cases.forEach((c) => check('select ' + s.name + ' ' + c.branch, () => {
  const hit = G.selectRule(s.rules, s.default_branch, c.branch);
  assert.strictEqual(hit ? hit.branches : null, c.rule);
  assert.strictEqual(hit ? hit.role : 'read', c.role);
  if ('run_workflows' in c) assert.strictEqual(hit ? hit.run_workflows : false, c.run_workflows);
})));
vec.tags.forEach((t) => check('tag ' + t.tag, () => assert.strictEqual(G.tagAllowed(t.create, t.tag), t.allowed)));

// Roles and actions (spec table).
check('actions', () => {
  eq(G.allowedActions('read', false), { push: false, createDelete: false, openPR: false, runWorkflows: false });
  eq(G.allowedActions('protected', true), { push: false, createDelete: false, openPR: true, runWorkflows: true });
  eq(G.allowedActions('agent', false), { push: true, createDelete: true, openPR: true, runWorkflows: false });
});

// Pattern validation.
check('pattern validation', () => {
  ['', '/x', 'a..b', 'a b', 'a~b', 'a:b', 'a?b', 'a[b', 'x'.repeat(201), '@default'].forEach((p) => assert(G.patternError(p, false), 'should reject ' + JSON.stringify(p)));
  ['release/**', 'refs/heads/agent/*', 'feature-1', 'v1.2'].forEach((p) => assert.strictEqual(G.patternError(p, false), '', p));
  assert.strictEqual(G.patternError('@default', true), '');
});

// Default value: the agent template, exactly spec section 1 / 5 JSON.
check('default value', () => {
  const v = G.defaultValue({ push: true, prWrite: true, dispatch: false });
  eq(JSON.parse(JSON.stringify(v)), {
    branch_policy: { rules: [
      { branches: '@default', role: 'protected', run_workflows: false },
      { branches: 'agent/**', role: 'agent', run_workflows: false },
      { branches: '**', role: 'read', run_workflows: false }], tags: { create: [] } },
    pull_requests_merge: false, pull_requests_approve: false });
});

// Serialisation sanitises by capability and never emits ref_allowlist.
check('sanitise', () => {
  const st = G.newState(G.TEMPLATES.ci.rules(), {});
  st.merge = true; st.approve = true;
  const off = G.stateValue(st, { dispatch: false, prWrite: false });
  assert(off.branch_policy.rules.every((r) => r.run_workflows === false));
  assert.strictEqual(off.pull_requests_merge, false);
  assert.strictEqual(off.pull_requests_approve, false);
  const on = G.stateValue(st, { dispatch: true, prWrite: true });
  assert.strictEqual(on.branch_policy.rules[0].run_workflows, true);
  assert.strictEqual(on.pull_requests_merge, true);
  assert(!('ref_allowlist' in on) && !('ref_allowlist' in on.branch_policy));
});

// Validation: dispatch without any run rule, duplicates, empty tag list, limits.
check('problems', () => {
  const st = G.newState(G.TEMPLATES.agent.rules(), {});
  eq(G.stateProblems(st, { dispatch: false }), []);
  assert(G.stateProblems(st, { dispatch: true, requireRun: true }).length === 1);
  assert(G.stateProblems(st, { dispatch: true, workflowCount: 0 }).length === 1);
  st.rules[0].run_workflows = true;
  eq(G.stateProblems(st, { dispatch: true, requireRun: true, workflowCount: 2 }), []);
  st.rules.splice(1, 0, { branches: 'agent/**', role: 'read', run_workflows: false });
  assert(G.stateProblems(st, { dispatch: false }).some((m) => /twice/.test(m)));
  st.rules.splice(1, 1);
  st.tagMode = 'some';
  assert(G.stateProblems(st, {}).some((m) => /tag pattern/.test(m)));
  st.tagText = 'v*, release-*';
  eq(G.stateProblems(st, {}), []);
  eq(G.stateValue(st, {}).branch_policy.tags, { create: ['v*', 'release-*'] });
  const many = G.newState(G.TEMPLATES.agent.rules(), {});
  for (let i = 0; i < 50; i++) many.rules.splice(1, 0, { branches: 'b' + i, role: 'read', run_workflows: false });
  assert(G.stateProblems(many, {}).some((m) => /At most 50/.test(m)));
});

// Fixed rows are always present and ordered; a missing @default keeps the same semantics.
check('fixed layout', () => {
  const rules = G.fixedLayout([{ branches: 'release/*', role: 'agent', run_workflows: false }, { branches: '*', role: 'protected', run_workflows: false }], 'trunk');
  assert.strictEqual(rules[0].branches, '@default');
  assert.strictEqual(rules[0].role, 'protected'); // trunk matched '*' before, so @default keeps that role
  assert.strictEqual(rules[rules.length - 1].branches, '**');
  eq(rules.map((r) => r.branches), ['@default', 'release/*', '*', '**']);
});

// Legacy conversion (spec section 4).
check('convert push', () => {
  const st = G.convertLegacy({ push: true, dispatch: true, refs: ['main', 'release/*', 'main'], prWrite: true, builder: false });
  eq(st.rules.map((r) => [r.branches, r.role, r.run_workflows]),
    [['@default', 'agent', false], ['main', 'agent', true], ['release/*', 'agent', true], ['**', 'agent', false]]);
  assert.strictEqual(st.merge, false);
  assert.strictEqual(st.approve, false);
  assert.strictEqual(st.tagMode, 'none');
});
check('convert read only', () => {
  const st = G.convertLegacy({ push: false, dispatch: true, refs: ['main'] });
  eq(st.rules.map((r) => [r.branches, r.role, r.run_workflows]),
    [['@default', 'read', false], ['main', 'protected', true], ['**', 'read', false]]);
});
check('convert builder merge', () => {
  assert.strictEqual(G.convertLegacy({ push: true, prWrite: true, builder: true }).merge, true);
  assert.strictEqual(G.convertLegacy({ push: true, prWrite: false, builder: true }).merge, false);
  assert(G.legacyDifferences({ push: true, refs: ['main'], dispatch: true }).length >= 3);
});

// Builder storage (spec section 5): the generated rule carries github_branch_policies and survives a round trip.
const A = (() => { const sb = { window: {} }; vm.createContext(sb); vm.runInContext(fs.readFileSync(aclPath, 'utf8'), sb); return sb.window.PTACL; })();
check('builder round trip', () => {
  A.setGitHubTable([{ key: 'contents', label: 'Contents', read: [{ method: 'GET', path: '/repos/{repo}/contents/*' }], write: [{ method: 'PUT', path: '/repos/{repo}/contents/*' }] }]);
  const g = A.newGitHubRule();
  g.caps['Acme/Api'] = { contents: 'write' };
  g.bp['acme/api'] = G.defaultValue({ push: true, prWrite: false, dispatch: false });
  g.bp['gone/repo'] = G.defaultValue({});
  const text = A.build({ version: '1', provider: 'github', rules: [g], extra: {}, v2: null });
  const cfg = JSON.parse(text);
  const rule = cfg.rules[0];
  eq(Object.keys(rule), ['name', 'github_capabilities', 'github_branch_policies', 'endpoints', 'deny_all_other']);
  eq(Object.keys(rule.github_branch_policies), ['acme/api']);
  assert.strictEqual(rule.github_branch_policies['acme/api'].pull_requests_merge, false);
  const back = A.parse(text);
  assert(back.ok && back.rules[0].t === 'g', 'rule must stay a builder rule');
  eq(back.rules[0].bp['acme/api'], rule.github_branch_policies['acme/api']);
  // A repository without an entry keeps legacy behaviour: no field emitted.
  const plain = A.newGitHubRule(); plain.caps['a/b'] = { contents: 'read' };
  assert(!('github_branch_policies' in JSON.parse(A.build({ version: '1', provider: 'github', rules: [plain], extra: {}, v2: null })).rules[0]));
  // A malformed policies field keeps the rule custom (never rewritten).
  rule.github_branch_policies = [];
  assert.strictEqual(A.parse(JSON.stringify(cfg)).rules[0].t, 'c');
});

// Presets: new configs use branch_policy, never ref_allowlist alongside it.
const PTsb = { window: { GBR: G } }; vm.createContext(PTsb); vm.runInContext(fs.readFileSync(commonPath, 'utf8'), PTsb);
const PT = PTsb.window.PT;
check('presets', () => {
  const cfg = JSON.parse(JSON.stringify(PT.gitACLForRepos(['a/dev', 'a/ci', 'a/ro'], 'dev', 'ci.yml', 'main, release/*', '')));
  const dev = cfg.repositories['a/dev'];
  assert(dev.branch_policy && !('ref_allowlist' in dev));
  assert.strictEqual(dev.capabilities.pull_requests_merge, false);
  const ci = JSON.parse(JSON.stringify(PT.gitACLForRepos(['a/ci'], 'cicd', 'ci.yml', 'main, release/*', ''))).repositories['a/ci'];
  assert(!('ref_allowlist' in ci));
  eq(ci.branch_policy.rules.map((r) => [r.branches, r.role, r.run_workflows]), [['@default', 'protected', true], ['main', 'protected', true], ['release/*', 'protected', true], ['**', 'read', false]]);
  const ro = JSON.parse(JSON.stringify(PT.gitACLForRepos(['a/ro'], 'deploy', '', '', ''))).repositories['a/ro'];
  assert(!('branch_policy' in ro) && !('ref_allowlist' in ro));
  // Reading a stored v2 config keeps branch_policy.
  const grants = PT.gitGrantsFromACL(JSON.stringify(cfg));
  assert(grants.find((g) => g.repo === 'a/dev').branch_policy);
});

// Rendering smoke test with a minimal DW stand-in (same escaping rules as dw.js) and a fake host.
(function () {
  class Safe { constructor(h) { this.__html = String(h); } }
  const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;', '`': '&#96;' };
  const part = (v) => v instanceof Safe ? v.__html : Array.isArray(v) ? v.map(part).join('') : (v == null ? '' : String(v).replace(/[&<>"'`]/g, (c) => ESC[c]));
  const DW = {
    actions: {},
    html: (strs, ...vals) => { let o = strs[0]; vals.forEach((v, i) => { o += part(v) + strs[i + 1]; }); return new Safe(o); },
    raw: (x) => new Safe(x),
    setHTML: (el, safe) => { if (!(safe instanceof Safe)) throw new TypeError('SafeHTML required'); el.innerHTML = safe.__html; }
  };
  const sb = { window: { DW }, document: { activeElement: null }, Event: function () {} };
  vm.createContext(sb); vm.runInContext(fs.readFileSync(srcPath, 'utf8'), sb);
  const g = sb.window.GBR;
  const host = () => ({ hidden: false, attrs: {}, innerHTML: '', isConnected: true, contains: () => false, setAttribute(k, v) { this.attrs[k] = v; }, getAttribute(k) { return this.attrs[k]; }, querySelector: () => null, addEventListener() {} });
  const mounted = (opts) => { const h = host(), inst = g.create(opts); g.mount(h, inst); return { h, inst }; };
  check('render hidden without write caps', () => {
    const { h } = mounted({ ctx: { push: false, prWrite: false, dispatch: false } });
    assert.strictEqual(h.hidden, true);
  });
  check('render new grant', () => {
    const { h } = mounted({ ctx: { push: true, prWrite: true, dispatch: true, defaultBranch: 'main', requireRun: true, workflowCount: 1 } });
    assert.strictEqual(h.hidden, false);
    ['Branch rules', 'Agent works on its own branches', 'agent/**', 'All other branches', 'data-action="gbrRole"', 'Try a branch', 'Merge pull requests', 'Workflow dispatch is on, but no branch rule can run workflows'].forEach((w) => assert(h.innerHTML.includes(w), 'missing ' + w));
    assert(!/ disabled/.test(h.innerHTML.split('Run workflows here')[0].split('gbrRun').pop() || ''), 'run box enabled with dispatch');
  });
  check('render legacy notice and convert', () => {
    const { h, inst } = mounted({ ctx: { push: true, prWrite: true, dispatch: true, requireRun: true }, legacy: { ref_allowlist: ['main', 'release/*'] } });
    ['Convert to branch rules', 'Allowed refs', 'release/*', 'New tags can no longer be created'].forEach((w) => assert(h.innerHTML.includes(w), 'missing ' + w));
    assert.strictEqual(g.value(inst), null);
    sb.window.DW.actions.gbrConvert({ closest: () => h });
    assert(h.innerHTML.includes('Converted. Review the rules'));
    assert.strictEqual(g.value(inst).branch_policy.rules.length, 4);
  });
  check('render escapes pattern text', () => {
    const { h } = mounted({ ctx: { push: true }, policy: { branch_policy: { rules: [{ branches: '<img onerror=x>', role: 'agent', run_workflows: false }] } } });
    assert(!h.innerHTML.includes('<img onerror'));
  });
})();

console.log('gbr js tests: ' + n + ' checks' + (process.exitCode ? ' (with failures)' : ' passed'));
