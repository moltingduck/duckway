/*
 * rule-preview.js - "Preview impact" for the three rule editors (fx1116): the
 * phantom token page, the credential rules dialog and the service default ACL.
 * The pure helpers (bucket tabs, row models, request builders, confirm text)
 * run under node tests; RPREV.mount() renders the result with DW.html and needs
 * dw.js. Exposed as window.RPREV and module.exports.
 *   POST /api/placeholders/{id}/preview-impact  {permission_config, requires_approval, approval_ttl_minutes}
 *   POST /api/keys/{id}/preview-impact          {acl}
 *   POST /api/services/{id}/preview-impact      {default_acl}
 *   -> {window_days,total_requests,distinct,truncated,buckets{},bucket_hits{},
 *       items[{bucket,placeholder_id,token_name,method,path,client,hits,before{verdict,kind,rule},
 *              after{...},reason?,reason_code?,refs?[{ref,before,after}],simulate_url}],tokens?[]}
 */
(function (root) {
  'use strict';
  var node = typeof module === 'object' && module && module.exports;

  var BUCKETS = [
    { id: 'newly_refused', label: 'Newly refused', tone: 'bad', help: 'Allowed or held today, refused with this change.' },
    { id: 'newly_held', label: 'Newly held', tone: 'warn', help: 'Not held today, waits for an admin with this change.' },
    { id: 'newly_allowed', label: 'Newly allowed', tone: 'ok', help: 'Refused today, allowed with this change.' },
    { id: 'no_longer_held', label: 'No longer held', tone: 'ok', help: 'Waited for an admin today, would not with this change.' },
    { id: 'unknown', label: 'Cannot tell', tone: 'mute', help: 'The log does not keep what is needed to decide these.' },
    { id: 'unchanged', label: 'Unchanged', tone: 'mute', help: 'Same outcome with this change.' }
  ];
  var BUCKET_IDS = BUCKETS.map(function (b) { return b.id; });
  function bucketDef(id) { for (var i = 0; i < BUCKETS.length; i++) if (BUCKETS[i].id === id) return BUCKETS[i]; return null; }

  function num(v) { return typeof v === 'number' && isFinite(v) && v > 0 ? Math.floor(v) : 0; }
  function str(v) { return typeof v === 'string' ? v : (v === null || v === undefined ? '' : String(v)); }
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : (many || one + 's')); }

  /* ---- requests ---- */
  function endpoint(kind, id) {
    var e = encodeURIComponent(id);
    if (kind === 'token') return '/api/placeholders/' + e + '/preview-impact';
    if (kind === 'key') return '/api/keys/' + e + '/preview-impact';
    if (kind === 'service') return '/api/services/' + e + '/preview-impact';
    throw new Error('unknown preview kind ' + kind);
  }
  // st: {config, approval, ttl}; the same draft the editor sends to acl/test.
  function tokenBody(st) {
    var b = { permission_config: str(st.config) };
    if (typeof st.approval === 'boolean') b.requires_approval = st.approval;
    if (typeof st.ttl === 'number' && st.ttl > 0) b.approval_ttl_minutes = Math.floor(st.ttl);
    return b;
  }
  function keyBody(acl) { return { acl: str(acl) }; }
  function serviceBody(cfg) { return { default_acl: str(cfg) }; }

  /* ---- result ---- */
  function counts(r) {
    var b = (r && r.buckets) || {}, out = {};
    BUCKET_IDS.forEach(function (id) { out[id] = num(b[id]); });
    return out;
  }
  function hitsOf(r) {
    var h = (r && r.bucket_hits) || {}, out = {};
    BUCKET_IDS.forEach(function (id) { out[id] = num(h[id]); });
    return out;
  }
  // tabs(result) -> one entry per bucket in display order.
  function tabs(r) {
    var c = counts(r), h = hitsOf(r);
    return BUCKETS.map(function (b) { return { id: b.id, label: b.label, tone: b.tone, help: b.help, count: c[b.id], hits: h[b.id] }; });
  }
  // The tab to open first: the first bucket that carries a change, else unchanged.
  function defaultTab(r) {
    var c = counts(r);
    for (var i = 0; i < BUCKET_IDS.length; i++) if (BUCKET_IDS[i] !== 'unchanged' && BUCKET_IDS[i] !== 'unknown' && c[BUCKET_IDS[i]] > 0) return BUCKET_IDS[i];
    if (c.unknown > 0) return 'unknown';
    return 'unchanged';
  }
  function itemsFor(r, tab) {
    return ((r && r.items) || []).filter(function (it) { return it && it.bucket === tab; });
  }
  // How many groups of a bucket exist beyond the 200 items the server returns.
  function hiddenCount(r, tab) { return Math.max(0, counts(r)[tab] - itemsFor(r, tab).length); }

  // subject names what was replayed against ("the "Read only" template"), when the caller has one.
  function summary(r, subject) {
    if (!r) return '';
    var total = num(r.total_requests), distinct = num(r.distinct), days = num(r.window_days) || 7;
    if (total === 0 && distinct === 0) return 'No requests in the last ' + plural(days, 'day') + ', so there is nothing to compare yet.';
    var c = counts(r), parts = [];
    if (c.newly_refused) parts.push(plural(c.newly_refused, 'request pattern') + ' would be refused');
    if (c.newly_held) parts.push(plural(c.newly_held, 'pattern') + ' would wait for approval');
    if (c.newly_allowed) parts.push(plural(c.newly_allowed, 'pattern') + ' would be allowed');
    if (c.no_longer_held) parts.push(plural(c.no_longer_held, 'pattern') + ' would no longer wait');
    var head = 'Replayed ' + plural(total, 'request') + ' (' + plural(distinct, 'distinct pattern') + ') from the last ' + plural(days, 'day') + (subject ? ' against ' + subject : '') + '. ';
    var body = parts.length ? parts.join(', ') + '.' : 'Nothing changes for the traffic that could be checked.';
    if (c.unknown) body += ' ' + plural(c.unknown, 'pattern') + ' could not be decided from the log.';
    if (r.truncated) body += ' The preview stopped early (too many patterns or too slow), so this is a partial result.';
    return head + body;
  }

  function needsConfirm(r) { return !!r && num((r.buckets || {}).newly_refused) > 0; }
  function confirmText(r, what) {
    var n = num(((r && r.buckets) || {}).newly_refused), h = num(((r && r.bucket_hits) || {}).newly_refused);
    var unknown = num(((r && r.buckets) || {}).unknown), extra = '';
    if (unknown) extra += ' ' + plural(unknown, 'more pattern') + ' could not be checked and may be refused too.';
    if (r && r.truncated) extra += ' The preview stopped early, so there may be more.';
    return {
      title: 'Save rules that refuse recent traffic?',
      body: 'With this change ' + plural(n, 'request pattern') + ' (' + plural(h, 'request') + ' in the last 7 days) that ' + (what || 'the rules') + ' allow or hold today would be refused.' + extra + ' Save anyway?',
      confirmLabel: 'Save anyway', cancelLabel: 'Keep editing', danger: true
    };
  }
  // Asked when the preview itself failed, so a save is never blocked by it.
  function errorConfirmText() {
    return { title: 'Could not check recent traffic', body: 'Could not check recent traffic. Save anyway?', confirmLabel: 'Save anyway', cancelLabel: 'Keep editing', danger: true };
  }

  /* ---- rows ---- */
  var VERDICT = { allowed: 'Allowed', refused: 'Refused', held: 'Held', unknown: 'Unknown' };
  function verdictLabel(v) { return VERDICT[v] || (v ? String(v) : 'Unknown'); }
  function verdictTone(v) { return v === 'refused' ? 'bad' : (v === 'held' ? 'warn' : (v === 'allowed' ? 'ok' : 'mute')); }
  function side(s) {
    s = s || {};
    return { verdict: str(s.verdict), label: verdictLabel(s.verdict), tone: verdictTone(s.verdict), rule: str(s.rule), kind: str(s.kind) };
  }
  // Only same-origin links into a phantom token's Test access tab are used.
  function safeHref(u) { u = str(u); return /^\/admin\/phantom-tokens\/[^/?#]+\?/.test(u) ? u : ''; }
  // A token-wide approval holds a request before the rules run, so a request the
  // rules refuse today shows as "newly held" although approving it does not let it through.
  function heldNote(it) {
    var b = it.before || {}, a = it.after || {};
    return a.verdict === 'held' && a.kind === 'approval' && b.verdict === 'refused'
      ? 'Held first; after approval the rules still refuse it.' : '';
  }
  function rowModel(it) {
    var hits = num(it && it.hits);
    return {
      note: heldNote(it),
      method: str(it.method), path: str(it.path),
      who: str(it.token_name), client: str(it.client),
      hits: hits, hitsText: plural(hits, 'request'),
      before: side(it.before), after: side(it.after),
      changed: side(it.before).verdict !== side(it.after).verdict,
      reason: str(it.reason), reasonCode: str(it.reason_code),
      href: safeHref(it.simulate_url),
      refs: (Array.isArray(it.refs) ? it.refs : []).map(function (r) { return { ref: str(r.ref), before: side(r.before), after: side(r.after) }; })
    };
  }
  // Per-token totals of a key or service preview.
  function tokenRows(r) {
    return ((r && r.tokens) || []).map(function (t) {
      var b = t.bucket_hits || {};
      return { id: str(t.placeholder_id), name: str(t.token_name), client: str(t.client), refused: num((t.buckets || {}).newly_refused), distinct: num(t.distinct), refusedHits: num(b.newly_refused) };
    });
  }

  var api = {
    BUCKETS: BUCKETS, endpoint: endpoint, tokenBody: tokenBody, keyBody: keyBody, serviceBody: serviceBody,
    counts: counts, tabs: tabs, defaultTab: defaultTab, itemsFor: itemsFor, hiddenCount: hiddenCount,
    summary: summary, needsConfirm: needsConfirm, confirmText: confirmText, errorConfirmText: errorConfirmText, verdictLabel: verdictLabel,
    rowModel: rowModel, tokenRows: tokenRows, bucketDef: bucketDef, safeHref: safeHref
  };

  /* ---- browser: DW.html rendering and the controller ---- */
  function mount(host, opts) {
    var DW = root.DW, PT = root.PT;
    // opts.call(url, body) -> Promise of the parsed JSON; defaults to the phantom token pages' PT.api.
    var call = opts.call || function (url, body) { return PT.api('POST', url, body); };
    var S = { status: 'idle', result: null, tab: 'unchanged', err: '', seq: 0, stale: false, sig: '', ackSig: '' };
    DW.setHTML(host, DW.html`<div class="rp"><div class="rp-headhost"></div><div class="rp-live" aria-live="polite"></div></div>`);
    var headEl = host.querySelector('.rp-headhost'), liveEl = host.querySelector('.rp-live');
    var idBase = 'rp-' + Math.random().toString(36).slice(2, 8);

    function pill(tone, text) { return DW.html`<span class="cl-pill cl-pill-${tone}">${text}</span>`; }
    function sideText(s) { return s.rule ? DW.html`${pill(s.tone, s.label)} <span class="rp-rule">${s.rule}</span>` : pill(s.tone, s.label); }
    function refList(m) {
      if (!m.refs.length) return '';
      return DW.html`<ul class="rp-refs">${m.refs.map(function (r) { return DW.html`<li><code class="mono">${r.ref}</code> ${pill(r.before.tone, r.before.label)} <span aria-hidden="true">&rarr;</span><span class="sr-only">now</span> ${pill(r.after.tone, r.after.label)}</li>`; })}</ul>`;
    }
    function link(m) {
      return m.href ? DW.html`<a class="btn btn-sm" href="${m.href}">Test in simulator</a>` : '';
    }
    function tableRow(m) {
      return DW.html`<tr><td class="rp-req"><code class="mono">${m.method} ${m.path}</code>${m.reason ? DW.html`<p class="rp-why">${m.reason}</p>` : ''}${m.note ? DW.html`<p class="rp-why">${m.note}</p>` : ''}${refList(m)}</td><td>${m.who}${m.client ? DW.html`<span class="rp-sub">${m.client}</span>` : ''}</td><td class="rp-num">${m.hitsText}</td><td>${sideText(m.before)}</td><td>${sideText(m.after)}</td><td>${link(m)}</td></tr>`;
    }
    function card(m) {
      return DW.html`<li class="rp-card"><div class="rp-card-head"><code class="mono rp-card-path">${m.method} ${m.path}</code><span class="rp-num">${m.hitsText}</span></div>${m.reason ? DW.html`<p class="rp-why">${m.reason}</p>` : ''}${m.note ? DW.html`<p class="rp-why">${m.note}</p>` : ''}${refList(m)}<dl class="rp-card-dl"><dt>Token</dt><dd>${m.who}${m.client ? ' · ' + m.client : ''}</dd><dt>Today</dt><dd>${sideText(m.before)}</dd><dt>With this change</dt><dd>${sideText(m.after)}</dd></dl>${link(m)}</li>`;
    }
    function tokenTable(r) {
      var rows = api.tokenRows(r).filter(function (t) { return t.refused > 0; });
      if (!rows.length) return '';
      return DW.html`<div class="rp-tokens"><h3 class="rp-h">Tokens that lose access</h3><ul class="rp-toklist">${rows.map(function (t) { return DW.html`<li><a href="${'/admin/phantom-tokens/' + encodeURIComponent(t.id)}">${t.name}</a>${t.client ? ' · ' + t.client : ''}: ${plural(t.refused, 'pattern')} refused (${plural(t.refusedHits, 'request')})</li>`; })}</ul></div>`;
    }
    function panel() {
      var r = S.result, items = api.itemsFor(r, S.tab).map(api.rowModel);
      var def = api.bucketDef(S.tab);
      if (!items.length) return DW.html`<p class="rp-empty">${def ? def.help + ' ' : ''}Nothing here.</p>`;
      var more = api.hiddenCount(r, S.tab);
      return DW.html`<p class="rp-help">${def.help}</p><div class="table-wrap rp-table"><table class="as-table"><thead><tr><th scope="col">Request</th><th scope="col">Token</th><th scope="col">Seen</th><th scope="col">Today</th><th scope="col">With this change</th><th scope="col"><span class="sr-only">Simulator</span></th></tr></thead><tbody>${items.map(tableRow)}</tbody></table></div><ul class="rp-cards">${items.map(card)}</ul>${more ? DW.html`<p class="rp-more">${'Showing the ' + items.length + ' most frequent of ' + (items.length + more) + '.'}</p>` : ''}`;
    }
    function render() {
      var r = S.result, loading = S.status === 'loading';
      var head = DW.html`<div class="rp-bar"><button type="button" class="btn" data-action="rpRun" data-rp="${idBase}"${loading ? ' disabled' : ''}>${loading ? 'Checking recent traffic…' : 'Preview impact'}</button><span class="rp-hint">${opts.hint || 'Replays the last 7 days of requests against this draft. Nothing is sent or saved.'}</span></div>`;
      var body = '';
      if (S.status === 'error') body = DW.html`<p class="rp-err" role="alert">${S.err}</p>`;
      else if (r) {
        var tabsEl = api.tabs(r).map(function (t) {
          var on = t.id === S.tab;
          return DW.html`<button type="button" role="tab" class="lg-chip rp-tab rp-tab-${t.tone}" id="${idBase + '-t-' + t.id}" aria-selected="${on ? 'true' : 'false'}" aria-controls="${idBase + '-p'}" tabindex="${on ? '0' : '-1'}" data-action="rpTab" data-rp="${idBase}" data-tab="${t.id}">${t.label} <span class="rp-count">${t.count}</span></button>`;
        });
        body = DW.html`<div class="rp-result${S.stale ? ' rp-stale' : ''}"><p class="rp-summary">${api.summary(r, opts.subject ? opts.subject() : '')}</p>${S.stale ? DW.html`<p class="rp-stalenote">The draft changed after this preview. Run it again.</p>` : ''}${tokenTable(r)}<div class="rp-tabs" role="tablist" aria-label="Outcome of the change">${tabsEl}</div><div id="${idBase + '-p'}" role="tabpanel" aria-labelledby="${idBase + '-t-' + S.tab}" tabindex="0">${panel()}</div></div>`;
      }
      DW.setHTML(headEl, head);
      DW.setHTML(liveEl, DW.html`${body}`);
    }

    function run() {
      var b = opts.body();
      if (!b) { S.status = 'error'; S.err = opts.blocked || 'Fix the errors in the draft before previewing.'; render(); return Promise.resolve(null); }
      var seq = ++S.seq, sig = JSON.stringify(b);
      S.status = 'loading'; S.stale = false; render();
      return call(api.endpoint(opts.kind, opts.id()), b).then(function (res) {
        if (seq !== S.seq) return null;
        S.status = 'done'; S.result = res; S.sig = sig; S.stale = false; S.tab = api.defaultTab(res); render();
        if (opts.scroll && host.scrollIntoView) host.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
        return res;
      }).catch(function (e) {
        if (seq !== S.seq) return null;
        S.status = 'error'; S.err = 'Could not preview: ' + e.message; render();
        return null;
      });
    }
    function invalidate() {
      S.seq++; // a response for an older draft is dropped
      if (S.status === 'loading') S.status = S.result ? 'done' : 'idle';
      if (S.result) { S.stale = true; }
      if (S.status === 'error') S.status = 'idle';
      render();
    }
    // Before a save: preview the draft when no fresh preview exists, then ask
    // once if recent traffic would be refused. Resolves true to go ahead.
    function checkBeforeSave() {
      var b = opts.body();
      if (!b) return Promise.resolve(true);
      var sig = JSON.stringify(b);
      var have = S.result && !S.stale && S.sig === sig ? Promise.resolve(S.result) : run();
      return have.then(function (res) {
        if (!res) {
          // A failed preview (not a superseded one) must not block the save silently: ask.
          if (S.status === 'error' && S.ackSig !== sig) {
            return DW.confirm(api.errorConfirmText()).then(function (ok) { if (ok) S.ackSig = sig; return ok; });
          }
          return true;
        }
        if (!api.needsConfirm(res) || S.ackSig === sig) return true;
        return DW.confirm(api.confirmText(res, opts.what)).then(function (ok) { if (ok) S.ackSig = sig; return ok; });
      }).catch(function () { return true; });
    }
    function reset() { S.seq++; S.status = 'idle'; S.result = null; S.stale = false; S.err = ''; S.ackSig = ''; render(); }

    var prevRun = DW.actions.rpRun;
    DW.actions.rpRun = function (el) {
      if (el.dataset.rp !== idBase) { if (prevRun) return prevRun(el); return; }
      return run();
    };
    var prevTab = DW.actions.rpTab;
    DW.actions.rpTab = function (el) {
      if (el.dataset.rp !== idBase) { if (prevTab) prevTab(el); return; }
      S.tab = el.dataset.tab; render();
      var t = document.getElementById(idBase + '-t-' + S.tab); if (t) t.focus();
    };
    host.addEventListener('keydown', function (e) {
      var t = e.target && e.target.getAttribute && e.target.getAttribute('role') === 'tab' ? e.target : null;
      if (!t || !S.result) return;
      var i = BUCKET_IDS.indexOf(S.tab), n = i;
      if (e.key === 'ArrowRight') n = (i + 1) % BUCKET_IDS.length;
      else if (e.key === 'ArrowLeft') n = (i + BUCKET_IDS.length - 1) % BUCKET_IDS.length;
      else if (e.key === 'Home') n = 0;
      else if (e.key === 'End') n = BUCKET_IDS.length - 1;
      else return;
      e.preventDefault();
      S.tab = BUCKET_IDS[n]; render();
      var f = document.getElementById(idBase + '-t-' + S.tab); if (f) f.focus();
    });
    render();
    return { run: run, invalidate: invalidate, checkBeforeSave: checkBeforeSave, reset: reset, state: S };
  }
  api.mount = mount;

  if (node) module.exports = api; else root.RPREV = api;
})(typeof window !== 'undefined' ? window : globalThis);
