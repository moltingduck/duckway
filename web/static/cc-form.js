/**
 * Shared control-channel form (DW.ccForm).
 *
 * Used by the Add client wizard (step 3) and the Control channels drawer.
 * It renders the form parts that both pages need and holds the pure helpers
 * for the typed key/value settings and secret masking. It makes no API calls
 * and keeps no state: each host owns its state, its flow (wizard drafts,
 * drawer save/test/verify) and the DW.actions handlers named in the options.
 *
 * Secrets: the server never sends a credential value. A masked setting
 * arrives as MASK; the editor shows an empty password input and, when the
 * input stays empty, kvApply writes MASK back so the server restores the
 * stored value. No secret is ever placed in the DOM.
 *
 * Markup is built only with DW.html / DW.raw (Trusted Types). Handlers are
 * data-action / data-on-* names supplied by the host.
 */
(function () {
  'use strict';

  var AGENTS = [['claude_code', 'Claude Code'], ['codex', 'Codex'], ['openclaw', 'OpenClaw']];
  var SANDBOXES = ['workspace-write', 'read-only', 'danger-full-access', 'none'];
  var MASK = '********';
  var SECRET_RE = /token|secret|password|passwd|apikey|api_key|credential|authorization|bearer|private|signing|_key$|^key$/i;
  var NAME_RE = /^[A-Za-z][A-Za-z0-9_-]*$/;

  function selAttr(b) { return b ? DW.raw(' selected') : ''; }
  function disAttr(b) { return b ? DW.raw(' disabled') : ''; }
  function fieldAttr(f) { return f && NAME_RE.test(f) ? DW.html` data-field="${f}"` : ''; }
  function actionName(a) {
    if (!a || !NAME_RE.test(a)) throw new Error('ccForm: invalid action name');
    return a;
  }

  function agentLabel(a) {
    for (var i = 0; i < AGENTS.length; i++) if (AGENTS[i][0] === a) return AGENTS[i][1];
    return a || 'Unknown';
  }

  // field wraps one control in the standard form group.
  function field(id, label, control, hint) {
    return DW.html`<div class="form-group ccf-field"><label for="${id}">${label}</label>${control}${hint || ''}</div>`;
  }
  function select(o, options) {
    return DW.html`<select id="${o.id}"${fieldAttr(o.field)} data-on-change="${actionName(o.action)}">${options}</select>`;
  }

  // agentField: o = {id, value, action, field?, hint?}
  function agentField(o) {
    var opts = AGENTS.map(function (a) { return DW.html`<option value="${a[0]}"${selAttr(a[0] === o.value)}>${a[1]}</option>`; });
    return field(o.id, 'Agent', select(o, opts), o.hint);
  }
  // sandboxField: o = {id, value, action, field?, noneLabel?, hint?}
  function sandboxField(o) {
    var opts = SANDBOXES.map(function (v) {
      return DW.html`<option value="${v}"${selAttr(v === o.value)}>${v === 'none' && o.noneLabel ? o.noneLabel : v}</option>`;
    });
    return field(o.id, 'Codex sandbox', select(o, opts), o.hint);
  }
  // credentialField: o = {id, label, keys, value, action, field?, prompt?, empty?, hint?}
  function credentialField(o) {
    var keys = o.keys || [];
    var opts = [];
    if (o.prompt) opts.push(DW.html`<option value="">${o.prompt}</option>`);
    keys.forEach(function (k) { opts.push(DW.html`<option value="${k.id}"${selAttr(k.id === o.value)}>${k.name}</option>`); });
    if (!keys.length && o.empty) opts.push(DW.html`<option value="">${o.empty}</option>`);
    return field(o.id, o.label, select(o, opts), o.hint);
  }
  function itemOptions(items, value, showIds) {
    return (items || []).map(function (g) {
      return DW.html`<option value="${g.id}"${selAttr(g.id === value)}>${g.name}${showIds ? ' (' + g.id + ')' : ''}</option>`;
    });
  }
  // serverField / categoryField: o = {id, label, items, value, action, field?,
  //   blank?, showIds?, leading?: [{value, label}]}
  function pickerField(o) {
    var opts = [];
    if (o.blank) opts.push(DW.html`<option value="">${o.blank}</option>`);
    (o.leading || []).forEach(function (l) { opts.push(DW.html`<option value="${l.value}"${selAttr(l.value === o.value)}>${l.label}</option>`); });
    return field(o.id, o.label, select(o, opts.concat(itemOptions(o.items, o.value, o.showIds))), o.hint);
  }
  // idField: free-text fallback when the server or category cannot be listed.
  // o = {id, label, value, action, field?, placeholder?, hint?}
  function idField(o) {
    return field(o.id, o.label, DW.html`<input type="text" id="${o.id}"${fieldAttr(o.field)} data-on-input="${actionName(o.action)}" value="${o.value || ''}" placeholder="${o.placeholder || ''}" autocomplete="off">`, o.hint);
  }
  function grid(content) { return DW.html`<div class="ccf-grid">${content}</div>`; }

  /* ---------- check results ---------- */
  function stepList(steps) {
    return DW.html`<ul class="ccf-steps">${(steps || []).map(function (s) {
      return DW.html`<li class="ccf-step ${s.ok ? 'is-ok' : 'is-bad'}"><span class="ccf-step-st">${s.ok ? 'Pass' : 'Fail'}</span><span class="ccf-step-name">${s.name}</span>${s.note ? DW.html`<span class="ccf-step-note">${s.note}</span>` : ''}</li>`;
    })}</ul>`;
  }
  function resultCard(ok, title, detail, steps, extra) {
    return DW.html`<div class="ccf-card ${ok ? 'is-ok' : 'is-bad'}"><div class="ccf-card-head"><span class="badge ${ok ? 'badge-green' : 'badge-red'}">${ok ? 'Ready' : 'Needs attention'}</span><strong>${title}</strong></div>${detail ? DW.html`<p class="text-sm">${detail}</p>` : ''}${steps && steps.length ? stepList(steps) : ''}${extra || ''}</div>`;
  }
  function pending(text) {
    return DW.html`<div class="ccf-card"><div class="ccf-card-head"><span class="badge badge-gray">Working</span><strong>${text}</strong></div></div>`;
  }
  // checkRow: pill + text + steps + Grant access + Check again.
  // o = {rowId, buttonId, skipped?, check, newText?, checkAction, grantAction}
  // check is null (not checked), {busy:true}, {ok, steps} or {ok:false, error}.
  function checkRow(o) {
    var c = o.check, pill, text;
    var mute = DW.raw('<span class="ro-pill ro-pill-mute">Not checked</span>');
    if (o.skipped) { pill = mute; text = o.newText || 'The category is created with bot access when you continue.'; }
    else if (!c) { pill = mute; text = 'Check that the bot can create channels and post in this category.'; }
    else if (c.busy) { pill = DW.raw('<span class="ro-pill ro-pill-mute">Checking</span>'); text = 'Checking Discord permissions…'; }
    else if (c.ok) { pill = DW.raw('<span class="ro-pill ro-pill-ok">Ready</span>'); text = 'Bot can create channels and post in this category.'; }
    else { pill = DW.raw('<span class="ro-pill ro-pill-bad">Needs permissions</span>'); text = c.error || 'Permission check failed.'; }
    var steps = c && !c.busy && !c.ok && c.steps ? stepList(c.steps) : '';
    var fix = c && !c.busy && !c.ok ? DW.html` <button type="button" class="btn btn-sm" data-action="${actionName(o.grantAction)}">Grant access</button>` : '';
    return DW.html`<div class="ccf-check" id="${o.rowId}">${pill}<span class="ccf-check-text">${text}</span>${fix}<button type="button" class="btn btn-sm" id="${o.buttonId}" data-action="${actionName(o.checkAction)}"${disAttr(o.skipped)}>Check again</button></div>${steps}`;
  }

  /* ---------- typed key/value settings ---------- */
  // kvFromConfig turns a stored config (secrets already masked by the server)
  // into editor rows. agent_options is edited separately and skipped.
  function kvFromConfig(cfg) {
    return Object.keys(cfg || {}).filter(function (k) { return k !== 'agent_options'; }).sort().map(function (k) {
      var v = cfg[k];
      var complex = v !== null && typeof v === 'object';
      var masked = v === MASK;
      return {k: k, v: complex || masked ? '' : String(v), orig: v, masked: masked, complex: complex, existing: true};
    });
  }
  // kvApply writes the rows into cfg (mutates and returns it). A masked row
  // left empty writes MASK back so the server keeps the stored secret; typed
  // numbers and booleans keep their type. Throws on a duplicate name.
  function kvApply(cfg, rows) {
    var seen = {};
    rows.forEach(function (r) {
      var k = (r.k || '').trim();
      if (!k) return;
      if (seen[k]) throw new Error('Duplicate setting name: ' + k);
      seen[k] = true;
      if (r.complex) { cfg[k] = r.orig; return; }
      if (r.masked && r.v === '') { cfg[k] = MASK; return; }
      var v = r.v;
      if (r.existing && typeof r.orig === 'number' && v.trim() !== '' && !isNaN(Number(v))) cfg[k] = Number(v);
      else if (r.existing && typeof r.orig === 'boolean' && (v === 'true' || v === 'false')) cfg[k] = v === 'true';
      else cfg[k] = v;
    });
    Object.keys(cfg).forEach(function (k) { if (k !== 'agent_options' && !seen[k]) delete cfg[k]; });
    return cfg;
  }
  // kvFieldset: o = {rows, note?, addAction, removeAction, inputAction}
  function kvFieldset(o) {
    var rows = o.rows || [];
    var body = rows.length ? rows.map(function (r, i) {
      var secret = r.masked || SECRET_RE.test(r.k);
      var value = r.complex
        ? DW.html`<input type="text" id="kv-v${i}" value="structured value (edit in Advanced JSON)" disabled>`
        : DW.html`<input type="${secret ? 'password' : 'text'}" id="kv-v${i}" data-kv="v" data-idx="${i}" data-on-input="${actionName(o.inputAction)}" value="${r.v}" placeholder="${r.masked ? 'Stored on server, leave blank to keep' : 'value'}" autocomplete="off">`;
      return DW.html`<div class="ccf-kv-row"><label class="ccf-sr" for="kv-k${i}">Setting name ${i + 1}</label><input type="text" id="kv-k${i}" data-kv="k" data-idx="${i}" data-on-input="${actionName(o.inputAction)}" value="${r.k}" placeholder="name"${disAttr(r.existing)} autocomplete="off"><label class="ccf-sr" for="kv-v${i}">Value ${i + 1}</label>${value}<button type="button" class="btn btn-sm" data-action="${actionName(o.removeAction)}" data-idx="${i}" aria-label="Remove setting ${r.k || i + 1}">Remove</button></div>`;
    }) : DW.raw('<p class="text-muted text-sm">No settings yet.</p>');
    return DW.html`<fieldset class="ccf-set"><legend>Platform settings</legend>${o.note || ''}<div class="ccf-kv">${body}</div><button type="button" class="btn btn-sm" data-action="${actionName(o.addAction)}">Add setting</button></fieldset>`;
  }
  // kvHandlers returns the three DW.actions the host assigns for kvFieldset.
  // opts = {rows: function () -> array|null, render: function (), focusId?}
  function kvHandlers(opts) {
    return {
      add: function () {
        var rows = opts.rows(); if (!rows) return;
        rows.push({k: '', v: '', existing: false});
        opts.render();
        var n = document.getElementById('kv-k' + (rows.length - 1));
        if (n) n.focus();
      },
      remove: function (el) {
        var rows = opts.rows(); if (!rows) return;
        rows.splice(parseInt(el.dataset.idx, 10), 1);
        opts.render();
      },
      input: function (el) {
        var rows = opts.rows(); if (!rows) return;
        var r = rows[parseInt(el.dataset.idx, 10)];
        if (r) r[el.dataset.kv] = el.value;
      }
    };
  }

  window.DW = window.DW || {};
  DW.ccForm = {
    AGENTS: AGENTS, SANDBOXES: SANDBOXES, MASK: MASK, SECRET_RE: SECRET_RE,
    agentLabel: agentLabel,
    agentField: agentField, sandboxField: sandboxField, credentialField: credentialField,
    serverField: pickerField, categoryField: pickerField, idField: idField, grid: grid,
    stepList: stepList, resultCard: resultCard, pending: pending, checkRow: checkRow,
    kvFromConfig: kvFromConfig, kvApply: kvApply, kvFieldset: kvFieldset, kvHandlers: kvHandlers
  };
})();
