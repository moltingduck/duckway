/*
 * dw.js - Duckway console foundation. Plain ES2020, no dependencies.
 * Load FIRST (before other static scripts). Every <script> needs nonce="{{$.CSPNonce}}".
 *
 * USAGE
 *   Escaping:    DW.esc(value)                      -> escaped string (null/undefined -> '')
 *   Markup:      const m = DW.html`<b>${name}</b>`  -> SafeHTML; interpolations are escaped,
 *                nested SafeHTML (and arrays of values) are inserted as-is/each escaped.
 *                DW.raw('<i>trusted</i>') marks a trusted literal (never use with data).
 *                String(m) / m.toString() gives the markup string.
 *   Insertion:   DW.setHTML(el, m)                  -> only accepts SafeHTML (throws otherwise).
 *                This is the ONLY place innerHTML is written, through the single Trusted Types
 *                policy `dw`. Prefer textContent when possible.
 *   Events:      NO inline on*= attributes. Use delegated attributes, handler names registered:
 *                  DW.actions.deleteKey = (el, event) => {...};   // el = element carrying attr
 *                  <button data-action="deleteKey" data-id="{{.ID}}">     (click)
 *                  <select data-on-change="pick">  <input data-on-input="filter">
 *                  <form data-on-submit="save">    (preventDefault is called for you)
 *                Read parameters from el.dataset (data-id, data-name...). Handlers may be async.
 *                click on an <a data-action> also gets preventDefault.
 *                Built-ins: data-action="modal-open" data-target="#id"; data-action="modal-close"
 *                (closest .modal-backdrop); clicking a .modal-backdrop itself closes it.
 *   Keyboard:    elements with data-action AND role="button" (tabindex=0) activate on Enter/Space.
 *   Toast:       DW.toast('Saved', 'success'|'error'|''); window.showToast is an alias.
 *   Modals:      DW.modal.open(elOrSelector) / DW.modal.close(elOrSelector). Works on .modal-backdrop
 *                (display toggled). Esc closes the topmost, Tab is trapped, focus returns to the
 *                opener. Legacy modals toggled via style.display are tracked too.
 */
