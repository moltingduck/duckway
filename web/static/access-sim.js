/*
 * access-sim.js - "Test access" tab of the phantom token page (rlog1116).
 * Depends on dw.js, phantom-common.js (PT), access-sim-hex.js (ASHEX) and
 * access-sim-api.js (ASAPI, the only place that knows backend field names).
 * Markup goes through DW.html / DW.setHTML; no inline handlers.
 */
(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var host = $('as-root');
  var dataEl = $('pt-data');
  if (!host || !dataEl || !window.ASHEX || !window.ASAPI || !window.PT) return;
  var D = JSON.parse(dataEl.textContent);
  var ID = encodeURIComponent(D.id);
  var H = window.ASHEX, A = window.ASAPI;
  var SIM_URL = '/api/phantom-tokens/' + ID + '/simulate';
  var SUMMARY_URL = '/api/phantom-tokens/' + ID + '/access-summary';
  var METHODS = ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'];
  var DUMP_ROWS = 2048; // 32 KiB shown; the whole body is still sent

  var S = {
    shown: false, mode: D.is_github ? 'action' : 'raw',
    summary: [], summaryErr: '', summaryLoaded: false,
    repo: '', action: 'push', ref: 'main', workflow: 'deploy.yml',
    result: null, busy: false, err: '', seq: 0, timer: 0,
    view: 'text', bytes: new Uint8Array(0), hexErr: ''
  };

  function repoSummary() { return S.summary.filter(function (r) { return r.repo === S.repo; })[0] || null; }
  function say(t) { $('as-live').textContent = t; }

  /* ---------- shell ---------- */
  function buildShell() {
    var gh = D.is_github;
    var modes = gh ? DW.html`<div class="as-modes" role="group" aria-label="How to describe the request"><button type="button" class="lg-chip" data-action="asMode" data-mode="action" aria-pressed="true">Pick an action</button><button type="button" class="lg-chip" data-action="asMode" data-mode="raw" aria-pressed="false">Raw request</button></div>` : '';
    var actionCard = gh ? DW.html`<section class="card pt-card as-card" id="as-action" aria-labelledby="as-action-h">
      <h2 id="as-action-h">Would the agent be allowed to&hellip;</h2>
      <div class="as-grid">
        <label class="pt-fld"><span class="pt-fld-k">Repository</span><select id="as-repo" data-on-change="asRepo"></select></label>
        <label class="pt-fld pt-fld-grow"><span class="pt-fld-k">Branch or tag</span><input type="text" id="as-ref" class="mono" spellcheck="false" autocomplete="off" autocapitalize="off" placeholder="agent/fix-1203" data-on-input="asRefInput" aria-describedby="as-ref-hint"></label>
      </div>
      <div class="as-chips" id="as-refchips" role="group" aria-label="Example refs"></div>
      <p class="pt-hint" id="as-ref-hint"></p>
      <div class="pt-fld-k as-k" id="as-act-k">Action</div>
      <div class="as-chips" id="as-actions" role="group" aria-labelledby="as-act-k"></div>
      <label class="pt-fld as-wf" id="as-wf-row" hidden><span class="pt-fld-k">Workflow file</span><input type="text" id="as-wf" class="mono" value="deploy.yml" spellcheck="false" autocomplete="off" autocapitalize="off" data-on-input="asWfInput"></label>
    </section>` : '';
    var methodOpts = METHODS.map(function (m) { return DW.html`<option>${m}</option>`; });
    DW.setHTML(host, DW.html`<p class="pt-lede as-lede">Runs a request through the same checks the gateway uses and stops there. Nothing is sent upstream, and nothing is logged or counted. It tests this token as saved, so unsaved edits on the Overview tab are not included.</p>
    ${modes}${actionCard}
    <section class="card pt-card as-card" id="as-raw" aria-labelledby="as-raw-h"${gh ? DW.raw(' hidden') : ''}>
      <h2 id="as-raw-h">Raw request</h2>
      <p class="pt-hint">Works for every service, not only GitHub. A body that is not valid UTF-8 text opens in Hex; in Text, any byte that is not printable is written as <code>\\xNN</code>.</p>
      <div class="as-presets" role="group" aria-label="Start from an example"><span class="pt-fld-k">Start from an example</span>
        <button type="button" class="btn btn-sm" data-action="asPreset" data-preset="json">JSON: move main with force</button>
        <button type="button" class="btn btn-sm" data-action="asPreset" data-preset="push">git push (binary pkt-line)</button></div>
      <form id="as-raw-form" data-on-submit="asRawRun" novalidate>
        <div class="as-grid">
          <label class="pt-fld"><span class="pt-fld-k">Method</span><select id="as-method">${methodOpts}</select></label>
          <label class="pt-fld pt-fld-grow"><span class="pt-fld-k">Path</span><input type="text" id="as-path" class="mono" value="/" spellcheck="false" autocomplete="off" autocapitalize="off" placeholder="/repos/acme/api/issues"></label>
        </div>
        <label class="pt-fld as-hdr"><span class="pt-fld-k">Headers, one per line</span><textarea id="as-headers" class="mono pt-textarea-sm" spellcheck="false" autocomplete="off" placeholder="Content-Type: application/json"></textarea></label>
        <div class="as-bodyhead"><span class="pt-fld-k" id="as-body-k">Body</span><span class="pt-fld-k" id="as-size"></span>
          <div class="as-seg" role="group" aria-label="Body view"><button type="button" class="lg-chip" data-action="asView" data-view="text" aria-pressed="true">Text</button><button type="button" class="lg-chip" data-action="asView" data-view="hex" aria-pressed="false">Hex</button></div></div>
        <div id="as-text-panel"><label class="pt-fld"><span class="sr-only">Body as text</span><textarea id="as-text" class="mono as-body" spellcheck="false" autocomplete="off" data-on-input="asTextInput" aria-describedby="as-text-note"></textarea></label>
          <p class="pt-hint" id="as-text-note" hidden>This body has bytes that are not plain text, so Text is read-only. Switch to Hex to edit it.</p></div>
        <div id="as-hex-panel" hidden><pre class="as-dump mono" id="as-dump" tabindex="0" aria-label="Hex dump: offset, hex bytes, ASCII"></pre>
          <label class="pt-fld"><span class="pt-fld-k">Edit as hex (digits and spaces)</span><textarea id="as-hex" class="mono as-body" rows="4" spellcheck="false" autocomplete="off" autocapitalize="off" data-on-input="asHexInput" aria-describedby="as-hex-err"></textarea></label>
          <p class="pt-bad pt-hint" id="as-hex-err" role="alert" hidden></p></div>
        <div class="as-submit"><button type="submit" class="btn btn-primary" id="as-raw-go">Check request</button></div>
      </form>
    </section>
    <p class="sr-only" id="as-live" role="status" aria-live="polite" aria-atomic="true"></p>
    <p class="pt-bad pt-hint" id="as-err" role="alert" hidden></p>
    <div id="as-out"></div>
    <section class="card pt-card as-card" id="as-can" aria-labelledby="as-can-h"${gh ? '' : DW.raw(' hidden')}></section>`);
  }

  /* ---------- action mode ---------- */
  function paintRepos() {
    var sel = $('as-repo'); if (!sel) return;
    DW.setHTML(sel, DW.html`${S.summary.map(function (r) { return DW.html`<option value="${r.repo}"${r.repo === S.repo ? DW.raw(' selected') : ''}>${r.repo}</option>`; })}`);
    if (!S.summary.length) DW.setHTML(sel, DW.html`<option value="">No repositories on this token</option>`);
  }
  function paintChips() {
    var def = A.actionDef(S.action);
    var ex = A.refExamples(repoSummary());
    var refBox = $('as-refchips'); if (!refBox) return;
    DW.setHTML(refBox, DW.html`${ex.map(function (x) { return DW.html`<button type="button" class="lg-chip mono" data-action="asRefChip" data-ref="${x}" aria-pressed="${x === S.ref ? 'true' : 'false'}"${def.ref ? '' : DW.raw(' disabled')}>${x}</button>`; })}`);
    DW.setHTML($('as-actions'), DW.html`${A.ACTIONS.map(function (a) { return DW.html`<button type="button" class="lg-chip" data-action="asAction" data-act="${a.id}" aria-pressed="${a.id === S.action ? 'true' : 'false'}">${a.label}</button>`; })}`);
    $('as-ref').disabled = !def.ref;
    $('as-ref-hint').textContent = def.ref ? 'Chips show this token’s own branch patterns, then generic examples.' : 'This action does not depend on a branch or tag.';
    $('as-wf-row').hidden = !def.workflow;
  }
  function paintCan() {
    var box = $('as-can'); if (!box) return;
    var r = repoSummary();
    if (!r) {
      DW.setHTML(box, DW.html`<h2 id="as-can-h">What this token can do</h2><p class="text-muted text-sm">${S.summaryErr || 'No repository is granted on this token.'}</p>`);
      return;
    }
    var yn = function (v) { return v ? DW.raw('<span class="as-yes">Yes</span>') : DW.raw('<span class="as-no">No</span>'); };
    var rows = r.rules.map(function (x) { return DW.html`<tr><th scope="row" class="mono">${x.pattern}</th><td>${x.role}</td><td>${yn(x.push)}</td><td>${yn(x.createForceDelete)}</td><td>${yn(x.prInto)}</td><td>${yn(x.runWorkflows)}</td></tr>`; });
    DW.setHTML(box, DW.html`<h2 id="as-can-h">${'What this token can do on ' + r.repo}</h2><p class="pt-hint">One row per branch rule, most specific first. Capabilities turned off for the repository apply to every row.</p>
      ${r.legacy ? DW.html`<p class="pt-notice">${'This repository uses an older grant without branch rules.' + (r.refAllowlist.length ? ' Only these refs are allowed: ' + r.refAllowlist.join(', ') + '.' : ' It has no ref limit.')}</p>` : (r.rules.length ? DW.html`<div class="table-wrap"><table class="as-table"><thead><tr><th scope="col">Branches</th><th scope="col">Role</th><th scope="col">Push</th><th scope="col">Create &middot; force &middot; delete</th><th scope="col">Open PR into</th><th scope="col">Run workflows</th></tr></thead><tbody>${rows}</tbody></table></div>` : DW.html`<p class="text-muted text-sm">This repository has no branch rules.</p>`)}
      <ul class="as-flags"><li>${'Tags: ' + (r.tagPatterns.length ? 'may create ' + r.tagPatterns.join(', ') + ' only; existing tags never move.' : (r.tags ? 'may be created.' : 'none can be created.'))}</li><li>${'Merge pull requests: ' + (r.merge ? 'on' : 'off')}</li><li>${'Approve pull requests: ' + (r.approve ? 'on' : 'off')}</li><li>${'Clone: ' + (r.clone ? 'on' : 'off')}</li></ul>`);
  }

  function resultCard(r, subject) {
    var cls = r.verdict === 'allowed' ? 'ok' : (r.verdict === 'held' ? 'warn' : 'bad');
    var steps = r.steps.map(function (s) {
      var lab = s.status === 'pass' ? 'Pass' : (s.status === 'stop' ? 'Stop' : 'n/a');
      var c = s.status === 'pass' ? 'ok' : (s.status === 'stop' ? 'bad' : 'mute');
      return DW.html`<li class="as-step as-step-${s.status}"><span class="cl-pill cl-pill-${c}">${lab}</span><div class="as-step-body"><strong>${s.label}</strong>${s.detail ? DW.html`<span class="text-sm text-muted">${s.detail}</span>` : ''}</div></li>`;
    });
    var refs = r.refs.length ? DW.html`<h3 class="as-sub">Refs in this push</h3><p class="pt-hint">Git pushes are all-or-nothing, so one refused ref rejects the whole push.</p><ul class="lg-refs">${r.refs.map(function (x) {
      return DW.html`<li class="lg-ref"><span class="mono lg-ref-n">${x.ref}</span><span class="lg-pill ${x.ok ? 'lg-pill-ok' : 'lg-pill-bad'}">${x.ok ? 'OK' : 'Refused'}</span>${[x.pattern, x.role, x.reason].filter(Boolean).length ? DW.html`<span class="text-sm text-muted lg-ref-why">${[x.pattern, x.role, x.reason].filter(Boolean).join(' · ')}</span>` : ''}</li>`;
    })}</ul>` : '';
    var perKey = r.perKey.length ? DW.html`<h3 class="as-sub">The result differs by key</h3><ul class="as-perkey">${r.perKey.map(function (k) { return DW.html`<li><span class="mono">${k.key}</span> &mdash; <strong>${k.verdictLabel}</strong>${k.title ? ' ' + k.title : ''}${k.sees ? DW.html`<pre class="lg-sees mono">${k.sees}</pre>` : ''}</li>`; })}</ul>` : '';
    return DW.html`<section class="card pt-card as-verdict as-v-${r.verdict}" aria-labelledby="as-verdict-h"><div class="as-vhead"><span class="cl-pill cl-pill-${cls} as-vpill">${r.verdictLabel}</span><h2 id="as-verdict-h">${r.title || subject}</h2></div>
      ${r.reason && r.reason !== r.title ? DW.html`<p class="as-reason">${r.reason}</p>` : ''}${steps.length ? DW.html`<ol class="as-steps">${steps}</ol>` : ''}${refs}${perKey}
      ${r.dependsOnDefault ? DW.html`<p class="pt-hint">This result depends on the repository’s current default branch, read from GitHub and cached for up to 5 minutes.</p>` : ''}
      ${r.sees ? DW.html`<h3 class="as-sub">What the agent would see</h3><pre class="lg-sees mono">${r.sees}</pre>` : ''}</section>`;
  }

  function showResult(raw, subject) {
    var r = window.ASAPI.adaptResult(raw);
    S.result = r;
    DW.setHTML($('as-out'), resultCard(r, subject));
    say(r.verdictLabel + '. ' + (r.title || subject));
  }
  function showError(msg) {
    var e = $('as-err'); e.textContent = msg || ''; e.hidden = !msg;
  }
  function run(body, subject) {
    var seq = ++S.seq;
    showError('');
    say('Checking…');
    return PT.api('POST', SIM_URL, body).then(function (res) {
      if (seq !== S.seq) return;
      showResult(res, subject);
    }).catch(function (e) {
      if (seq !== S.seq) return;
      DW.setHTML($('as-out'), DW.raw(''));
      showError(e.message);
      var off = e.data && typeof e.data.offset === 'number' ? e.data.offset : -1;
      if (off >= 0 && body.body_hex !== undefined) markHex(off);
      say('The check failed. ' + e.message);
    });
  }
  // The server's offset is a character offset into the body_hex text it received,
  // which is the hex textarea's own text.
  function markHex(off) {
    setView('hex');
    var t = $('as-hex');
    t.focus();
    try { t.setSelectionRange(off, Math.min(off + 1, t.value.length)); } catch (e) { /* ignore */ }
    var he = $('as-hex-err');
    he.textContent = 'The server rejected the hex at character ' + (off + 1) + '.'; he.hidden = false;
  }
  function runAction() {
    if (!S.repo) { showError('This token has no repository to test.'); return; }
    var def = A.actionDef(S.action);
    return run(A.actionRequest(S), def.label + (def.ref && S.ref ? ' ' + S.ref : '') + ' on ' + S.repo);
  }
  function runActionLater() {
    clearTimeout(S.timer);
    S.timer = setTimeout(runAction, 350);
  }

  DW.actions.asMode = function (el) { setMode(el.dataset.mode); };
  function setMode(mode) {
    S.mode = mode;
    host.querySelectorAll('[data-action="asMode"]').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.mode === mode)); });
    if ($('as-action')) $('as-action').hidden = mode !== 'action';
    $('as-raw').hidden = mode !== 'raw';
    if ($('as-can')) $('as-can').hidden = mode !== 'action' || !D.is_github;
    S.seq++;
    DW.setHTML($('as-out'), DW.raw(''));
    showError('');
    say('');
    if (mode === 'action') runAction();
  }
  DW.actions.asRepo = function (el) { S.repo = el.value; paintChips(); paintCan(); runAction(); };
  DW.actions.asAction = function (el) { S.action = el.dataset.act; paintChips(); runAction(); };
  DW.actions.asRefChip = function (el) { S.ref = el.dataset.ref; $('as-ref').value = S.ref; paintChips(); runAction(); };
  DW.actions.asRefInput = function (el) { S.ref = el.value.trim(); runActionLater(); };
  DW.actions.asWfInput = function (el) { S.workflow = el.value.trim(); runActionLater(); };

  /* ---------- raw mode ---------- */
  // 8 bytes per dump row when the tab is narrower than ~600px, else 16.
  function narrow() { return host.clientWidth > 0 && host.clientWidth < 600; }
  function binary() { return H.needsHex(S.bytes); }
  function syncBody(from) {
    var bin = binary();
    $('as-size').textContent = '· ' + S.bytes.length + (S.bytes.length === 1 ? ' byte' : ' bytes') + (S.bytes.length ? (bin ? ' · binary' : ' · UTF-8 text') : '');
    var t = $('as-text');
    if (from !== 'text' && from !== 'dump') t.value = bin ? H.textView(S.bytes) : H.decodeUTF8(S.bytes);
    t.readOnly = bin;
    $('as-text-note').hidden = !bin;
    if (from !== 'hex' && from !== 'dump') $('as-hex').value = H.formatHex(S.bytes);
    var per = narrow() ? 8 : 16;
    var rows = H.dumpRows(S.bytes, DUMP_ROWS, per);
    var more = S.bytes.length > DUMP_ROWS * per ? '\n\u2026 ' + (S.bytes.length - DUMP_ROWS * per) + ' more bytes not shown' : '';
    $('as-dump').textContent = rows.length ? rows.map(function (r) { return r.offset + ' ' + r.hex + ' ' + r.ascii; }).join('\n') + more : '(empty body)';
  }
  function setView(v) {
    S.view = v;
    host.querySelectorAll('[data-action="asView"]').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.view === v)); });
    $('as-text-panel').hidden = v !== 'text';
    $('as-hex-panel').hidden = v !== 'hex';
  }
  function loadBody(bytes) {
    S.bytes = bytes; S.hexErr = '';
    $('as-hex-err').hidden = true;
    syncBody('');
    setView(binary() ? 'hex' : 'text');
  }
  function setMethod(m) {
    m = String(m || 'GET').toUpperCase();
    var sel = $('as-method');
    if (METHODS.indexOf(m) < 0) { var o = document.createElement('option'); o.textContent = m; sel.appendChild(o); }
    sel.value = m;
  }
  DW.actions.asView = function (el) { setView(el.dataset.view); };
  DW.actions.asTextInput = function (el) {
    if (el.readOnly) return;
    S.bytes = H.encodeUTF8(el.value); S.hexErr = '';
    $('as-hex-err').hidden = true;
    var wasBin = binary();
    syncBody('text');
    if (wasBin) setView('hex');
  };
  DW.actions.asHexInput = function (el) {
    var p = H.parseHex(el.value), e = $('as-hex-err');
    if (p.error) {
      S.hexErr = p.error + ' (character ' + (p.offset + 1) + ', byte ' + p.byte + ').';
      e.textContent = S.hexErr; e.hidden = false;
      return;
    }
    S.hexErr = ''; e.hidden = true;
    S.bytes = p.bytes;
    syncBody('hex');
  };
  DW.actions.asPreset = function (el) {
    var repo = S.repo || 'acme/api';
    var p = el.dataset.preset === 'push' ? H.presetPush(repo) : H.presetJSON(repo);
    setMethod(p.method); $('as-path').value = p.path; $('as-headers').value = p.headers;
    loadBody(p.bytes);
    $('as-path').focus();
  };
  DW.actions.asRawRun = function () {
    if (S.hexErr) { $('as-hex').focus(); say('Fix the hex error first. ' + S.hexErr); return; }
    var path = $('as-path').value.trim();
    if (!path || path.charAt(0) !== '/') { showError('The path must start with /.'); $('as-path').focus(); return; }
    var hd = H.parseHeaders($('as-headers').value);
    if (hd.error) { showError(hd.error); $('as-headers').focus(); return; }
    if (S.bytes.length > H.MAX_BODY) { showError('The body is larger than 16 MiB.'); return; }
    var method = $('as-method').value;
    return run(A.rawRequest({ method: method, path: path, headers: hd.headers, bytes: S.bytes, hexText: $('as-hex').value }), method + ' ' + path);
  };

  /* ---------- summary ---------- */
  function loadSummary() {
    if (!D.is_github || S.summaryLoaded) return Promise.resolve();
    S.summaryLoaded = true;
    return PT.api('GET', SUMMARY_URL).then(function (r) {
      S.summary = A.adaptSummary(r);
      if (!S.repo && S.summary.length) S.repo = S.summary[0].repo;
    }).catch(function (e) {
      S.summaryErr = e.message;
      S.summaryLoaded = false;
    }).then(function () {
      paintRepos(); paintChips(); paintCan();
    });
  }

  /* ---------- page tab ---------- */
  var TABS = ['overview', 'test'];
  function setURL(tab) {
    var p = new URLSearchParams(window.location.search);
    if (tab === 'test') p.set('tab', 'test'); else { p.delete('tab'); p.delete('method'); p.delete('path'); }
    var qs = p.toString();
    try { history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : '')); } catch (e) { /* ignore */ }
  }
  function setPageTab(tab, urlToo) {
    TABS.forEach(function (t) {
      var b = $('pt-ptab-' + t), on = t === tab;
      b.setAttribute('aria-selected', String(on));
      b.tabIndex = on ? 0 : -1;
      $('pt-panel-' + t).hidden = !on;
    });
    var bar = document.querySelector('.pt-bar');
    if (bar) bar.hidden = tab === 'test';
    if (urlToo) setURL(tab);
    if (tab === 'test' && !S.shown) {
      S.shown = true;
      loadSummary().then(function () { if (S.mode === 'action') runAction(); });
    }
  }
  DW.actions.ptPageTab = function (el) { setPageTab(el.dataset.ptab, true); };
  $('pt-pagetablist').addEventListener('keydown', function (e) {
    var i = TABS.indexOf(document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.ptab : '');
    if (i < 0) return;
    var n = i;
    if (e.key === 'ArrowRight') n = (i + 1) % TABS.length;
    else if (e.key === 'ArrowLeft') n = (i + TABS.length - 1) % TABS.length;
    else if (e.key === 'Home') n = 0;
    else if (e.key === 'End') n = TABS.length - 1;
    else return;
    e.preventDefault();
    setPageTab(TABS[n], true);
    $('pt-ptab-' + TABS[n]).focus();
  });

  /* ---------- init ---------- */
  buildShell();
  var lastNarrow = narrow();
  window.addEventListener('resize', function () { if (narrow() !== lastNarrow) { lastNarrow = narrow(); syncBody('dump'); } });
  setView('text');
  syncBody('');
  if (D.is_github) { $('as-ref').value = S.ref; paintRepos(); paintChips(); paintCan(); }
  var q = new URLSearchParams(window.location.search);
  if (q.get('method') || q.get('path')) {
    // Deep link from a request-log row: prefill raw mode (the log stores no body).
    if (D.is_github) setMode('raw');
    setMethod(q.get('method') || 'GET');
    $('as-path').value = q.get('path') || '/';
  }
  if (q.get('tab') === 'test') setPageTab('test', false);
})();
