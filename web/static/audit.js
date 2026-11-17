/*
 * audit.js - admin audit log view helpers (fx1116).
 * Pure helpers (lineDiff, pretty, actionLabel, buildQuery, ...) work under node;
 * the DOM helpers (AUDIT.mountHistory, AUDIT.renderDetail) need window.DW.
 * Exposed as window.AUDIT (and module.exports under node tests).
 *
 * ADAPTER: API fields read: id, created_at, actor, via, remote_addr, action, resource_type,
 * resource_id, resource_name, summary, status, has_diff (list); before, after (detail).
 */
(function (root) {
  'use strict';

  var KINDS = [
    ['', 'All'], ['rules', 'Rules'], ['approvals', 'Approvals'], ['credentials', 'Credentials'],
    ['clients', 'Clients'], ['signins', 'Sign-ins'], ['settings', 'Settings']
  ];
  var TYPE_LABEL = {
    service: 'Service', api_key: 'Credential', placeholder: 'Phantom token', group: 'Group', client: 'Client',
    notification: 'Notification channel', setting: 'Settings', approval: 'Approval', signin: 'Sign-in',
    key_group: 'Key pool', key_suite: 'Key bundle', plugin: 'Plugin', cc: 'Control channel', canary: 'Canary', approver_token: 'Approver token'
  };

  function str(v) { return typeof v === 'string' ? v : (v === null || v === undefined ? '' : String(v)); }
  function typeLabel(t) { return TYPE_LABEL[t] || (t ? String(t).replace(/_/g, ' ') : ''); }

  // actionLabel("placeholder.acl_update") -> "Acl update"
  function actionLabel(action) {
    var a = str(action).split('.');
    var verb = (a.length > 1 ? a.slice(1).join(' ') : a[0]).replace(/_/g, ' ');
    return verb.charAt(0).toUpperCase() + verb.slice(1);
  }

  // Who: "admin:alice" -> "alice", with the channel kept as a chip elsewhere.
  function actorLabel(actor) {
    var a = str(actor);
    if (a === '' || a === 'anonymous') return 'Anonymous';
    var i = a.indexOf(':');
    return i > 0 ? a.slice(i + 1) + ' (' + a.slice(0, i) + ')' : a;
  }

  function fmtTime(s) {
    s = str(s);
    if (!s) return '';
    var d = new Date(s.replace(' ', 'T') + 'Z');
    if (isNaN(d.getTime())) return s;
    return d.toLocaleString([], { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
  }

  function pretty(v) {
    if (v === null || v === undefined || v === '') return '';
    if (typeof v === 'string') {
      try { v = JSON.parse(v); } catch (e) { return v; }
    }
    return JSON.stringify(sortKeys(v), null, 2);
  }
  function sortKeys(v) {
    if (Array.isArray(v)) return v.map(sortKeys);
    if (v && typeof v === 'object') {
      var o = {};
      Object.keys(v).sort().forEach(function (k) { o[k] = sortKeys(v[k]); });
      return o;
    }
    return v;
  }

  // lineDiff(beforeText, afterText) -> [{t: 'same'|'add'|'del', s: line}] via LCS.
  function lineDiff(a, b) {
    var x = a === '' ? [] : String(a).split('\n');
    var y = b === '' ? [] : String(b).split('\n');
    var n = x.length, m = y.length, i, j;
    var L = [];
    for (i = 0; i <= n; i++) { L.push(new Array(m + 1).fill(0)); }
    for (i = n - 1; i >= 0; i--) {
      for (j = m - 1; j >= 0; j--) {
        L[i][j] = x[i] === y[j] ? L[i + 1][j + 1] + 1 : Math.max(L[i + 1][j], L[i][j + 1]);
      }
    }
    var out = [];
    i = 0; j = 0;
    while (i < n && j < m) {
      if (x[i] === y[j]) { out.push({ t: 'same', s: x[i] }); i++; j++; }
      else if (L[i + 1][j] >= L[i][j + 1]) { out.push({ t: 'del', s: x[i] }); i++; }
      else { out.push({ t: 'add', s: y[j] }); j++; }
    }
    while (i < n) { out.push({ t: 'del', s: x[i++] }); }
    while (j < m) { out.push({ t: 'add', s: y[j++] }); }
    return out;
  }

  function parseVal(v) {
    if (v === null || v === undefined || v === '') return null;
    if (typeof v === 'string') { try { return JSON.parse(v); } catch (e) { return v; } }
    return v;
  }
  function isObj(v) { return v !== null && typeof v === 'object' && !Array.isArray(v); }
  function same(a, b) { return JSON.stringify(sortKeys(a)) === JSON.stringify(sortKeys(b)); }
  function valueLines(prefix, v, t, out) {
    var pad = prefix.replace(/\S.*$/, '');
    var lines = JSON.stringify(sortKeys(v), null, 2).split('\n');
    lines.forEach(function (l, i) { out.push({ t: t, s: (i === 0 ? prefix : pad) + l }); });
  }
  function foldNote(indent, names) {
    var shown = names.slice(0, 6).join(', ') + (names.length > 6 ? ' and ' + (names.length - 6) + ' more' : '');
    return { t: 'fold', s: indent + '\u2026 ' + names.length + ' unchanged: ' + shown };
  }
  function diffObjects(bm, am, indent, out) {
    var keys = Object.keys(bm).concat(Object.keys(am).filter(function (k) { return !(k in bm); })).sort();
    var unchanged = [];
    keys.forEach(function (k) {
      var inB = k in bm, inA = k in am, pre = indent + JSON.stringify(k) + ': ';
      if (inB && inA && same(bm[k], am[k])) { unchanged.push(k); return; }
      if (inB && inA && isObj(bm[k]) && isObj(am[k])) {
        out.push({ t: 'same', s: pre + '{' });
        diffObjects(bm[k], am[k], indent + '  ', out);
        out.push({ t: 'same', s: indent + '}' });
        return;
      }
      if (inB) valueLines(pre, bm[k], 'del', out);
      if (inA) valueLines(pre, am[k], 'add', out);
    });
    if (unchanged.length) out.push(foldNote(indent, unchanged));
  }
  // changeLines(before, after) -> [{t: 'same'|'add'|'del'|'fold', s}]: only the changed keys, as
  // "- before" / "+ after" lines, with unchanged keys collapsed into one note per object.
  function changeLines(before, after) {
    var b = parseVal(before), a = parseVal(after), out = [];
    if ((isObj(b) || b === null) && (isObj(a) || a === null) && (b !== null || a !== null)) {
      diffObjects(b || {}, a || {}, '', out);
      return out;
    }
    return lineDiff(pretty(before), pretty(after));
  }

  // buildQuery(state) -> URLSearchParams string for GET /api/audit.
  function buildQuery(s) {
    var p = new URLSearchParams();
    ['kind', 'actor', 'since', 'q', 'resource_type', 'resource_id', 'cursor'].forEach(function (k) {
      if (s && s[k]) p.set(k, s[k]);
    });
    p.set('limit', String((s && s.limit) || 50));
    return p.toString();
  }

  // sinceFor("24h") -> RFC 3339 time that far back; "all"/"" -> ''.
  function sinceFor(range, now) {
    var ms = { '24h': 864e5, '7d': 7 * 864e5, '30d': 30 * 864e5 }[range];
    if (!ms) return '';
    return new Date((now || Date.now()) - ms).toISOString();
  }

  var api = { KINDS: KINDS, typeLabel: typeLabel, actionLabel: actionLabel, actorLabel: actorLabel, fmtTime: fmtTime,
    pretty: pretty, lineDiff: lineDiff, changeLines: changeLines, buildQuery: buildQuery, sinceFor: sinceFor };

  /* ---------------- DOM ---------------- */
  var DW = root.DW;
  if (DW && root.document) {
    var html = DW.html;

    api.diffNode = function (before, after) {
      var d = changeLines(before, after);
      var sym = { same: '  ', add: '+ ', del: '- ', fold: '  ' };
      return DW.html`<pre class="au-diff" aria-label="Changes: removed lines marked minus, added lines marked plus">${d.map(function (l) {
        return DW.html`<span class="au-l au-${l.t}"><span class="au-sym" aria-hidden="true">${sym[l.t]}</span>${l.s}\n</span>`;
      })}</pre>`;
    };

    // renderDetail(el, entry): metadata plus the before/after line diff.
    api.renderDetail = function (el, e) {
      var hasBA = e.before !== undefined && e.before !== null || e.after !== undefined && e.after !== null;
      var meta = [
        ['When', fmtTime(e.created_at)], ['Who', actorLabel(e.actor)], ['Via', str(e.via)],
        ['From', str(e.remote_addr)], ['Action', str(e.action)],
        ['Resource', typeLabel(e.resource_type) + (e.resource_name ? ' · ' + e.resource_name : '') + (e.resource_id ? ' (' + e.resource_id + ')' : '')],
        ['Result', e.status === 'failed' ? 'Failed' : 'Done']
      ].filter(function (r) { return r[1]; });
      DW.setHTML(el, html`
        <p class="au-summary">${e.summary}</p>
        <dl class="au-meta">${meta.map(function (r) { return html`<dt>${r[0]}</dt><dd>${r[1]}</dd>`; })}</dl>
        ${hasBA ? html`<h3 class="au-h">Changes</h3>${api.diffNode(e.before, e.after)}<p class="text-sm text-muted">Secrets are never stored. A changed secret shows as (changed).</p>` : html`<p class="text-sm text-muted">No before and after recorded for this action.</p>`}`);
    };

    // mountHistory(el, {resource_type, resource_id}): compact list with expandable rows.
    var histCache = {};
    api.mountHistory = function (el, filter) {
      var key = filter.resource_type + '/' + filter.resource_id;
      var cursor = '', rows = [], loading = false;
      function url(c) {
        return '/api/audit?' + buildQuery({ resource_type: filter.resource_type, resource_id: filter.resource_id, cursor: c, limit: 20 });
      }
      function paint(more, err) {
        if (err) { DW.setHTML(el, html`<p class="text-sm" role="alert">${err} <button type="button" class="btn btn-sm" data-action="auditHistRetry">Retry</button></p>`); return; }
        if (!rows.length) { DW.setHTML(el, html`<p class="text-muted text-sm">No recorded changes yet.</p>`); return; }
        DW.setHTML(el, html`<ul class="au-hist">${rows.map(function (r) {
          return html`<li class="au-hist-item"><button type="button" class="au-hist-btn" data-action="auditHistToggle" data-id="${r.id}" aria-expanded="false">
            <span class="au-hist-when">${fmtTime(r.created_at)}</span>
            <span class="au-hist-what">${r.summary || actionLabel(r.action)}</span>
            <span class="au-hist-who text-muted">${actorLabel(r.actor)}</span>
            ${r.status === 'failed' ? html`<span class="badge badge-red">Failed</span>` : ''}
          </button><div class="au-hist-detail" id="au-hd-${r.id}" hidden></div></li>`;
        })}</ul>${more ? html`<button type="button" class="btn btn-sm" data-action="auditHistMore">Older changes</button>` : ''}`);
      }
      function load(reset) {
        if (loading) return Promise.resolve();
        loading = true;
        if (reset) { cursor = ''; rows = []; }
        return fetch(url(cursor), { credentials: 'same-origin', headers: { Accept: 'application/json' } })
          .then(function (r) { if (!r.ok) throw new Error('Could not load history (' + r.status + ')'); return r.json(); })
          .then(function (d) { rows = rows.concat(d.entries || []); cursor = d.next_cursor || ''; histCache[key] = { rows: rows, cursor: cursor }; paint(!!cursor); })
          .catch(function (e) { paint(false, e.message); })
          .then(function () { loading = false; });
      }
      el.__auditHist = {
        reload: function () { return load(true); }, more: function () { return load(false); },
        toggle: function (btn) {
          var box = el.querySelector('#au-hd-' + btn.dataset.id);
          var open = btn.getAttribute('aria-expanded') === 'true';
          btn.setAttribute('aria-expanded', String(!open));
          box.hidden = open;
          if (open || box.dataset.loaded) return;
          box.dataset.loaded = '1';
          DW.setHTML(box, html`<p class="text-muted text-sm">Loading…</p>`);
          fetch('/api/audit/' + encodeURIComponent(btn.dataset.id), { credentials: 'same-origin' })
            .then(function (r) { if (!r.ok) throw new Error('failed'); return r.json(); })
            .then(function (d) { api.renderDetail(box, d); })
            .catch(function () { delete box.dataset.loaded; DW.setHTML(box, html`<p class="text-sm" role="alert">Could not load this entry.</p>`); });
        }
      };
      if (histCache[key]) { rows = histCache[key].rows; cursor = histCache[key].cursor; paint(!!cursor); }
      else DW.setHTML(el, html`<p class="text-muted text-sm" role="status">Loading history…</p>`);
      return load(true);
    };
    function host(el) { var h = el.closest('[data-audit-history]'); return h && h.__auditHist; }
    DW.actions.auditHistToggle = function (el) { var h = host(el); if (h) h.toggle(el); };
    DW.actions.auditHistMore = function (el) { var h = host(el); if (h) h.more(); };
    DW.actions.auditHistRetry = function (el) { var h = host(el); if (h) h.reload(); };
  }

  root.AUDIT = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof window !== 'undefined' ? window : globalThis);
