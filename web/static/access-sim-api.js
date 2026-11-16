/*
 * access-sim-api.js - adapter between the "Test access" tab and the backend
 * contract (docs/access-explain.md sections 3 and 4). Pure, no DOM.
 * Exposed as window.ASAPI (and module.exports under node tests).
 *
 * Every backend field name the tab depends on is read or written here only.
 *   POST   /admin/api/phantom-tokens/{id}/simulate
 *     request  (action): mode, repo, action, ref, workflow
 *     request  (raw):    mode, method, path, headers{name:value}, body_text | body_hex
 *     response: verdict, kind, title, steps[].{status,label,detail}, refs[].{ref,verdict,pattern,role,reason},
 *               sees, depends_on_default_branch, per_key[].{key|name|label, verdict, title}
 *   GET    /admin/api/phantom-tokens/{id}/access-summary
 *     repos[].{repo, rules[].{pattern, role, push, create_force_delete, pr_into, run_workflows},
 *              tags, merge, approve, clone}
 */
(function (root) {
  'use strict';
  var H = typeof module === 'object' && module && module.exports ? require('./access-sim-hex.js') : root.ASHEX;

  var ACTIONS = [
    { id: 'clone', label: 'Clone / fetch', ref: false },
    { id: 'push', label: 'Push commits', ref: true },
    { id: 'force_or_delete', label: 'Force push or delete', ref: true },
    { id: 'create_branch', label: 'Create branch', ref: true },
    { id: 'open_pr_into', label: 'Open PR into', ref: true },
    { id: 'merge_pr', label: 'Merge PR into', ref: true },
    { id: 'approve_pr', label: 'Approve a PR', ref: false },
    { id: 'create_tag', label: 'Create tag', ref: true },
    { id: 'dispatch', label: 'Run workflow on', ref: true, workflow: true },
    { id: 'release', label: 'Publish release', ref: false }
  ];
  function actionDef(id) { for (var i = 0; i < ACTIONS.length; i++) if (ACTIONS[i].id === id) return ACTIONS[i]; return ACTIONS[1]; }

  var ROLE = { read: 'Read only', protected: 'Protected', agent: 'Agent-owned' };
  function roleLabel(r) { var k = String(r || '').toLowerCase(); return ROLE[k] || String(r || ''); }

  function pick(o, names) {
    for (var i = 0; i < names.length; i++) if (o && o[names[i]] !== undefined && o[names[i]] !== null) return o[names[i]];
    return undefined;
  }
  function str(v) { return typeof v === 'string' ? v : (v === null || v === undefined ? '' : String(v)); }

  /* ---- requests ---- */
  function actionRequest(st) {
    var def = actionDef(st.action), r = { mode: 'action', repo: st.repo, action: def.id };
    if (def.ref && st.ref) r.ref = st.ref;
    if (def.workflow && st.workflow) r.workflow = st.workflow;
    return r;
  }
  // st: {method, path, headers (object), bytes (Uint8Array)}
  function rawRequest(st) {
    var r = { mode: 'raw', method: st.method, path: st.path };
    if (st.headers && Object.keys(st.headers).length) r.headers = st.headers;
    if (st.bytes && st.bytes.length) {
      if (H.needsHex(st.bytes)) r.body_hex = H.formatHex(st.bytes).replace(/\n/g, ' ');
      else r.body_text = H.decodeUTF8(st.bytes);
    }
    return r;
  }

  /* ---- responses ---- */
  var VERDICT = { allowed: 'Allowed', refused: 'Refused', held: 'Held for approval' };
  function stepStatus(s) {
    s = String(s || '').toLowerCase();
    return s === 'pass' || s === 'stop' ? s : 'skip';
  }
  function adaptResult(r) {
    r = r || {};
    var v = String(r.verdict || '').toLowerCase();
    if (!VERDICT[v]) v = 'refused';
    return {
      verdict: v,
      verdictLabel: VERDICT[v],
      kind: str(r.kind),
      title: str(r.title),
      steps: (Array.isArray(r.steps) ? r.steps : []).map(function (s) {
        s = s || {};
        return { status: stepStatus(s.status), label: str(s.label), detail: str(s.detail) };
      }),
      refs: (Array.isArray(r.refs) ? r.refs : []).map(function (x) {
        x = x || {};
        return { ref: str(x.ref), ok: /^(ok|allow|allowed|pass)/i.test(str(x.verdict)), pattern: str(x.pattern), role: str(x.role), reason: str(x.reason) };
      }),
      sees: str(r.sees),
      dependsOnDefault: !!r.depends_on_default_branch,
      perKey: (Array.isArray(r.per_key) ? r.per_key : []).map(function (k) {
        k = k || {};
        var kv = String(k.verdict || '').toLowerCase();
        return { key: str(pick(k, ['key', 'name', 'label'])), verdictLabel: VERDICT[kv] || str(k.verdict), title: str(k.title) };
      })
    };
  }

  /* ---- access summary ---- */
  function yesNo(v) { return v === true || /^(yes|on|true|allowed)$/i.test(str(v)); }
  function listText(v) {
    if (Array.isArray(v)) return v.join(', ');
    if (v && typeof v === 'object' && Array.isArray(v.create)) return v.create.join(', ');
    return str(v);
  }
  function adaptSummary(r) {
    var repos = Array.isArray(r) ? r : (r && Array.isArray(r.repos) ? r.repos : []);
    return repos.map(function (p) {
      p = p || {};
      var flags = p.flags && typeof p.flags === 'object' ? p.flags : p;
      var tags = listText(pick(flags, ['tags', 'tag_create']));
      var clone = pick(flags, ['clone']);
      return {
        repo: str(pick(p, ['repo', 'name'])),
        rules: (Array.isArray(p.rules) ? p.rules : []).map(function (x) {
          x = x || {};
          return {
            pattern: str(pick(x, ['pattern', 'branches'])),
            role: roleLabel(x.role),
            push: yesNo(x.push),
            createForceDelete: yesNo(pick(x, ['create_force_delete', 'manage'])),
            prInto: yesNo(pick(x, ['pr_into', 'open_pr'])),
            runWorkflows: yesNo(x.run_workflows)
          };
        }),
        tags: tags,
        merge: yesNo(flags.merge),
        approve: yesNo(flags.approve),
        clone: typeof clone === 'string' && !/^(yes|no|on|off|true|false)$/i.test(clone) ? clone : (clone === undefined ? '' : (yesNo(clone) ? 'allowed' : 'off'))
      };
    });
  }

  // refExamples: ref chips built from the token's own patterns, then generic examples.
  function refExamples(repoSummary) {
    var out = [];
    function add(x) { if (x && out.indexOf(x) < 0) out.push(x); }
    ((repoSummary && repoSummary.rules) || []).forEach(function (r) {
      var p = r.pattern.replace(/^refs\/heads\//, '');
      if (p === '@default') { add('main'); return; }
      if (p === '**' || p === '*') return;
      add(p.replace(/\/?\*\*?$/, function (m) { return m.charAt(0) === '/' ? '/sim' : ''; }).replace(/\*+/g, 'sim'));
    });
    add('main'); add('feature/login'); add('v2.0.0');
    return out.slice(0, 8);
  }

  var api = {
    ACTIONS: ACTIONS, actionDef: actionDef, roleLabel: roleLabel,
    actionRequest: actionRequest, rawRequest: rawRequest,
    adaptResult: adaptResult, adaptSummary: adaptSummary, refExamples: refExamples
  };
  if (typeof module === 'object' && module && module.exports) module.exports = api;
  else root.ASAPI = api;
})(typeof window !== 'undefined' ? window : this);
