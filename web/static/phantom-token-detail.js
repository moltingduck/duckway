/*
 * phantom-token-detail.js - Phantom token detail page.
 * Depends on dw.js, phantom-common.js (PT) and phantom-acl.js (PTACL).
 * All markup goes through DW.html / DW.setHTML; no inline handlers.
 */
(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var root = $('pt-root');
  if (!root) return;
  var D = JSON.parse($('pt-data').textContent);
  var ID = D.id;

  var S = {
    orig: null,        // {config, approval, ttl}
    version: '1', provider: D.service.name, rules: [], extra: {}, v2: null,
    jsonText: '', jsonErr: '',
    mode: 'rules',
    approval: false, ttlVal: 24, ttlUnit: 'hours',
    hit: -1, saving: false, lastTest: null, testSeq: 0
  };

  /* ---------- helpers ---------- */
  function ttlMinutes() {
    var v = parseInt(S.ttlVal, 10);
    if (!(v >= 1)) return 0;
    return S.ttlUnit === 'hours' ? v * 60 : v;
  }
  function effTTL(m) { return m > 0 ? m : 1440; }
  function setTTLFromMinutes(m) {
    m = effTTL(m);
    if (m % 60 === 0) { S.ttlVal = m / 60; S.ttlUnit = 'hours'; } else { S.ttlVal = m; S.ttlUnit = 'minutes'; }
  }
  function applyParsed(p) {
    S.version = p.version; S.rules = p.rules; S.extra = p.extra; S.v2 = p.v2;
    if (p.provider) S.provider = p.provider;
  }
  function draftConfig() { return S.jsonText.trim() === '' ? '' : S.jsonText; }
  function ruleProblems() {
    var n = 0;
    S.rules.forEach(function (r) { if (PTACL.ruleProblem(r)) n++; });
    return n;
  }
  function isDirty() {
    return PTACL.normalize(S.jsonText) !== PTACL.normalize(S.orig.config) ||
      S.approval !== S.orig.approval ||
      (S.approval && effTTL(ttlMinutes()) !== effTTL(S.orig.ttl));
  }
  function setText(id, t) { var e = $(id); if (e) e.textContent = t; }

  /* ---------- header ---------- */
  function renderHeader() {
    var src = $('pt-source');
    if (D.source.kind === 'bundle') {
      src.className = 'cl-pill cl-pill-info';
      src.textContent = 'From key bundle' + (D.source.name ? ' ' + D.source.name : '');
    } else {
      src.className = 'cl-pill cl-pill-mute';
      src.textContent = 'Manual';
    }
    var cred;
    if (D.credential.kind === 'pool') cred = DW.html`<span>${D.credential.name || 'Key pool'} <span class="cl-pill cl-pill-info">Pool</span></span>`;
    else if (D.credential.kind === 'key') cred = DW.html`<span>${D.credential.name || 'Credential'}</span>`;
    else cred = DW.html`<span class="text-muted">no credential assigned</span>`;
    DW.setHTML($('pt-sub'), DW.html`Phantom token for <a href="${'/admin/clients/' + encodeURIComponent(D.client.id)}">${D.client.name}</a> &rarr; ${cred} <span class="text-muted">&middot; ${D.service.display_name}</span> <span class="pt-masked mono" title="Masked phantom token">${D.masked_token}</span>`);
    if (!D.is_active) $('pt-inactive').hidden = false;
  }

  /* ---------- layers ---------- */
  function layerChip(label, name, sum, href) {
    var inner = DW.html`<span class="pt-layer-k">${label}</span><span class="pt-layer-n">${name}</span><span class="pt-layer-s pt-sum-${sum.kind}">${sum.text}</span>`;
    return href ? DW.html`<a class="pt-layer" href="${href}">${inner}</a>` : DW.html`<span class="pt-layer pt-layer-here" aria-current="true">${inner}</span>`;
  }
  function renderLayers() {
    var credName = D.credential.kind === 'pool' ? (D.credential.name || 'Key pool') : (D.credential.name || 'Credential');
    var credSum = D.credential.kind === 'pool' ? { kind: 'none', text: 'rules of the key picked per request' } : PTACL.summarize(D.credential.acl);
    var credHref = D.credential.kind === 'pool' ? '/admin/key-pools' : '/admin/credentials';
    DW.setHTML($('pt-layers'), DW.html`${layerChip('Service', D.service.display_name, PTACL.summarize(D.service.default_acl), '/admin/services')}<span class="pt-arrow" aria-hidden="true">&rarr;</span>${layerChip('Credential', credName, credSum, credHref)}<span class="pt-arrow" aria-hidden="true">&rarr;</span>${layerChip('This token', D.env_name, S.jsonErr ? { kind: 'invalid', text: 'invalid JSON' } : PTACL.summarize(draftConfig()), '')}`);
  }

  /* ---------- rules tab ---------- */
  function methodChip(r, i, m) {
    var on = r.methods.indexOf(m) !== -1;
    var label = m === '*' ? 'Any' : m;
    return DW.html`<button type="button" class="pt-chip${on ? ' pt-chip-on' : ''}" aria-pressed="${on ? 'true' : 'false'}" data-action="ptMethod" data-i="${i}" data-m="${m}" data-fk="${'r' + i + 'm' + m}">${label}</button>`;
  }
  function ruleRow(r, i) {
    var last = S.rules.length - 1;
    var hit = S.hit === i ? ' pt-hit' : '';
    var ctl = DW.html`<div class="pt-rule-ctl"><button type="button" class="btn btn-sm" data-action="ptMove" data-i="${i}" data-d="-1" data-fk="${'r' + i + 'up'}" aria-label="${'Move rule ' + (i + 1) + ' up'}"${i === 0 ? ' disabled' : ''}>&uarr;</button><button type="button" class="btn btn-sm" data-action="ptMove" data-i="${i}" data-d="1" data-fk="${'r' + i + 'dn'}" aria-label="${'Move rule ' + (i + 1) + ' down'}"${i === last ? ' disabled' : ''}>&darr;</button><button type="button" class="btn btn-sm btn-dz" data-action="ptRemove" data-i="${i}" aria-label="${'Remove rule ' + (i + 1)}">Remove</button></div>`;
    if (r.t === 'c') {
      return DW.html`<li class="pt-rule pt-rule-custom${hit}" data-i="${i}"><span class="pt-rule-n" aria-hidden="true">${i + 1}</span><div class="pt-rule-body"><div class="pt-rule-line"><span class="cl-pill cl-pill-warn">Custom rule</span><strong class="pt-rule-name">${r.raw && r.raw.name ? r.raw.name : 'Unnamed rule'}</strong></div><p class="pt-rule-desc">${PTACL.describeCustom(r.raw)}.</p><p class="pt-hint">The builder can not show this rule. It is kept exactly as written when you save. <button type="button" class="pt-linkbtn" data-action="ptTab" data-tab="json">Edit in JSON</button></p></div>${ctl}</li>`;
    }
    var prob = PTACL.ruleProblem(r), adv = PTACL.pathAdvice(r.path);
    var methods = PTACL.METHODS.concat(['*']).map(function (m) { return methodChip(r, i, m); });
    return DW.html`<li class="pt-rule${hit}" data-i="${i}"><span class="pt-rule-n" aria-hidden="true">${i + 1}</span><div class="pt-rule-body">
      <div class="pt-rule-line">
        <label class="pt-fld"><span class="pt-fld-k">Effect</span><select class="pt-effect pt-eff-${r.effect}" data-on-change="ptEffect" data-i="${i}" data-fk="${'r' + i + 'eff'}" aria-label="${'Rule ' + (i + 1) + ' effect'}"><option value="allow"${r.effect === 'allow' ? ' selected' : ''}>Allow</option><option value="deny"${r.effect === 'deny' ? ' selected' : ''}>Deny</option></select></label>
        <div class="pt-fld pt-fld-methods" role="group" aria-label="${'Rule ' + (i + 1) + ' methods'}"><span class="pt-fld-k">Methods</span><div class="pt-chips">${methods}</div></div>
      </div>
      <label class="pt-fld pt-fld-path"><span class="pt-fld-k">Path pattern</span><input type="text" class="mono" value="${r.path}" data-on-input="ptPath" data-i="${i}" data-fk="${'r' + i + 'path'}" spellcheck="false" autocomplete="off" autocapitalize="off" aria-label="${'Rule ' + (i + 1) + ' path pattern'}"></label>
      <p class="pt-hint pt-bad" role="alert" data-prob="${i}"${prob ? '' : ' hidden'}>${prob}</p>
      <p class="pt-hint pt-warnnote" data-adv="${i}"${adv ? '' : ' hidden'}>${adv}</p>
    </div>${ctl}</li>`;
  }
  function renderRules() {
    var active = document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.fk : '';
    var box = $('pt-rules');
    if (S.v2) {
      DW.setHTML(box, DW.html`<div class="pt-notice" role="status"><strong>GitHub repository grants</strong><p>This token uses repository grants, which the gateway turns into path rules itself. Edit them in the GitHub repositories card, or in the JSON tab. The rule builder is not available for this format.</p></div>`);
      $('pt-add-rule').disabled = true;
      Array.prototype.forEach.call(document.querySelectorAll('[data-preset]'), function (b) { b.disabled = true; });
      $('pt-clear-rules').disabled = true;
      $('pt-default-note').hidden = true;
      return;
    }
    $('pt-add-rule').disabled = false;
    Array.prototype.forEach.call(document.querySelectorAll('[data-preset]'), function (b) { b.disabled = false; });
    $('pt-clear-rules').disabled = !S.rules.length;
    $('pt-default-note').hidden = false;
    if (!S.rules.length) {
      DW.setHTML(box, DW.html`<div class="pt-empty"><strong>No rules on this token</strong><p class="text-muted">Every request this token makes is allowed by this layer. The service and credential rules above still apply. Add a rule or start from a preset.</p></div>`);
    } else {
      DW.setHTML(box, DW.html`<ol class="pt-rulelist" aria-label="Access rules in evaluation order">${S.rules.map(ruleRow)}</ol>`);
    }
    if (active) {
      var el = box.querySelector('[data-fk="' + active + '"]');
      if (el && !el.disabled) el.focus();
    }
  }

  /* ---------- state sync ---------- */
  function rebuildJSON() {
    S.jsonText = PTACL.build({ version: S.version, provider: S.provider, rules: S.rules, extra: S.extra, v2: S.v2 });
    S.jsonErr = '';
    $('pt-json').value = S.jsonText;
    showJSONError();
  }
  function showJSONError() {
    var e = $('pt-json-err');
    e.textContent = S.jsonErr;
    e.hidden = !S.jsonErr;
    $('pt-json').setAttribute('aria-invalid', S.jsonErr ? 'true' : 'false');
    $('pt-json-ok').hidden = !!S.jsonErr;
  }
  function refresh() {
    var dirty = isDirty(), probs = ruleProblems();
    $('pt-dirty').hidden = !dirty;
    var canSave = dirty && !S.jsonErr && !probs && !S.saving && !approvalProblem();
    Array.prototype.forEach.call(document.querySelectorAll('[data-pt-save]'), function (b) { b.disabled = !canSave; });
    Array.prototype.forEach.call(document.querySelectorAll('[data-pt-discard]'), function (b) { b.disabled = !dirty || S.saving; });
    var note = '';
    if (dirty && S.jsonErr) note = 'Fix the JSON error to save.';
    else if (dirty && probs) note = 'Complete ' + probs + (probs === 1 ? ' rule' : ' rules') + ' to save.';
    else if (dirty && approvalProblem()) note = approvalProblem();
    Array.prototype.forEach.call(document.querySelectorAll('[data-pt-note]'), function (n) { n.textContent = note; });
    $('pt-test-go').disabled = !!S.jsonErr;
    $('pt-test-blocked').hidden = !S.jsonErr;
    renderLayers();
    renderGitHub();
  }
  function approvalProblem() {
    if (S.approval && !(parseInt(S.ttlVal, 10) >= 1)) return 'Enter how long an approval stays valid.';
    return '';
  }
  function edited() { S.hit = -1; clearTest(); rebuildJSON(); refresh(); }

  /* ---------- rule actions ---------- */
  DW.actions.ptAddRule = function () {
    if (S.v2) return;
    S.rules.push(PTACL.newRule());
    edited(); renderRules();
    var last = $('pt-rules').querySelector('[data-fk="r' + (S.rules.length - 1) + 'path"]');
    if (last) last.focus();
  };
  DW.actions.ptRemove = function (el) {
    S.rules.splice(+el.dataset.i, 1);
    edited(); renderRules();
  };
  DW.actions.ptMove = function (el) {
    var i = +el.dataset.i, j = i + (+el.dataset.d);
    if (j < 0 || j >= S.rules.length) return;
    var t = S.rules[i]; S.rules[i] = S.rules[j]; S.rules[j] = t;
    edited();
    var key = 'r' + j + (+el.dataset.d < 0 ? 'up' : 'dn');
    renderRules();
    var b = $('pt-rules').querySelector('[data-fk="' + key + '"]');
    if (b && !b.disabled) b.focus(); else {
      var other = $('pt-rules').querySelector('[data-fk="r' + j + (+el.dataset.d < 0 ? 'dn' : 'up') + '"]');
      if (other) other.focus();
    }
  };
  DW.actions.ptMethod = function (el) {
    var r = S.rules[+el.dataset.i], m = el.dataset.m;
    if (!r || r.t !== 's') return;
    if (m === '*') {
      r.methods = r.methods.indexOf('*') === -1 ? ['*'] : [];
    } else {
      var ms = r.methods.filter(function (x) { return x !== '*'; });
      var at = ms.indexOf(m);
      if (at === -1) ms.push(m); else ms.splice(at, 1);
      ms.sort(function (x, y) { return PTACL.METHODS.indexOf(x) - PTACL.METHODS.indexOf(y); });
      r.methods = ms;
    }
    edited(); renderRules();
  };
  DW.actions.ptEffect = function (el) {
    var r = S.rules[+el.dataset.i];
    if (!r || r.t !== 's') return;
    r.effect = el.value === 'deny' ? 'deny' : 'allow';
    edited(); renderRules();
  };
  DW.actions.ptPath = function (el) {
    var i = +el.dataset.i, r = S.rules[i];
    if (!r || r.t !== 's') return;
    r.path = el.value;
    var row = el.closest('.pt-rule');
    var prob = PTACL.ruleProblem(r), adv = PTACL.pathAdvice(r.path);
    var pe = row.querySelector('[data-prob]'), ae = row.querySelector('[data-adv]');
    pe.textContent = prob; pe.hidden = !prob;
    ae.textContent = adv; ae.hidden = !adv;
    edited();
  };
  DW.actions.ptPreset = function (el) {
    var p = PTACL.PRESETS[el.dataset.preset];
    if (!p) return;
    var replaced = S.rules.length;
    S.version = '1'; S.v2 = null;
    S.rules = p.rules();
    edited(); renderRules();
    setText('pt-rules-status', p.label + ' applied' + (replaced ? '; it replaced ' + replaced + (replaced === 1 ? ' rule' : ' rules') : '') + '. Use Discard to undo.');
  };
  DW.actions.ptClearRules = function () {
    var n = S.rules.length;
    S.rules = [];
    edited(); renderRules();
    setText('pt-rules-status', n ? 'Removed ' + n + (n === 1 ? ' rule' : ' rules') + '. Use Discard to undo.' : '');
  };

  /* ---------- tabs ---------- */
  function setTab(tab, force) {
    if (tab === 'rules' && S.jsonErr && !force) {
      setText('pt-json-err', S.jsonErr + ' Fix the JSON before returning to the rule list.');
      $('pt-json').focus();
      return;
    }
    S.mode = tab;
    ['rules', 'json'].forEach(function (t) {
      var b = $('pt-tab-' + t), on = t === tab;
      b.setAttribute('aria-selected', on ? 'true' : 'false');
      b.tabIndex = on ? 0 : -1;
      $('pt-panel-' + t).hidden = !on;
    });
    if (tab === 'rules') renderRules();
  }
  DW.actions.ptTab = function (el) { setTab(el.dataset.tab); };
  $('pt-tablist').addEventListener('keydown', function (e) {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    e.preventDefault();
    var next = S.mode === 'rules' ? 'json' : 'rules';
    setTab(next);
    if (S.mode === next) $('pt-tab-' + next).focus();
  });

  /* ---------- JSON tab ---------- */
  DW.actions.ptJsonInput = function (el) {
    S.jsonText = el.value;
    var p = PTACL.parse(el.value);
    if (p.ok) { S.jsonErr = ''; applyParsed(p); } else { S.jsonErr = p.err; }
    S.hit = -1; clearTest();
    showJSONError();
    refresh();
  };
  DW.actions.ptJsonFormat = function () {
    if (S.jsonErr) return;
    var p = PTACL.parse(S.jsonText);
    if (!p.ok || p.empty) return;
    S.jsonText = JSON.stringify(JSON.parse(S.jsonText), null, 2);
    $('pt-json').value = S.jsonText;
    refresh();
  };

  /* ---------- test a request ---------- */
  function clearTest() {
    S.lastTest = null; S.testSeq++;
    var box = $('pt-test-out');
    if (box.firstChild) {
      DW.setHTML(box, DW.html``);
    }
  }
  var LAYER_NAMES = { service: 'Service', credential: 'Credential', token: 'This token' };
  function layerLine(l) {
    var pill;
    if (l.result === 'skipped') pill = DW.html`<span class="cl-pill cl-pill-mute">Not checked</span>`;
    else if (l.result === 'deny') pill = DW.html`<span class="cl-pill cl-pill-bad">Denies</span>`;
    else pill = DW.html`<span class="cl-pill cl-pill-ok">Allows</span>`;
    var detail;
    if (l.result === 'deny') detail = l.reason;
    else if (l.result === 'skipped') detail = l.note;
    else if (!l.configured) detail = 'No rules, so nothing is restricted here.';
    else if (l.default) detail = 'No rule matched, so the request is allowed here.';
    else detail = 'Matched rule ' + (l.rule_index + 1) + (l.rule_name ? ' "' + l.rule_name + '"' : '') + '.';
    return DW.html`<tr><th scope="row">${LAYER_NAMES[l.layer] || l.layer}${l.label ? DW.html` <span class="text-muted">${l.label}</span>` : ''}</th><td>${pill}</td><td class="pt-layer-detail">${detail}${l.note && l.result !== 'skipped' ? DW.html` <span class="text-muted">${l.note}</span>` : ''}</td></tr>`;
  }
  function renderTest(r) {
    var allow = r.decision === 'allow';
    var headline;
    if (!allow) {
      var who = r.decided_by === 'token' ? 'this token' : (r.decided_by === 'service' ? 'the service rules' : 'the credential rules');
      headline = 'Denied by ' + (r.rule_index >= 0 ? 'rule ' + (r.rule_index + 1) + ' on ' : '') + who + '.';
      var others = r.layers.filter(function (l) { return l.layer !== r.decided_by && l.result === 'allow'; });
      if (others.length) headline += ' The ' + others.map(function (l) { return LAYER_NAMES[l.layer].toLowerCase(); }).join(' and ') + ' layer' + (others.length > 1 ? 's' : '') + ' would have allowed it.';
    } else {
      headline = r.decided_by === 'default' ? 'Allowed. No rule matched, so the request is allowed by default.' :
        'Allowed by ' + (r.rule_index >= 0 ? 'rule ' + (r.rule_index + 1) + ' on ' : '') + (r.decided_by === 'token' ? 'this token' : (r.decided_by === 'service' ? 'the service rules' : 'the credential rules')) + '.';
    }
    var approval = r.approval.required ? DW.html`<p class="pt-hint">Approval: an admin must approve this token before the request is sent upstream (valid for ${fmtTTL(r.approval.ttl_minutes)} once approved).</p>` : DW.html`<p class="pt-hint">Approval: not required.</p>`;
    var body = r.needs_body ? DW.html`<p class="pt-hint">A matching rule has body constraints. Add a request body below for a complete answer.</p>` : '';
    DW.setHTML($('pt-test-out'), DW.html`<div class="pt-result ${allow ? 'pt-result-allow' : 'pt-result-deny'}" role="status"><p class="pt-result-head"><span class="cl-pill ${allow ? 'cl-pill-ok' : 'cl-pill-bad'}">${allow ? 'Allowed' : 'Denied'}</span> <span class="mono">${r.method} ${r.path}</span></p><p>${headline}</p><div class="table-wrap"><table class="pt-layers-table"><thead><tr><th scope="col">Layer</th><th scope="col">Result</th><th scope="col">Why</th></tr></thead><tbody>${r.layers.map(layerLine)}</tbody></table></div>${body}${approval}</div>`);
    S.hit = (r.decided_by === 'token' && r.rule_index >= 0 && !S.v2) ? r.rule_index : -1;
    if (S.mode === 'rules') renderRules();
  }
  function fmtTTL(m) {
    m = effTTL(m);
    if (m % 1440 === 0) return (m / 1440) + (m === 1440 ? ' day' : ' days');
    if (m % 60 === 0) return (m / 60) + (m === 60 ? ' hour' : ' hours');
    return m + ' minutes';
  }
  DW.actions.ptTest = function () {
    var errEl = $('pt-test-err');
    errEl.hidden = true;
    if (S.jsonErr) return;
    var method = $('pt-test-method').value, path = $('pt-test-path').value.trim();
    if (!path || path.charAt(0) !== '/') { errEl.textContent = 'Enter a path that starts with /, for example /repos/acme/app/issues.'; errEl.hidden = false; $('pt-test-path').focus(); return; }
    var seq = ++S.testSeq, btn = $('pt-test-go');
    btn.disabled = true;
    var body = { method: method, path: path, permission_config: draftConfig(), requires_approval: S.approval, approval_ttl_minutes: effTTL(ttlMinutes()) };
    var rb = $('pt-test-body').value;
    if (rb.trim()) body.body = rb;
    return PT.api('POST', '/api/placeholders/' + encodeURIComponent(ID) + '/acl/test', body).then(function (r) {
      if (seq !== S.testSeq) return;
      S.lastTest = r; renderTest(r);
    }).catch(function (e) {
      if (seq !== S.testSeq) return;
      errEl.textContent = e.message; errEl.hidden = false;
    }).then(function () { btn.disabled = !!S.jsonErr; });
  };

  /* ---------- GitHub card ---------- */
  var ghSeeded = false;
  function renderGitHub() {
    var card = $('pt-github');
    if (!card || !D.is_github) return;
    var grants = S.jsonErr ? [] : PT.gitGrantsFromACL(S.jsonText);
    if (!ghSeeded && !S.jsonErr) {
      ghSeeded = true;
      $('pt-gh-repos').value = grants.map(function (g) { return g.repo; }).join('\n');
      $('pt-gh-mode').value = grants.length ? PT.grantMode(grants.find(function (g) { return PT.grantMode(g) === 'cicd'; }) || grants.find(function (g) { return g.push; }) || grants[0]) : 'deploy';
      var first = grants[0] || {};
      $('pt-gh-wf').value = (first.workflow_allowlist || []).join(', ');
      $('pt-gh-refs').value = (first.ref_allowlist || []).join(', ');
      $('pt-gh-envs').value = (first.environment_allowlist || []).join(', ');
    }
    if (!grants.length) {
      DW.setHTML($('pt-gh-grants'), DW.html`<p class="text-muted text-sm">No repository grants in this token's rules.</p>`);
    } else {
      DW.setHTML($('pt-gh-grants'), DW.html`<ul class="pt-repos">${grants.map(function (g) {
        var caps = PT.grantCapabilities(g);
        return DW.html`<li><span class="mono pt-repo">${g.repo}</span><span class="pt-caps">${caps.length ? caps.map(function (c) { return DW.html`<span class="pt-cap">${c}</span>`; }) : DW.html`<span class="pt-cap pt-cap-none">no access</span>`}</span></li>`;
      })}</ul>`);
    }
    var sel = $('pt-gh-clone'), keep = sel.value;
    DW.setHTML(sel, DW.html`${grants.map(function (g) { return DW.html`<option value="${g.repo}">${g.repo}</option>`; })}`);
    if (grants.some(function (g) { return g.repo === keep; })) sel.value = keep;
    sel.disabled = !grants.length;
    var repo = sel.value || 'OWNER/REPO';
    var mode = grants.length ? PT.grantMode(grants.find(function (g) { return g.repo === repo; }) || grants[0]) : 'deploy';
    $('pt-gh-cmds').textContent = ['duckway git list', 'duckway git setup', '', '# pick a repository from `duckway git list`:', 'duckway git clone ' + repo, '', 'cd ' + repo.split('/').pop(), mode === 'deploy' ? 'git pull --ff-only' : 'git pull --ff-only && git push'].join('\n');
  }
  DW.actions.ptGhApply = function () {
    var repos = PT.parseGitRepoList($('pt-gh-repos').value), err = $('pt-gh-err');
    if (!repos.length) { err.textContent = 'Enter at least one repository as OWNER/REPO.'; err.hidden = false; return; }
    err.hidden = true;
    var cfg = PT.gitACLForRepos(repos, $('pt-gh-mode').value || 'deploy', $('pt-gh-wf').value, $('pt-gh-refs').value, $('pt-gh-envs').value);
    var p = PTACL.parse(JSON.stringify(cfg));
    if (!p.ok) { err.textContent = p.err; err.hidden = false; return; }
    applyParsed(p);
    edited(); renderRules();
    setText('pt-gh-status', 'Repository grants applied to the draft rules. Save to keep them.');
  };
  DW.actions.ptGhClone = function () { renderGitHub(); };

  /* ---------- approval card ---------- */
  function renderApproval() {
    $('pt-appr-off').checked = !S.approval;
    $('pt-appr-on').checked = S.approval;
    $('pt-ttl-val').value = S.ttlVal;
    $('pt-ttl-unit').value = S.ttlUnit;
    $('pt-ttl-val').disabled = !S.approval;
    $('pt-ttl-unit').disabled = !S.approval;
    $('pt-ttl-row').classList.toggle('pt-dim', !S.approval);
  }
  DW.actions.ptApproval = function (el) {
    S.approval = el.value === 'on';
    clearTest(); renderApproval(); refresh();
  };
  DW.actions.ptTTL = function () {
    S.ttlVal = $('pt-ttl-val').value;
    S.ttlUnit = $('pt-ttl-unit').value;
    refresh();
  };

  /* ---------- client card ---------- */
  function renderClientCard() {
    var rows = [];
    rows.push(DW.html`<p>Exposed to the client as <code class="mono">${D.env_name}</code>.</p>`);
    if (D.key_path) rows.push(DW.html`<p>Also written to <code class="mono">${'~/' + D.key_path}</code>.</p>`);
    if (D.is_github) rows.push(DW.html`<p>Git over HTTPS is routed through the gateway for the repositories granted to this token.</p>`);
    if (D.requests_7d !== null && D.requests_7d !== undefined) rows.push(DW.html`<p class="pt-big"><strong>${D.requests_7d.toLocaleString()}</strong> ${D.requests_7d === 1 ? 'request' : 'requests'} in the last 7 days</p>`);
    rows.push(DW.html`<p class="text-muted text-sm">${D.usage_count.toLocaleString()} lifetime ${D.usage_count === 1 ? 'use' : 'uses'}${D.last_used_at ? DW.html`, last used ${D.last_used_at} UTC` : ', never used'}.</p>`);
    rows.push(DW.html`<p><a href="${'/admin/clients/' + encodeURIComponent(D.client.id)}">Open client ${D.client.name}</a></p>`);
    DW.setHTML($('pt-client-body'), DW.html`${rows}`);
  }

  /* ---------- save / discard ---------- */
  function showBanner(msg, kind) {
    var b = $('pt-banner');
    b.textContent = msg || '';
    b.hidden = !msg;
    b.className = 'pt-banner ' + (kind === 'ok' ? 'pt-banner-ok' : 'pt-banner-bad');
    b.setAttribute('role', kind === 'ok' ? 'status' : 'alert');
  }
  DW.actions.ptSave = function () {
    if (S.saving || S.jsonErr || ruleProblems() || approvalProblem()) return;
    S.saving = true; refresh(); showBanner('');
    var cfg = draftConfig(), ttl = effTTL(ttlMinutes());
    var payload = { permission_config: cfg, requires_approval: S.approval };
    if (S.approval || S.orig.approval) payload.approval_ttl_minutes = S.approval ? ttl : effTTL(S.orig.ttl);
    return PT.api('PUT', '/api/placeholders/' + encodeURIComponent(ID), payload).then(function (saved) {
      S.orig = { config: cfg, approval: S.approval, ttl: payload.approval_ttl_minutes !== undefined ? payload.approval_ttl_minutes : S.orig.ttl };
      D.permission_config = cfg;
      showBanner('Changes saved.', 'ok');
      DW.toast('Phantom token saved', 'success');
    }).catch(function (e) {
      showBanner('Could not save: ' + e.message, 'bad');
    }).then(function () { S.saving = false; refresh(); });
  };
  function loadFromOrig() {
    var p = PTACL.parse(S.orig.config);
    if (p.ok) { S.jsonErr = ''; applyParsed(p); S.jsonText = S.orig.config; }
    else { S.jsonText = S.orig.config; S.jsonErr = p.err; S.rules = []; S.v2 = null; S.extra = {}; }
    S.approval = S.orig.approval;
    setTTLFromMinutes(S.orig.ttl);
    $('pt-json').value = S.jsonText;
    showJSONError();
    S.hit = -1; clearTest();
    renderRules(); renderApproval();
  }
  DW.actions.ptDiscard = function () {
    loadFromOrig();
    ghSeeded = false;
    showBanner('');
    setText('pt-rules-status', '');
    setText('pt-gh-status', '');
    refresh();
    DW.toast('Changes discarded', '');
  };

  /* ---------- unassign ---------- */
  DW.actions.ptAskUnassign = function () { $('pt-del-err').hidden = true; $('pt-del-go').disabled = false; DW.modal.open('#pt-del-modal'); };
  DW.actions.ptDoUnassign = function () {
    var btn = $('pt-del-go');
    btn.disabled = true;
    return PT.api('DELETE', '/api/placeholders/' + encodeURIComponent(ID)).then(function () {
      S.orig = { config: draftConfig(), approval: S.approval, ttl: ttlMinutes() };
      window.location = '/admin/phantom-tokens';
    }).catch(function (e) {
      var err = $('pt-del-err'); err.textContent = e.message; err.hidden = false; btn.disabled = false;
    });
  };

  window.addEventListener('beforeunload', function (e) {
    if (S.orig && isDirty()) { e.preventDefault(); e.returnValue = ''; }
  });

  /* ---------- init ---------- */
  S.orig = { config: D.permission_config || '', approval: !!D.requires_approval, ttl: D.approval_ttl_minutes || 0 };
  renderHeader();
  renderClientCard();
  loadFromOrig();
  if ($('pt-github')) $('pt-github').hidden = !D.is_github;
  refresh();
  setTab(S.jsonErr ? 'json' : 'rules', true);
})();
