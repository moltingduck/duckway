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
    summary: [], summaryErr: '', summaryLoaded: false, sumState: D.is_github ? 'idle' : 'ok', summaryP: null, refreshNext: false,
    lastBody: null, lastSubject: '', busy: false,
    repo: '', action: 'push', ref: 'main', workflow: 'deploy.yml',
    result: null, err: '', seq: 0, timer: 0,
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
    <p class="as-checking" id="as-checking" role="status" hidden>Checking&hellip;</p>
    <div id="as-out" aria-live="off"></div>
    <section class="card pt-card as-card" id="as-can" aria-labelledby="as-can-h"${gh ? '' : DW.raw(' hidden')}></section>`);
  }

  /* ---------- action mode ---------- */
  // 'idle' and 'loading' both show a loading state; 'ok' and 'error' are final until a retry.
  function summaryPending() { return S.sumState === 'idle' || S.sumState === 'loading'; }
  function paintRepos() {
    var sel = $('as-repo'); if (!sel) return;
    sel.setAttribute('aria-busy', summaryPending() ? 'true' : 'false');
    if (summaryPending()) { DW.setHTML(sel, DW.html`<option value="">Loading\u2026</option>`); return; }
    if (!S.summary.length) { DW.setHTML(sel, DW.html`<option value="">${S.summaryErr ? 'Could not load repositories' : 'No repositories on this token'}</option>`); return; }
    DW.setHTML(sel, DW.html`${S.summary.map(function (r) { return DW.html`<option value="${r.repo}"${r.repo === S.repo ? DW.raw(' selected') : ''}>${r.repo}</option>`; })}`);
  }
  // Chips are re-rendered only when their set changes (a repository switch or the summary
  // arriving). Otherwise aria-pressed/disabled are updated in place, so keyboard focus stays
  // on the chip that was just activated.
  var chipKey = '', actionsBuilt = false;
  function paintChips() {
    var def = A.actionDef(S.action);
    var ex = A.refExamples(repoSummary());
    var refBox = $('as-refchips'); if (!refBox) return;
    var key = ex.join('\u0000');
    if (key !== chipKey) {
      chipKey = key;
      DW.setHTML(refBox, DW.html`${ex.map(function (x) { return DW.html`<button type="button" class="lg-chip mono" data-action="asRefChip" data-ref="${x}" aria-pressed="false">${x}</button>`; })}`);
    }
    refBox.querySelectorAll('[data-ref]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(b.dataset.ref === S.ref));
      b.disabled = !def.ref;
    });
    if (!actionsBuilt) {
      actionsBuilt = true;
      DW.setHTML($('as-actions'), DW.html`${A.ACTIONS.map(function (a) { return DW.html`<button type="button" class="lg-chip" data-action="asAction" data-act="${a.id}" aria-pressed="false">${a.label}</button>`; })}`);
    }
    $('as-actions').querySelectorAll('[data-act]').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.act === S.action)); });
    $('as-ref').disabled = !def.ref;
    $('as-ref-hint').textContent = def.ref ? 'Chips show this token\u2019s own branch patterns, then generic examples.' : 'This action does not depend on a branch or tag.';
    $('as-wf-row').hidden = !def.workflow;
  }
  function paintCan() {
    var box = $('as-can'); if (!box) return;
    var r = repoSummary();
    box.setAttribute('aria-busy', summaryPending() ? 'true' : 'false');
    if (summaryPending()) { DW.setHTML(box, DW.html`<h2 id="as-can-h">What this token can do</h2><p class="text-muted text-sm" role="status">Loading\u2026</p>`); return; }
    if (!r) {
      if (S.summaryErr) DW.setHTML(box, DW.html`<h2 id="as-can-h">What this token can do</h2><p class="pt-bad pt-hint" role="alert">${'Could not load this token\u2019s access summary: ' + S.summaryErr}</p><button type="button" class="btn btn-sm" data-action="asRetrySummary">Retry</button>`);
      else DW.setHTML(box, DW.html`<h2 id="as-can-h">What this token can do</h2><p class="text-muted text-sm">No repository is granted on this token.</p>`);
      return;
    }
    var yn = function (v) { return v ? DW.raw('<span class="as-yes">Yes</span>') : DW.raw('<span class="as-no">No</span>'); };
    var cards = r.rules.map(function (x) {
      var pat = x.pattern === '@default' ? '@default (' + (r.defaultBranch || 'default branch') + ')' : x.pattern;
      return DW.html`<li class="as-rcard"><div class="as-rcard-head"><span class="mono as-rcard-pat">${pat}</span><span class="cl-pill cl-pill-mute">${x.role}</span></div><dl class="as-rcard-dl"><dt>Push</dt><dd>${yn(x.push)}</dd><dt>Create &middot; force &middot; delete</dt><dd>${yn(x.createForceDelete)}</dd><dt>Open PR into</dt><dd>${yn(x.prInto)}</dd><dt>Run workflows</dt><dd>${yn(x.runWorkflows)}</dd></dl></li>`;
    });
    var rows = r.rules.map(function (x) { return DW.html`<tr><th scope="row" class="mono">${x.pattern === '@default' ? '@default (' + (r.defaultBranch || 'default branch') + ')' : x.pattern}</th><td>${x.role}</td><td>${yn(x.push)}</td><td>${yn(x.createForceDelete)}</td><td>${yn(x.prInto)}</td><td>${yn(x.runWorkflows)}</td></tr>`; });
    DW.setHTML(box, DW.html`<h2 id="as-can-h">${'What this token can do on ' + r.repo}</h2><p class="pt-hint">One row per branch rule, most specific first. Capabilities turned off for the repository apply to every row.</p>
      ${r.legacy ? DW.html`<p class="pt-notice">${'This repository uses an older grant without branch rules.' + (r.refAllowlist.length ? ' Only these refs are allowed: ' + r.refAllowlist.join(', ') + '.' : ' It has no ref limit.')}</p>` : (r.rules.length ? DW.html`<div class="table-wrap"><table class="as-table"><thead><tr><th scope="col">Branches</th><th scope="col">Role</th><th scope="col">Push</th><th scope="col">Create &middot; force &middot; delete</th><th scope="col">Open PR into</th><th scope="col">Run workflows</th></tr></thead><tbody>${rows}</tbody></table></div><ul class="as-rcards">${cards}</ul>` : DW.html`<p class="text-muted text-sm">This repository has no branch rules.</p>`)}
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
    var perKey = r.perKey.length ? DW.html`<h3 class="as-sub">The result differs by key</h3><ul class="as-perkey">${r.perKey.map(function (k) { return DW.html`<li><span class="mono">${k.key}</span> &mdash; <strong>${k.verdictLabel}</strong>${k.kind ? ' (' + k.kind.replace(/_/g, ' ') + ')' : ''}${k.title ? ' ' + k.title : ''}${k.sees ? DW.html`<pre class="lg-sees mono">${k.sees}</pre>` : ''}</li>`; })}</ul>` : '';
    return DW.html`<section class="card pt-card as-verdict as-v-${r.verdict}" aria-labelledby="as-verdict-h"><div class="as-vhead"><span class="cl-pill cl-pill-${cls} as-vpill">${r.verdictLabel}</span>${r.kindLabel ? DW.html`<span class="cl-pill cl-pill-mute">${r.kindLabel}</span>` : ''}<h2 id="as-verdict-h" tabindex="-1">${r.title || subject}</h2></div>
      ${r.reason && r.reason !== r.title ? DW.html`<p class="as-reason">${r.reason}</p>` : ''}${r.rule ? DW.html`<p class="as-reason text-muted"><strong>Decided by</strong> ${r.rule}</p>` : ''}${steps.length ? DW.html`<ol class="as-steps">${steps}</ol>` : ''}${refs}${perKey}
      ${r.defaultUnresolved ? DW.html`<p class="pt-hint">The default branch could not be read from GitHub. <button type="button" class="btn btn-sm" data-action="asRetryLookup">Retry lookup</button></p>` : ''}
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
  // While a check is in flight the previous verdict stays visible but marked stale,
  // the region is aria-busy and the submit button is disabled. Only the latest request renders.
  function setBusy(on) {
    $('as-out').setAttribute('aria-busy', on ? 'true' : 'false');
    $('as-out').classList.toggle('as-stale', on && !!$('as-out').firstChild);
    $('as-checking').hidden = !on;
    S.busy = on;
    var go = $('as-raw-go'); if (go) { if (on) go.setAttribute('aria-disabled', 'true'); else go.removeAttribute('aria-disabled'); }
  }
  function run(body, subject, focusHeading) {
    if (S.refreshNext) { body.refresh = true; S.refreshNext = false; }
    S.lastBody = body; S.lastSubject = subject;
    var seq = ++S.seq;
    showError('');
    setBusy(true);
    say('Checking\u2026');
    return PT.api('POST', SIM_URL, body).then(function (res) {
      if (seq !== S.seq) return;
      setBusy(false);
      showResult(res, subject);
      if (focusHeading && $('as-verdict-h')) $('as-verdict-h').focus();
    }).catch(function (e) {
      if (seq !== S.seq) return;
      setBusy(false);
      DW.setHTML($('as-out'), DW.raw(''));
      showError(e.message);
      var off = e.data && typeof e.data.offset === 'number' ? e.data.offset : -1;
      if (off >= 0 && body.body_hex !== undefined) markHex(off);
      else if (e.status === 400 && body.mode === 'raw' && /path|redirect|fragment|#/i.test(e.message)) $('as-path').focus();
      say('The check failed. ' + e.message);
    });
  }
  DW.actions.asRetryLookup = function () {
    if (S.busy || !S.lastBody) return;
    var b = Object.assign({}, S.lastBody, { refresh: true });
    return run(b, S.lastSubject, true);
  };
  // The server's offset is a character offset into the body_hex text it received,
  // which is the hex textarea's own text.
  function markHex(off) {
    setView('hex');
    var t = $('as-hex');
    t.focus();
    try { t.setSelectionRange(off, Math.min(off + 1, t.value.length)); } catch (e) { /* ignore */ }
    // Scroll the selected character into view (a textarea does not always do it on focus).
    var lh = parseFloat(window.getComputedStyle(t).lineHeight) || 18;
    var line = (t.value.slice(0, off).match(/\n/g) || []).length;
    t.scrollTop = Math.max(0, line * lh - t.clientHeight / 2);
    var he = $('as-hex-err');
    he.textContent = 'The server rejected the hex at character ' + (off + 1) + '.'; he.hidden = false;
  }
  function runAction() {
    if (summaryPending()) return;
    if (!S.repo) { showError(S.sumState === 'error' ? 'The repository list could not be loaded. Use Retry under \u201cWhat this token can do\u201d.' : 'This token has no repository to test.'); return; }
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
    setBusy(false);
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
    if (from !== 'text' && from !== 'dump' && from !== 'resize') t.value = bin ? H.textView(S.bytes) : H.decodeUTF8(S.bytes);
    t.readOnly = bin;
    $('as-text-note').hidden = !bin;
    var per = narrow() ? 8 : 16;
    if ((from !== 'hex' && from !== 'dump') || (from === 'resize' && !S.hexErr)) $('as-hex').value = H.formatHex(S.bytes, per);
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
    if (binary()) say('This body has non-text bytes, so it opened in Hex.');
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
    if (wasBin) {
      // Typed or pasted a non-text byte: Text turns read-only, so move to Hex and say so.
      syncBody('');
      setView('hex');
      $('as-hex').focus();
      say('This body now has non-text bytes, so it opened in Hex.');
    } else syncBody('text');
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
    if (S.busy) return;
    if (S.hexErr) { $('as-hex').focus(); say('Fix the hex error first. ' + S.hexErr); return; }
    var path = $('as-path').value.trim();
    if (!path || path.charAt(0) !== '/') { showError('The path must start with /.'); $('as-path').focus(); return; }
    var hd = H.parseHeaders($('as-headers').value);
    if (hd.error) { showError(hd.error); $('as-headers').focus(); return; }
    if (S.bytes.length > H.MAX_BODY) { showError('The body is larger than 16 MiB.'); return; }
    var method = $('as-method').value;
    return run(A.rawRequest({ method: method, path: path, headers: hd.headers, bytes: S.bytes, hexText: $('as-hex').value }), method + ' ' + path);
  };

  // Retry: refresh past the server's remembered lookup failure. The button is marked busy in
  // place (not re-rendered) so focus stays on it; afterwards focus goes to the repository
  // select on success or to the new Retry button on failure. A second click while one is in
  // flight is ignored.
  DW.actions.asRetrySummary = function (el) {
    if (S.summaryP) return S.summaryP;
    el.setAttribute('aria-disabled', 'true');
    el.textContent = 'Retrying\u2026';
    return loadSummary(true).then(function () {
      if (S.sumState === 'ok') {
        if ($('as-repo')) $('as-repo').focus();
        if (S.mode === 'action') { S.refreshNext = true; runAction(); }
      } else {
        var again = $('as-can').querySelector('[data-action="asRetrySummary"]');
        if (again) again.focus();
      }
    });
  };

  /* ---------- summary ---------- */
  // One in-flight summary request at a time; summaryLoaded is true only once it succeeded.
  function loadSummary(refresh) {
    if (!D.is_github || S.summaryLoaded) return Promise.resolve();
    if (S.summaryP) return S.summaryP;
    if (!refresh) { S.sumState = 'loading'; paintRepos(); paintCan(); }
    S.summaryP = PT.api('GET', SUMMARY_URL + (refresh ? '?refresh=1' : '')).then(function (r) {
      S.summary = A.adaptSummary(r);
      if (!S.repo && S.summary.length) S.repo = S.summary[0].repo;
      S.summaryErr = ''; S.summaryLoaded = true; S.sumState = 'ok';
    }).catch(function (e) {
      S.summaryErr = e.message; S.sumState = 'error';
    }).then(function () {
      S.summaryP = null;
      paintRepos(); paintChips(); paintCan();
    });
    return S.summaryP;
  }
  /* ---------- page tab ---------- */
  var TABS = ['overview', 'test', 'history'];
  function setURL(tab) {
    var p = new URLSearchParams(window.location.search);
    if (tab === 'test' || tab === 'history') p.set('tab', tab); else { p.delete('tab'); p.delete('method'); p.delete('path'); }
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
    if (bar) bar.hidden = tab !== 'overview';
    if (urlToo) setURL(tab);
    if (tab === 'history' && window.AUDIT && !S.histShown) {
      S.histShown = true;
      AUDIT.mountHistory($('pt-history'), { resource_type: 'placeholder', resource_id: D.id });
    }
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
  window.addEventListener('resize', function () { if (narrow() !== lastNarrow) { lastNarrow = narrow(); syncBody('resize'); } });
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
  if (q.get('tab') === 'test' || q.get('tab') === 'history') setPageTab(q.get('tab'), false);
})();
