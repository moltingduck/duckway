/*
 * github-branch-rules.js - GitHub branch rules model and editor, shared by the
 * client "assign mintable GitHub key" dialog and the phantom token builder.
 * Contract: docs/github-branch-rules.md (sections 1, 2, 4, 5).
 *
 * Pure part (no DOM, loadable under node for tests): matching, validation,
 * templates, legacy conversion, serialisation. Exposed on window.GBR.
 * Editor part: GBR.create() builds an instance; GBR.mount(host, inst) renders it
 * through DW.html / DW.setHTML and drives it with data-action / data-on-change
 * attributes (CSP safe, no inline handlers, no string-built HTML).
 */
(function () {
  'use strict';

  var ROLES = ['read', 'protected', 'agent'];
  var RANK = { read: 0, protected: 1, agent: 2 };
  var ROLE_LABEL = { read: 'Read only', protected: 'Protected', agent: 'Agent-owned' };
  var DEFAULT = '@default';
  var OTHER = '**';
  var MAX_RULES = 50, MAX_TAGS = 20, MAX_LEN = 200;

  /* ------------------------------------------------------------------ */
  /* Matching (spec section 1)                                           */
  /* ------------------------------------------------------------------ */
  function stripHeads(s) {
    s = String(s == null ? '' : s).trim();
    return s.indexOf('refs/heads/') === 0 ? s.slice('refs/heads/'.length) : s;
  }
  function stripTags(s) {
    s = String(s == null ? '' : s).trim();
    return s.indexOf('refs/tags/') === 0 ? s.slice('refs/tags/'.length) : s;
  }

  // '*' matches within one '/' segment, '**' matches anything including '/', '?' is literal.
  function globRegex(p) {
    var out = '';
    for (var i = 0; i < p.length; i++) {
      var ch = p.charAt(i);
      if (ch === '*' && p.charAt(i + 1) === '*') { out += '[\\s\\S]*'; i++; }
      else if (ch === '*') out += '[^/]*';
      else out += ch.replace(/[.+?^${}()|[\]\\\/-]/g, '\\$&');
    }
    return new RegExp('^' + out + '$');
  }
  function globMatch(pattern, branch) {
    return globRegex(String(pattern)).test(String(branch));
  }
  function specificity(p) {
    var first = p.indexOf('*');
    return { lit: first < 0 ? p.length : first, stars: (p.match(/\*/g) || []).length, len: p.length };
  }
  // 1 when a is more specific than b, -1 when less, 0 on a tie.
  function compareSpec(a, b) {
    if (a.lit !== b.lit) return a.lit > b.lit ? 1 : -1;
    if (a.stars !== b.stars) return a.stars < b.stars ? 1 : -1;
    if (a.len !== b.len) return a.len > b.len ? 1 : -1;
    return 0;
  }
  // true when rule a beats rule b on the restriction tie-break.
  function moreRestrictive(a, b) {
    var ra = RANK[a.role], rb = RANK[b.role];
    if (ra !== rb) return ra < rb;
    return !a.run_workflows && !!b.run_workflows;
  }

  // The rule that governs `branch`, or null (no rule: role read, no workflows).
  function selectRule(rules, defaultBranch, branch) {
    var b = stripHeads(branch);
    var best = null, bestSpec = null;
    var i, r, p;
    if (defaultBranch && b.toLowerCase() === stripHeads(defaultBranch).toLowerCase()) {
      for (i = 0; i < rules.length; i++) {
        r = rules[i];
        if (r.branches !== DEFAULT) continue;
        if (!best || moreRestrictive(r, best)) best = r;
      }
      if (best) return best;
    }
    for (i = 0; i < rules.length; i++) {
      r = rules[i];
      if (r.branches === DEFAULT) continue;
      p = stripHeads(r.branches);
      if (!globMatch(p, b)) continue;
      var s = specificity(p);
      if (!best) { best = r; bestSpec = s; continue; }
      var c = compareSpec(s, bestSpec);
      if (c > 0 || (c === 0 && moreRestrictive(r, best))) { best = r; bestSpec = s; }
    }
    return best;
  }
  function tagAllowed(create, tag) {
    var t = stripTags(tag);
    return (create || []).some(function (p) { return globMatch(stripTags(p), t); });
  }
  // What a role allows (spec section 1 table); run is the matched rule's flag.
  function allowedActions(role, run) {
    return {
      push: role === 'agent',
      createDelete: role === 'agent',
      openPR: role === 'protected' || role === 'agent',
      runWorkflows: !!run
    };
  }

  /* ------------------------------------------------------------------ */
  /* Validation                                                          */
  /* ------------------------------------------------------------------ */
  // Returns '' or a message. `allowDefault` permits the literal @default.
  function patternError(raw, allowDefault) {
    var p = stripHeads(raw);
    if (!p) return 'Enter a branch pattern.';
    if (p === DEFAULT) return allowDefault ? '' : 'The default branch already has the first row.';
    if (p.length > MAX_LEN) return 'A pattern can be at most ' + MAX_LEN + ' characters.';
    if (/[\u0000- \u007f~^:?\[\\]/.test(p)) return '"' + p + '" has a character branch names cannot contain (space, ~ ^ : ? [ \\).';
    if (p.indexOf('..') !== -1) return '"' + p + '" contains "..", which branch names cannot.';
    if (p.indexOf('//') !== -1 || p.indexOf('@{') !== -1) return '"' + p + '" contains "//" or "@{", which branch names cannot.';
    if (p.charAt(0) === '/') return '"' + p + '" cannot start with "/".';
    return '';
  }
  function tagPatternError(raw) {
    var p = stripTags(raw);
    if (!p) return 'Enter a tag pattern.';
    if (p.length > MAX_LEN) return 'A pattern can be at most ' + MAX_LEN + ' characters.';
    if (/[\u0000- \u007f~^:?\[\\]/.test(p)) return 'Tag pattern "' + p + '" has a character tag names cannot contain.';
    if (p.indexOf('..') !== -1) return 'Tag pattern "' + p + '" contains "..".';
    if (p.indexOf('//') !== -1 || p.indexOf('@{') !== -1) return 'Tag pattern "' + p + '" contains "//" or "@{".';
    if (p === DEFAULT) return 'Tag patterns cannot use ' + DEFAULT + '.';
    if (p.charAt(0) === '/') return 'Tag pattern "' + p + '" cannot start with "/".';
    return '';
  }
  function parseTagList(text) {
    var seen = {}, out = [];
    String(text || '').split(/[\s,]+/).forEach(function (t) {
      t = stripTags(t);
      if (t && !seen[t]) { seen[t] = 1; out.push(t); }
    });
    return out;
  }

  /* ------------------------------------------------------------------ */
  /* Templates and state                                                 */
  /* ------------------------------------------------------------------ */
  function rule(branches, role, run) { return { branches: branches, role: role, run_workflows: !!run }; }
  var TEMPLATES = {
    agent: { label: 'Agent works on its own branches', rules: function () {
      return [rule(DEFAULT, 'protected'), rule('agent/**', 'agent'), rule(OTHER, 'read')];
    } },
    ci: { label: 'CI operator', rules: function () {
      return [rule(DEFAULT, 'protected', true), rule('release/**', 'protected', true), rule(OTHER, 'read')];
    } },
    read: { label: 'Read only', rules: function () {
      return [rule(DEFAULT, 'read'), rule(OTHER, 'read')];
    } }
  };
  var TEMPLATE_ORDER = ['agent', 'ci', 'read'];

  function cloneRule(r) {
    return { branches: String(r.branches), role: ROLES.indexOf(r.role) >= 0 ? r.role : 'read', run_workflows: r.run_workflows === true };
  }
  // Fixed first row (@default) and fixed last row (**); free rows keep their order between.
  function fixedLayout(rules, defaultBranch) {
    var free = [], def = null, other = null;
    rules.forEach(function (r) {
      var c = cloneRule(r);
      if (c.branches === DEFAULT) { if (!def) def = c; return; }
      c.branches = stripHeads(c.branches);
      if (c.branches === OTHER) { if (!other) other = c; return; }
      free.push(c);
    });
    if (!def) {
      var hit = defaultBranch ? selectRule(rules.map(cloneRule), defaultBranch, defaultBranch) : null;
      hit = hit || other;
      def = rule(DEFAULT, hit ? hit.role : 'read', hit ? hit.run_workflows : false);
    }
    if (!other) other = rule(OTHER, 'read', false);
    return [def].concat(free, [other]);
  }
  function newState(rules, ctx) {
    return { rules: fixedLayout(rules, ctx && ctx.defaultBranch), tagMode: 'none', tagText: '', merge: false, approve: false, draft: '', test: 'agent/fix-login' };
  }
  function stateFromPolicy(p, ctx) {
    var bp = (p && p.branch_policy) || {};
    var st = newState(Array.isArray(bp.rules) ? bp.rules : [], ctx);
    var tags = bp.tags && Array.isArray(bp.tags.create) ? bp.tags.create.map(String) : [];
    st.tagMode = tags.length ? 'some' : 'none';
    st.tagText = tags.join(', ');
    st.merge = !!(p && p.pull_requests_merge);
    st.approve = !!(p && p.pull_requests_approve);
    return st;
  }

  // Legacy grant -> editor state (spec section 4 "Conversion").
  // opts: push, dispatch, prWrite, builder, refs (ref_allowlist), defaultBranch.
  function convertLegacy(opts) {
    opts = opts || {};
    var base = opts.push ? 'agent' : 'read';
    var rules = [rule(DEFAULT, base, false)];
    var seen = {};
    seen[OTHER] = 1;
    (opts.refs || []).forEach(function (ref) {
      var p = stripHeads(ref);
      if (!p || seen[p]) return;
      seen[p] = 1;
      rules.push(rule(p, opts.push ? 'agent' : 'protected', !!opts.dispatch));
    });
    rules.push(rule(OTHER, base, false));
    var st = newState(rules, opts);
    st.merge = !!(opts.builder && opts.prWrite);
    st.approve = false;
    return st;
  }
  function legacyDifferences(opts) {
    opts = opts || {};
    var base = opts.push ? 'Agent-owned' : 'Read only';
    var refRole = opts.push ? 'Agent-owned' : 'Protected';
    var out = [];
    out.push('All other branches and the default branch become ' + base + (opts.push ? ' (before: pushes were not limited by branch).' : '.'));
    var refs = (opts.refs || []).map(stripHeads).filter(Boolean);
    if (refs.length) out.push('Each allowed ref (' + refs.join(', ') + ') becomes its own rule: ' + refRole + (opts.dispatch ? ', may run workflows.' : '.'));
    else if (opts.dispatch) out.push('No branch may run workflows until you turn it on for a rule.');
    out.push('New tags can no longer be created (before: not limited).');
    if (opts.builder) out.push(opts.prWrite ? 'Merging pull requests stays on; approving reviews is off.' : 'Merging and approving pull requests are off.');
    else out.push('Merging pull requests and approving reviews are off (before: allowed with pull request write).');
    return out;
  }

  // Rules as enforced given the current capabilities: run_workflows needs dispatch.
  function effectiveRules(st, ctx) {
    return st.rules.map(function (r) {
      // Patterns are serialised trimmed and without refs/heads/, so a stray space never reaches the server.
      return { branches: r.branches === DEFAULT ? DEFAULT : stripHeads(r.branches), role: r.role, run_workflows: !!(r.run_workflows && ctx.dispatch) };
    });
  }
  function stateProblems(st, ctx) {
    var out = [], seen = {}, eff = effectiveRules(st, ctx);
    if (st.rules.length > MAX_RULES) out.push('At most ' + MAX_RULES + ' branch rules are allowed.');
    st.rules.forEach(function (r, i) {
      if (i === 0 || i === st.rules.length - 1) return;
      var e = patternError(r.branches, false);
      if (e) out.push(e);
      var p = stripHeads(r.branches);
      if (p === OTHER) out.push('"**" already has the last row (All other branches).');
      if (seen[p]) out.push('"' + p + '" is listed twice.');
      seen[p] = 1;
    });
    if (st.tagMode === 'some') {
      var tags = parseTagList(st.tagText);
      if (!tags.length) out.push('Enter at least one tag pattern, or choose "No new tags".');
      if (tags.length > MAX_TAGS) out.push('At most ' + MAX_TAGS + ' tag patterns are allowed.');
      tags.forEach(function (t) { var e = tagPatternError(t); if (e) out.push(e); });
    }
    if (ctx.dispatch && ctx.requireRun && !eff.some(function (r) { return r.run_workflows; })) out.push('Workflow dispatch is on, but no branch rule can run workflows. Tick "Run workflows here" on at least one rule.');
    if (ctx.dispatch && ctx.workflowCount === 0) out.push('Workflow dispatch is on: add at least one allowed workflow below.');
    return out;
  }
  function stateValue(st, ctx) {
    var tags = st.tagMode === 'some' ? parseTagList(st.tagText) : [];
    return {
      branch_policy: {
        rules: effectiveRules(st, ctx).map(function (r) { return { branches: r.branches, role: r.role, run_workflows: r.run_workflows }; }),
        tags: { create: tags }
      },
      pull_requests_merge: !!(st.merge && ctx.prWrite),
      pull_requests_approve: !!(st.approve && ctx.prWrite)
    };
  }
  function defaultValue(ctx) {
    ctx = ctx || {};
    return stateValue(newState(TEMPLATES.agent.rules(), ctx), ctx);
  }

  /* ------------------------------------------------------------------ */
  /* Editor instances                                                    */
  /* ------------------------------------------------------------------ */
  var instances = {};
  var seq = 0;
  var dw = typeof window !== 'undefined' ? window.DW : null;

  function prune() {
    Object.keys(instances).forEach(function (id) {
      var h = instances[id].host;
      if (h && !h.isConnected) delete instances[id];
    });
  }

  // opts: ctx {push, prWrite, dispatch, defaultBranch, requireRun, workflowCount, builder},
  //       policy (stored {branch_policy,...}) | legacy ({ref_allowlist}) | neither = new grant,
  //       onChange(inst).
  function create(opts) {
    prune();
    var inst = {
      id: 'gbr' + (++seq),
      ctx: Object.assign({}, opts.ctx || {}),
      onChange: opts.onChange || function () {},
      legacy: null, st: null, status: '', host: null
    };
    if (opts.policy) inst.st = stateFromPolicy(opts.policy, inst.ctx);
    else if (opts.legacy) { inst.legacy = { refs: (opts.legacy.ref_allowlist || []).slice() }; inst.st = newState(TEMPLATES.agent.rules(), inst.ctx); }
    else inst.st = newState(TEMPLATES.agent.rules(), inst.ctx);
    instances[inst.id] = inst;
    return inst;
  }
  function relevant(inst) { var c = inst.ctx; return !!(c.push || c.prWrite || c.dispatch); }
  function problems(inst) { return (inst.legacy || !relevant(inst)) ? [] : stateProblems(inst.st, inst.ctx); }
  // null for a legacy repository (keep ref_allowlist), else the config to store.
  function valueOf(inst) { return inst.legacy ? null : stateValue(inst.st, inst.ctx); }
  function legacyOpts(inst) {
    var c = inst.ctx;
    return { push: !!c.push, dispatch: !!c.dispatch, prWrite: !!c.prWrite, builder: !!c.builder, refs: inst.legacy ? inst.legacy.refs : [], defaultBranch: c.defaultBranch };
  }
  function changed(inst) { inst.onChange(inst); }

  /* -------------------------- view -------------------------- */
  function verdict(ok, text) {
    return dw.html`<li class="gbr-verdict ${ok ? 'gbr-ok' : 'gbr-no'}"><span class="gbr-verdict-word">${ok ? 'Allowed' : 'Refused'}</span><span>${text}</span></li>`;
  }
  function ruleLabel(inst, r, i) {
    var last = i === inst.st.rules.length - 1;
    if (i === 0) return { name: inst.ctx.defaultBranch || 'Default branch', sub: inst.ctx.defaultBranch ? 'default branch, follows a rename' : 'default branch (name not loaded)', mono: !!inst.ctx.defaultBranch };
    if (last) return { name: 'All other branches', sub: 'fallback', mono: false };
    return { name: r.branches, sub: '', mono: true };
  }
  function ruleView(inst, r, i) {
    var lab = ruleLabel(inst, r, i), last = i === inst.st.rules.length - 1, free = i > 0 && !last;
    var run = !!(r.run_workflows && inst.ctx.dispatch);
    var warn = run && r.role === 'agent';
    var seg = ROLES.map(function (role) {
      var on = r.role === role;
      return dw.html`<button type="button" role="radio" aria-checked="${on ? 'true' : 'false'}" class="gbr-seg-btn${on ? ' gbr-seg-on' : ''}" data-action="gbrRole" data-i="${i}" data-role="${role}" data-gbr-fk="${'role' + i + role}">${ROLE_LABEL[role]}</button>`;
    });
    var runTitle = inst.ctx.dispatch ? '' : 'Turn on Dispatch for this repository to run workflows.';
    return dw.html`<li class="gbr-rule">
      <div class="gbr-rule-main">
        <div class="gbr-rule-name"><span class="${lab.mono ? 'mono ' : ''}gbr-rule-label">${lab.name}</span>${lab.sub ? dw.html`<span class="gbr-sub">${lab.sub}</span>` : ''}</div>
        <div class="gbr-seg" role="radiogroup" aria-label="${'Role for ' + lab.name}">${seg}</div>
        <label class="gbr-run" title="${runTitle}"><input type="checkbox" data-on-change="gbrRun" data-i="${i}" data-gbr-fk="${'run' + i}" ${run ? dw.raw('checked') : ''} ${inst.ctx.dispatch ? '' : dw.raw('disabled')}> <span>Run workflows here</span></label>
        ${free ? dw.html`<button type="button" class="gbr-x" data-action="gbrRemove" data-i="${i}" aria-label="${'Remove rule ' + r.branches}" data-gbr-fk="${'rm' + i}">&times;</button>` : dw.html`<span class="gbr-x-gap" aria-hidden="true"></span>`}
      </div>
      ${warn ? dw.html`<div class="gbr-warn" role="note">The agent writes this branch and can start workflows on it, so it controls the code those workflows run, with their secrets. Keep this only for workflows without deploy secrets.</div>` : ''}
    </li>`;
  }
  function testView(inst) {
    var test = stripHeads(inst.st.test);
    if (!test) return dw.html`<p class="gbr-sub">Type a branch name to see which rule applies.</p>`;
    var eff = effectiveRules(inst.st, inst.ctx);
    var assumed = !inst.ctx.defaultBranch;
    var hit = selectRule(eff, inst.ctx.defaultBranch || 'main', test);
    var role = hit ? hit.role : 'read';
    var which = !hit ? 'no rule (falls back to read only)' : (hit.branches === DEFAULT ? 'default branch' : (hit.branches === OTHER ? 'All other branches' : hit.branches));
    var a = allowedActions(role, hit && hit.run_workflows);
    return dw.html`<p class="gbr-hit">Rule <span class="mono">${which}</span> &rarr; <strong>${ROLE_LABEL[role]}</strong></p>
      ${assumed ? dw.html`<p class="gbr-sub">The default branch name is not known here, so "main" is assumed.</p>` : ''}
      <ul class="gbr-verdicts" aria-label="What the agent may do on this branch">${[
        verdict(a.push, 'Push or commit to it'), verdict(a.createDelete, 'Create or delete it'),
        verdict(a.openPR, 'Open a PR into it'), verdict(a.runWorkflows, 'Start workflows on it')
      ]}</ul>`;
  }
  function view(inst) {
    var st = inst.st, ctx = inst.ctx;
    if (!relevant(inst)) return dw.html``;
    if (inst.legacy) {
      var diffs = legacyDifferences(legacyOpts(inst));
      var refs = inst.legacy.refs;
      return dw.html`<section class="gbr gbr-legacy" aria-label="Branch rules">
        <h4 class="gbr-h">Branch rules</h4>
        <div class="gbr-notice" role="note">
          <strong>${refs.length ? 'This repository uses the older "Allowed refs" list.' : 'This repository has no branch rules yet.'}</strong>
          <p class="gbr-sub">${refs.length ? dw.html`Allowed refs: <span class="mono">${refs.join(', ')}</span>.` : 'It works as before: writes are not limited by branch.'} Converting fills the editor; nothing changes until you save. Differences:</p>
          <ul class="gbr-diffs">${diffs.map(function (d) { return dw.html`<li>${d}</li>`; })}</ul>
          <button type="button" class="btn" data-action="gbrConvert" data-gbr-fk="convert">Convert to branch rules</button>
        </div>
      </section>`;
    }
    var probs = stateProblems(st, ctx);
    var tagsOn = st.tagMode === 'some';
    var samples = [ctx.defaultBranch || 'main', 'agent/fix-login', 'release/2.4', 'feature/x'].map(function (b) {
      return dw.html`<button type="button" class="gbr-chip mono" data-action="gbrSample" data-b="${b}">${b}</button>`;
    });
    var tplButtons = TEMPLATE_ORDER.map(function (k) {
      return dw.html`<button type="button" class="gbr-tpl" data-action="gbrTemplate" data-tpl="${k}" data-gbr-fk="${'tpl' + k}">${TEMPLATES[k].label}</button>`;
    });
    var Y = 'yes', N = 'no';
    var matrix = ROLES.map(function (role) {
      var a = allowedActions(role, false);
      return dw.html`<tr><th scope="row">${ROLE_LABEL[role]}</th><td>${a.push ? Y : N}</td><td>${a.createDelete ? Y : N}</td><td>${a.openPR ? Y : N}</td><td>per rule</td></tr>`;
    });
    return dw.html`<section class="gbr" aria-label="Branch rules">
      <h4 class="gbr-h">Branch rules</h4>
      <p class="gbr-sub">Give each branch a role, like GitHub branch protection. It narrows what the capabilities above allow, whichever API the agent uses. The most specific pattern wins.${inst.status ? dw.html` <strong class="gbr-status" role="status">${inst.status}</strong>` : ''}</p>
      <div class="gbr-tpls"><span class="gbr-sub">Start from:</span>${tplButtons}</div>
      <ul class="gbr-rules" aria-label="Branch rules, most specific first">${st.rules.map(function (r, i) { return ruleView(inst, r, i); })}</ul>
      <div class="gbr-add">
        <input type="text" class="mono" data-gbr-draft data-gbr-fk="draft" data-on-change="gbrDraft" aria-label="New branch pattern" placeholder="e.g. release/**" value="${st.draft}" spellcheck="false" autocomplete="off" autocapitalize="off">
        <button type="button" class="btn" data-action="gbrAdd" data-gbr-fk="add">Add branch rule</button>
        <span class="gbr-sub">* one level &middot; ** any depth</span>
      </div>
      ${inst.addError ? dw.html`<p class="gbr-add-error" role="alert">${inst.addError}</p>` : ''}
      <div class="gbr-group">
        <h5 class="gbr-h2">Tags and releases</h5>
        <div class="gbr-tags">
          <div class="gbr-seg" role="radiogroup" aria-label="Tag creation">
            <button type="button" role="radio" aria-checked="${tagsOn ? 'false' : 'true'}" class="gbr-seg-btn${tagsOn ? '' : ' gbr-seg-on'}" data-action="gbrTagMode" data-mode="none" data-gbr-fk="tagnone">No new tags</button>
            <button type="button" role="radio" aria-checked="${tagsOn ? 'true' : 'false'}" class="gbr-seg-btn${tagsOn ? ' gbr-seg-on' : ''}" data-action="gbrTagMode" data-mode="some" data-gbr-fk="tagsome">May create tags matching</button>
          </div>
          ${tagsOn ? dw.html`<input type="text" class="mono" data-on-change="gbrTagText" aria-label="Tag patterns, comma separated" placeholder="v*, release-*" value="${st.tagText}" spellcheck="false" autocomplete="off" autocapitalize="off" data-gbr-fk="tagtext">` : ''}
        </div>
        <p class="gbr-sub">Applies to pushing tags and to releases (a release creates its tag). Existing tags can never be moved or deleted.</p>
      </div>
      <div class="gbr-group">
        <h5 class="gbr-h2">Pull requests</h5>
        <label class="gbr-opt"><input type="checkbox" data-on-change="gbrMerge" data-gbr-fk="merge" ${st.merge && ctx.prWrite ? dw.raw('checked') : ''} ${ctx.prWrite ? '' : dw.raw('disabled')}> <span><span>Merge pull requests</span><span class="gbr-sub">Merge and update-branch on any pull request this token can see.${ctx.prWrite ? '' : ' Needs pull request write.'}</span></span></label>
        <label class="gbr-opt"><input type="checkbox" data-on-change="gbrApprove" data-gbr-fk="approve" ${st.approve && ctx.prWrite ? dw.raw('checked') : ''} ${ctx.prWrite ? '' : dw.raw('disabled')}> <span><span>Submit approving reviews</span><span class="gbr-sub">Off: comments and change requests still work, only "Approve" is refused.${ctx.prWrite ? '' : ' Needs pull request write.'}</span></span></label>
      </div>
      <div class="gbr-group">
        <h5 class="gbr-h2">Try a branch</h5>
        <input type="text" class="mono" data-gbr-test data-on-input="gbrTest" aria-label="Branch to test" value="${st.test}" spellcheck="false" autocomplete="off" autocapitalize="off">
        <div class="gbr-chips">${samples}</div>
        <div class="gbr-test-out" data-gbr-out aria-live="polite">${testView(inst)}</div>
      </div>
      <details class="gbr-matrix"><summary>What each role allows</summary>
        <div class="table-wrap"><table><thead><tr><th scope="col">Role</th><th scope="col">Push, commit</th><th scope="col">Create, delete</th><th scope="col">Open PR into</th><th scope="col">Run workflows</th></tr></thead><tbody>${matrix}</tbody></table></div>
      </details>
      ${probs.length ? dw.html`<ul class="gbr-problems" role="alert">${probs.map(function (p) { return dw.html`<li>${p}</li>`; })}</ul>` : ''}
    </section>`;
  }

  function render(inst) {
    if (!inst.host || !dw) return;
    // setHTML removes the focused input, which blurs it and fires `change`
    // synchronously; a handler that renders again from inside setHTML would
    // remove nodes of the tree being replaced. Defer such a render until this
    // one has finished.
    if (inst.rendering) { inst.renderAgain = true; return; }
    inst.rendering = true;
    try {
      for (var pass = 0; pass < 3; pass++) {
        inst.renderAgain = false;
        renderOnce(inst);
        if (!inst.renderAgain) break;
      }
    } finally {
      inst.rendering = false;
      inst.renderAgain = false;
    }
  }
  function renderOnce(inst) {
    var host = inst.host, active = document.activeElement, fk = '';
    if (active && host.contains(active) && active.getAttribute) fk = active.getAttribute('data-gbr-fk') || '';
    dw.setHTML(host, view(inst));
    host.hidden = !relevant(inst);
    if (fk) {
      var el = host.querySelector('[data-gbr-fk="' + fk.replace(/"/g, '') + '"]');
      if (el && !el.disabled) el.focus();
    }
  }
  function mount(host, inst) {
    inst.host = host;
    host.setAttribute('data-gbr-id', inst.id);
    if (!host.__gbrKeys) {
      host.__gbrKeys = true;
      // Enter inside these inputs must not submit a surrounding form.
      host.addEventListener('keydown', function (e) {
        if (e.key !== 'Enter' || !e.target.matches || !e.target.matches('input[type="text"]')) return;
        e.preventDefault();
        var i = instOf(e.target);
        if (!i) return;
        if (e.target.hasAttribute('data-gbr-draft')) { i.st.draft = e.target.value; addRule(i); }
        else if (e.target.hasAttribute('data-on-change')) e.target.dispatchEvent(new Event('change', { bubbles: true }));
      });
    }
    render(inst);
    return inst;
  }
  function setCtx(inst, patch, silent) {
    Object.assign(inst.ctx, patch);
    if (!silent) render(inst);
  }
  function instOf(el) {
    var h = el && el.closest ? el.closest('[data-gbr-id]') : null;
    return h ? instances[h.getAttribute('data-gbr-id')] || null : null;
  }
  function forHost(host) { return host ? instances[host.getAttribute('data-gbr-id')] || null : null; }

  function addRule(inst) {
    var raw = inst.st.draft;
    var p = stripHeads(raw);
    if (!p) return;
    var err = patternError(p, false);
    if (!err && p === OTHER) err = '"**" already has the last row (All other branches).';
    if (!err && inst.st.rules.some(function (r) { return stripHeads(r.branches) === p; })) err = '"' + p + '" is already a rule.';
    if (err) { inst.st.draft = p; inst.status = ''; inst.addError = err; render(inst); changed(inst); return; }
    inst.addError = '';
    inst.st.rules.splice(inst.st.rules.length - 1, 0, rule(p, 'protected', false));
    inst.st.draft = '';
    inst.status = '';
    render(inst); changed(inst);
  }

  /* ------------------ delegated actions (registered once) ------------------ */
  if (dw && dw.actions) {
    dw.actions.gbrTemplate = function (el) {
      var inst = instOf(el), t = inst && TEMPLATES[el.dataset.tpl];
      if (!t) return;
      inst.st.rules = fixedLayout(t.rules(), inst.ctx.defaultBranch);
      inst.st.tagMode = 'none'; inst.st.tagText = '';
      inst.status = '';
      render(inst); changed(inst);
    };
    dw.actions.gbrRole = function (el) {
      var inst = instOf(el), r = inst && inst.st.rules[+el.dataset.i];
      if (!r || ROLES.indexOf(el.dataset.role) < 0) return;
      r.role = el.dataset.role;
      inst.status = '';
      render(inst); changed(inst);
    };
    dw.actions.gbrRun = function (el) {
      var inst = instOf(el), r = inst && inst.st.rules[+el.dataset.i];
      if (!r) return;
      r.run_workflows = !!el.checked;
      inst.status = '';
      render(inst); changed(inst);
    };
    dw.actions.gbrRemove = function (el) {
      var inst = instOf(el), i = +el.dataset.i;
      if (!inst || !(i > 0 && i < inst.st.rules.length - 1)) return;
      inst.st.rules.splice(i, 1);
      inst.status = '';
      render(inst); changed(inst);
      var add = inst.host.querySelector('[data-gbr-draft]'); if (add) add.focus();
    };
    dw.actions.gbrDraft = function (el) {
      var inst = instOf(el);
      // A change fired by the blur of the input being replaced carries the
      // text that render already holds; it must neither re-render nor clear
      // the error that render is showing.
      if (!inst || inst.rendering) return;
      var edited = el.value !== inst.st.draft;
      inst.st.draft = el.value;
      // The message belongs to the draft that produced it.
      if (inst.addError && edited) { inst.addError = ''; render(inst); }
    };
    dw.actions.gbrAdd = function (el) {
      var inst = instOf(el);
      if (!inst) return;
      var input = inst.host.querySelector('[data-gbr-draft]');
      if (input) inst.st.draft = input.value;
      addRule(inst);
    };
    dw.actions.gbrTagMode = function (el) {
      var inst = instOf(el);
      if (!inst) return;
      inst.st.tagMode = el.dataset.mode === 'some' ? 'some' : 'none';
      inst.status = '';
      render(inst); changed(inst);
    };
    dw.actions.gbrTagText = function (el) {
      var inst = instOf(el);
      if (!inst) return;
      inst.st.tagText = el.value;
      render(inst); changed(inst);
    };
    dw.actions.gbrMerge = function (el) {
      var inst = instOf(el);
      if (!inst) return;
      inst.st.merge = !!el.checked;
      render(inst); changed(inst);
    };
    dw.actions.gbrApprove = function (el) {
      var inst = instOf(el);
      if (!inst) return;
      inst.st.approve = !!el.checked;
      render(inst); changed(inst);
    };
    dw.actions.gbrTest = function (el) {
      var inst = instOf(el);
      if (!inst) return;
      inst.st.test = el.value;
      var out = inst.host.querySelector('[data-gbr-out]');
      if (out) dw.setHTML(out, testView(inst));
    };
    dw.actions.gbrSample = function (el) {
      var inst = instOf(el);
      if (!inst) return;
      inst.st.test = el.dataset.b;
      var input = inst.host.querySelector('[data-gbr-test]');
      if (input) input.value = el.dataset.b;
      var out = inst.host.querySelector('[data-gbr-out]');
      if (out) dw.setHTML(out, testView(inst));
    };
    dw.actions.gbrConvert = function (el) {
      var inst = instOf(el);
      if (!inst || !inst.legacy) return;
      var o = legacyOpts(inst);
      inst.st = convertLegacy(o);
      inst.legacy = null;
      inst.status = 'Converted. Review the rules, then save.';
      render(inst); changed(inst);
      var first = inst.host.querySelector('[data-gbr-fk^="role"]'); if (first) first.focus();
    };
  }

  window.GBR = {
    ROLES: ROLES, ROLE_LABEL: ROLE_LABEL, DEFAULT: DEFAULT, OTHER: OTHER, TEMPLATES: TEMPLATES,
    LIMITS: { rules: MAX_RULES, tags: MAX_TAGS, pattern: MAX_LEN },
    globMatch: globMatch, selectRule: selectRule, tagAllowed: tagAllowed, allowedActions: allowedActions,
    patternError: patternError, tagPatternError: tagPatternError, parseTagList: parseTagList,
    newState: newState, stateFromPolicy: stateFromPolicy, stateProblems: stateProblems, stateValue: stateValue,
    convertLegacy: convertLegacy, legacyDifferences: legacyDifferences, defaultValue: defaultValue,
    fixedLayout: fixedLayout,
    create: create, mount: mount, forHost: forHost, setCtx: setCtx,
    relevant: relevant, problems: problems, value: valueOf, render: render
  };
})();