(function () {
  'use strict';

  class SafeHTML {
    constructor(s) { this.__html = String(s); Object.freeze(this); }
    toString() { return this.__html; }
  }

  const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;', '`': '&#96;' };
  function esc(v) {
    if (v === null || v === undefined) return '';
    if (v instanceof SafeHTML) return v.__html;
    return String(v).replace(/[&<>"'`]/g, (c) => ESC[c]);
  }

  function part(v) {
    if (v instanceof SafeHTML) return v.__html;
    if (Array.isArray(v)) return v.map(part).join('');
    return esc(v);
  }

  function html(strings, ...vals) {
    let out = strings[0];
    for (let i = 0; i < vals.length; i++) out += part(vals[i]) + strings[i + 1];
    return new SafeHTML(out);
  }

  function raw(s) { return new SafeHTML(s); }

  // Trusted Types: the page CSP says `require-trusted-types-for 'script'; trusted-types dw`.
  // This is the one and only policy (no default policy). Its createHTML is an identity
  // function because the only caller, setHTML, accepts nothing but SafeHTML, whose
  // interpolations were already escaped by DW.html.
  let policy = null;
  if (window.trustedTypes && typeof window.trustedTypes.createPolicy === 'function') {
    try { policy = window.trustedTypes.createPolicy('dw', { createHTML: (s) => s }); } catch (e) { policy = null; }
  }

  function setHTML(el, safe) {
    if (!(safe instanceof SafeHTML)) throw new TypeError('DW.setHTML requires SafeHTML (use DW.html)');
    el.innerHTML = policy ? policy.createHTML(safe.__html) : safe.__html;
    if (typeof labelControls === 'function' && el.querySelector) labelControls(el);
    return el;
  }

  /* ---- delegated actions ---- */
  const actions = Object.create(null);
  const EVENTS = [
    ['click', 'data-action'],
    ['change', 'data-on-change'],
    ['input', 'data-on-input'],
    ['submit', 'data-on-submit'],
  ];
  function dispatch(type, attr, e) {
    const start = e.target instanceof Element ? e.target : null;
    const el = start && start.closest('[' + attr + ']');
    if (!el) return;
    const name = el.getAttribute(attr);
    const fn = actions[name];
    if (typeof fn !== 'function') { console.error('DW: no action registered for "' + name + '"'); return; }
    if (type === 'submit' || (type === 'click' && el.tagName === 'A')) e.preventDefault();
    try {
      const r = fn(el, e);
      if (r && typeof r.catch === 'function') r.catch((err) => console.error('DW action ' + name + ':', err));
    } catch (err) { console.error('DW action ' + name + ':', err); }
  }
  EVENTS.forEach(([type, attr]) => document.addEventListener(type, (e) => dispatch(type, attr, e)));

  /* Keyboard activation for non-native controls: <tr|div|span data-action role="button" tabindex="0">. */
  document.addEventListener('keydown', (e) => {
    if (e.repeat) return;
    if (e.key !== 'Enter' && e.key !== ' ') return;
    const t = e.target;
    if (!(t instanceof Element) || !t.matches('[data-action][role="button"]')) return;
    e.preventDefault();
    dispatch('click', 'data-action', e);
  });

  /* ---- toast ---- */
  function toast(msg, type) {
    let box = document.getElementById('toast');
    if (!box) {
      box = document.createElement('div');
      box.id = 'toast';
      box.setAttribute('role', 'status');
      box.setAttribute('aria-live', 'polite');
      document.body.appendChild(box);
    }
    const el = document.createElement('div');
    el.className = 'toast-msg ' + (type || '');
    // announced once via the polite live region; no per-message role=alert
    el.textContent = msg;
    box.appendChild(el);
    setTimeout(() => el.remove(), 4000);
    return el;
  }

  /* ---- modals ---- */
  const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]):not([type=hidden]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';
  const state = new WeakMap(); // backdrop -> {opener}
  const stack = [];

  function resolve(t) {
    if (typeof t !== 'string') return t;
    try { return document.querySelector(t) || document.getElementById(t); } catch (e) { return document.getElementById(t); }
  }
  function isShown(b) { return b.isConnected && b.style.display !== 'none' && !b.hidden && getComputedStyle(b).display !== 'none'; }
  function focusables(b) { return Array.from(b.querySelectorAll(FOCUSABLE)).filter((x) => x.offsetParent !== null || x === document.activeElement); }

  let uidN = 0;
  function uid(prefix) { uidN += 1; return prefix + '-' + uidN; }

  /* Associate <label> (without for) with the single control in its .form-group, when unambiguous. */
  const CONTROL = 'input:not([type=hidden]):not([type=checkbox]):not([type=radio]),select,textarea';
  function labelControls(root) {
    root.querySelectorAll('.form-group > label:not([for])').forEach((l) => {
      if (l.querySelector(CONTROL)) return; // implicit association
      const controls = l.parentElement.querySelectorAll(CONTROL);
      if (controls.length !== 1) return;
      const c = controls[0];
      if (!c.id) c.id = uid('dw-field');
      if (document.querySelectorAll('[id="' + c.id + '"]').length > 1) return;
      l.setAttribute('for', c.id);
    });
  }
  document.addEventListener('DOMContentLoaded', () => labelControls(document));
  window.addEventListener('load', () => labelControls(document));

  let loaded = document.readyState === 'complete';
  window.addEventListener('load', () => { loaded = true; });
  function activate(b) {
    if (state.has(b)) return;
    state.set(b, { opener: document.activeElement });
    stack.push(b);
    const dlg = b.querySelector('.modal');
    if (dlg) {
      dlg.setAttribute('role', 'dialog'); dlg.setAttribute('aria-modal', 'true');
      if (!dlg.hasAttribute('tabindex')) dlg.setAttribute('tabindex', '-1');
      // Auto-label from the first heading. Recomputed on every activation when we set it
      // (data-dw-labelled), because some pages rebuild the dialog body each time it opens.
      const auto = dlg.hasAttribute('data-dw-labelled');
      const cur = dlg.getAttribute('aria-labelledby');
      const stale = auto && (!cur || !dlg.querySelector('[id="' + cur + '"]'));
      if (stale || (!cur && !dlg.hasAttribute('aria-label'))) {
        const h = dlg.querySelector('h2,h3');
        if (h) {
          if (!h.id) h.id = uid('dw-modal-title');
          dlg.setAttribute('aria-labelledby', h.id);
          dlg.setAttribute('data-dw-labelled', '');
        } else if (stale) {
          dlg.removeAttribute('aria-labelledby');
        }
      }
      labelControls(dlg);
    }
    if (!loaded) return; // visible at page load: track for Esc/Tab but do not steal focus
    const f = focusables(b);
    const target = b.querySelector('[autofocus]') || f[0] || dlg || b;
    if (target && target.focus) target.focus();
  }

  function deactivate(b) {
    const s = state.get(b);
    if (!s) return;
    state.delete(b);
    const i = stack.indexOf(b);
    if (i >= 0) stack.splice(i, 1);
    if (s.opener && s.opener.isConnected && s.opener.focus) s.opener.focus();
  }

  const modal = {
    open(t) {
      const b = resolve(t);
      if (!b) return;
      b.hidden = false;
      b.style.display = 'flex';
      activate(b);
    },
    close(t) {
      const b = resolve(t);
      if (!b) return;
      b.style.display = 'none';
      deactivate(b);
    },
  };

  function sync(b) { if (isShown(b)) activate(b); else deactivate(b); }

  function eachBackdrop(n, fn) {
    if (n.nodeType !== 1) return; // skip text/comment nodes
    if (n.classList.contains('modal-backdrop')) fn(n);
    if (!n.firstElementChild) return; // childless: nothing nested
    const found = n.getElementsByClassName('modal-backdrop');
    for (let k = 0; k < found.length; k++) fn(found[k]);
  }

  new MutationObserver((muts) => {
    for (const m of muts) {
      if (m.type === 'attributes') {
        if (m.target.classList && m.target.classList.contains('modal-backdrop')) sync(m.target);
      } else {
        m.addedNodes.forEach((n) => eachBackdrop(n, sync));
        m.removedNodes.forEach((n) => eachBackdrop(n, (x) => { if (state.has(x)) deactivate(x); }));
      }
    }
  }).observe(document.documentElement, { attributes: true, attributeFilter: ['style', 'hidden'], childList: true, subtree: true });

  document.addEventListener('keydown', (e) => {
    const top = stack[stack.length - 1];
    if (!top) return;
    if (e.key === 'Escape') { e.preventDefault(); modal.close(top); return; }
    if (e.key === 'Tab') {
      const f = focusables(top);
      if (!f.length) { e.preventDefault(); return; }
      const first = f[0], last = f[f.length - 1];
      if (!top.contains(document.activeElement)) { e.preventDefault(); first.focus(); }
      else if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
  });

  document.addEventListener('mousedown', (e) => {
    if (e.target instanceof Element && e.target.classList.contains('modal-backdrop') && state.has(e.target)) modal.close(e.target);
  });

  actions['modal-open'] = (el) => modal.open(el.dataset.target);
  actions['modal-close'] = (el) => { const b = el.closest('.modal-backdrop'); if (b) modal.close(b); };

  window.DW = { esc, html, raw, setHTML, SafeHTML, actions, toast, modal };
  window.showToast = toast;
})();
