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
 *                DW.toast(msg, {undo: fn, kind, undoLabel, duration}) adds a keyboard-reachable Undo button.
 *   Confirm:     DW.confirm({title, body, confirmLabel, cancelLabel, danger}) -> Promise<boolean>
 *                (in-page dialog; focus starts on Cancel; Esc / backdrop = false).
 *   More menu:   <details class="more-menu"> (see TASK contract); Esc/outside click close, arrows move.
 *   Term hint:   <button class="term-hint" data-term="credential">; DW.terms holds the definitions.
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
  // toast(msg)                      plain message, auto-dismisses after 4s
  // toast(msg, 'success'|'error')   kind as 2nd argument (legacy)
  // toast(msg, {kind, undo, undoLabel, duration})
  //   undo: fn called (once) when the Undo button is pressed; the toast then closes.
  //   duration: ms before auto-dismiss (default 4000, 8000 with undo, 0 = until dismissed).
  //   The timer pauses while the pointer or keyboard focus is inside the toast.
  // Returns the toast element; el.dismiss() removes it, el.setText(s) replaces the text.
  const isMacPlatform = () => /mac|iphone|ipad/i.test((navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || navigator.userAgent || '');
  // Ctrl/Cmd+Z runs the newest visible Undo, unless focus is in a field where
  // the key means "undo typing".
  document.addEventListener('keydown', (e) => {
    if (!((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && (e.key === 'z' || e.key === 'Z'))) return;
    if (e.repeat) return; // holding the keys must not fire Undo repeatedly
    // Never undo "behind" an open dialog (the dialog's own context wins). A surface that
    // is itself the place where the undoable change was made (a drawer) opts in with data-undo-ok.
    const top = stack[stack.length - 1];
    if (top && !top.hasAttribute('data-undo-ok')) return;
    if (!top && document.querySelector('.modal-backdrop:not([hidden])[style*="flex"]')) return;
    const a = document.activeElement;
    const ab = a && a.closest && a.closest('.modal-backdrop');
    if (ab && !ab.hasAttribute('data-undo-ok')) return;
    if (a && (a.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName))) return;
    const btns = document.querySelectorAll('#toast .toast-undo');
    if (!btns.length) return;
    e.preventDefault();
    btns[btns.length - 1].click();
  });

  // "Skip to notifications" exists only while a toast does. It is a button that moves
  // focus without touching the URL hash (pages such as Settings use the hash for state).
  function syncSkipToast() {
    const b = document.getElementById('skip-toast');
    const box = document.getElementById('toast');
    if (b) b.hidden = !(box && box.children.length);
  }
  actions.skipToToast = () => {
    const box = document.getElementById('toast');
    if (!box) return;
    const f = box.querySelector('button') || box;
    f.focus();
  };

  function toast(msg, opts) {
    let box = document.getElementById('toast');
    if (!box) {
      box = document.createElement('div');
      box.id = 'toast';
      box.setAttribute('role', 'status');
      box.setAttribute('aria-live', 'polite');
      document.body.appendChild(box);
    }
    const o = (opts && typeof opts === 'object') ? opts : { kind: opts };
    const hasUndo = typeof o.undo === 'function';
    const el = document.createElement('div');
    el.className = 'toast-msg ' + (o.kind || '');
    // announced once via the polite live region; no per-message role=alert
    const text = document.createElement('span');
    text.className = 'toast-text';
    text.textContent = msg;
    el.appendChild(text);
    let timer = null;
    let done = false;
    const dur = typeof o.duration === 'number' ? o.duration : (hasUndo ? 8000 : 4000);
    function dismiss() {
      if (done) return;
      done = true;
      if (timer) clearTimeout(timer);
      // keep keyboard focus somewhere sensible when the focused button disappears
      const hadFocus = el.contains(document.activeElement);
      el.remove();
      syncSkipToast();
      if (hadFocus && document.body) document.body.focus && document.body.focus();
    }
    function arm() { if (dur > 0 && !done) { if (timer) clearTimeout(timer); timer = setTimeout(dismiss, dur); } }
    function hold() { if (timer) { clearTimeout(timer); timer = null; } }
    if (hasUndo) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'btn btn-sm toast-undo';
      b.textContent = o.undoLabel || ('Undo (' + (isMacPlatform() ? '\u2318Z' : 'Ctrl Z') + ')');
      b.addEventListener('click', () => {
        const fn = o.undo;
        dismiss();
        try {
          const r = fn();
          if (r && typeof r.catch === 'function') r.catch((err) => { console.error('DW toast undo:', err); toast('Undo failed', { kind: 'error' }); });
        } catch (err) { console.error('DW toast undo:', err); toast('Undo failed', { kind: 'error' }); }
      });
      el.appendChild(b);
    }
    if (o.dismiss) {
      const x = document.createElement('button');
      x.type = 'button';
      x.className = 'btn btn-sm toast-dismiss';
      x.textContent = 'Dismiss';
      x.addEventListener('click', () => dismiss());
      el.appendChild(x);
    }
    el.addEventListener('mouseenter', hold);
    el.addEventListener('mouseleave', arm);
    el.addEventListener('focusin', hold);
    el.addEventListener('focusout', arm);
    el.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.stopPropagation(); dismiss(); } });
    el.dismiss = dismiss;
    el.setText = (s) => { text.textContent = s; };
    box.appendChild(el);
    syncSkipToast();
    arm();
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

  /* ---- in-page confirm ---- */
  function confirmDialog(opts) {
    const o = opts || {};
    return new Promise((resolve) => {
      const bd = document.createElement('div');
      bd.className = 'modal-backdrop';
      bd.hidden = true;
      const body = o.body === undefined || o.body === null ? '' : o.body;
      setHTML(bd, html`<div class="modal dw-confirm">
        <h2>${o.title || 'Are you sure?'}</h2>
        ${body === '' ? '' : html`<p class="dw-confirm-body">${body}</p>`}
        <div class="modal-actions">
          <button type="button" class="btn" data-dw-confirm="cancel" autofocus>${o.cancelLabel || 'Cancel'}</button>
          <button type="button" class="btn ${o.danger ? 'btn-danger' : 'btn-primary'}" data-dw-confirm="ok">${o.confirmLabel || 'Confirm'}</button>
        </div>
      </div>`);
      document.body.appendChild(bd);
      let settled = false;
      function finish(v) {
        if (settled) return;
        settled = true;
        obs.disconnect();
        modal.close(bd);
        bd.remove();
        resolve(v);
      }
      // Esc and backdrop clicks close through modal.close (display:none): treat as cancel.
      const obs = new MutationObserver(() => { if (bd.style.display === 'none' || bd.hidden) finish(false); });
      obs.observe(bd, { attributes: true, attributeFilter: ['style', 'hidden'] });
      bd.addEventListener('click', (e) => {
        const t = e.target instanceof Element ? e.target.closest('[data-dw-confirm]') : null;
        if (t) finish(t.getAttribute('data-dw-confirm') === 'ok');
      });
      modal.open(bd);
    });
  }

  /* ---- glossary term hints ---- */
  const TERMS = {
    'credential': { term: 'Credential', def: 'A real upstream secret stored encrypted on the server: static API key, refreshable OAuth token, or GitHub App.' },
    'phantom-token': { term: 'Phantom token', def: 'The stand-in a client sees. The gateway swaps it for the real credential and enforces its ACL and approval rule.' },
    'key-pool': { term: 'Key pool (was Key Group)', def: 'Several credentials of one service rotated by a strategy: score, round-robin, failover or random.' },
    'key-bundle': { term: 'Key bundle (was Key Suite)', def: 'One credential or pool per service. Assigning a bundle to a client creates its phantom tokens in one step.' },
    'control-channel': { term: 'Control channel', def: 'A Discord bot + category bound to one client so its agent can be driven from chat. The bot token never leaves the server.' },
    'acl-layers': { term: 'ACL layers', def: 'Service \u2192 credential \u2192 phantom token. Each layer can only narrow what the one above allows.' },
  };
  let termPop = null;
  let termBtn = null;
  function placeFixed(box, anchor, gap) {
    const vw = document.documentElement.clientWidth;
    const vh = document.documentElement.clientHeight;
    const r = anchor.getBoundingClientRect();
    box.style.maxHeight = (vh - 16) + 'px';
    const w = box.offsetWidth, h = box.offsetHeight;
    let left = Math.min(Math.max(8, r.left), Math.max(8, vw - w - 8));
    let top = r.bottom + gap;
    if (top + h > vh - 8) top = r.top - h - gap;
    if (top < 8) top = 8;
    box.style.left = left + 'px';
    box.style.top = top + 'px';
  }
  function closeTerm(returnFocus) {
    if (!termPop) return;
    const b = termBtn;
    termPop.remove();
    termPop = null; termBtn = null;
    if (b) {
      b.setAttribute('aria-expanded', 'false');
      b.removeAttribute('aria-describedby');
      if (returnFocus && b.isConnected) b.focus();
    }
  }
  function openTerm(btn) {
    const t = TERMS[btn.getAttribute('data-term')];
    if (!t) return;
    closeTerm(false);
    const pop = document.createElement('div');
    pop.className = 'term-pop';
    pop.id = uid('dw-term');
    pop.setAttribute('role', 'tooltip');
    const h = document.createElement('strong');
    h.className = 'term-pop-title';
    h.textContent = t.term;
    const p = document.createElement('span');
    p.className = 'term-pop-def';
    p.textContent = t.def;
    pop.appendChild(h); pop.appendChild(p);
    document.body.appendChild(pop);
    termPop = pop; termBtn = btn;
    btn.setAttribute('aria-expanded', 'true');
    btn.setAttribute('aria-describedby', pop.id);
    placeFixed(pop, btn, 6);
  }
  document.addEventListener('click', (e) => {
    const t = e.target instanceof Element ? e.target.closest('.term-hint') : null;
    if (t) {
      e.preventDefault();
      if (termBtn === t) closeTerm(false); else openTerm(t);
      return;
    }
    if (termPop && !(e.target instanceof Element && e.target.closest('.term-pop'))) closeTerm(false);
  }, true);
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && termPop) { e.preventDefault(); e.stopPropagation(); closeTerm(true); }
  }, true);
  function repositionTerm() { if (termPop && termBtn) placeFixed(termPop, termBtn, 6); }
  window.addEventListener('resize', repositionTerm);
  window.addEventListener('scroll', repositionTerm, true);

  /* ---- More menu ---- */
  function menuItems(d) { return Array.from(d.querySelectorAll('.more-menu-list [role="menuitem"]:not([disabled])')); }
  function positionMenu(d) {
    const list = d.querySelector('.more-menu-list');
    const sum = d.querySelector('summary');
    if (list && sum) placeFixed(list, sum, 4);
    // right-align under the summary when there is room
    if (list && sum) {
      const vw = document.documentElement.clientWidth;
      const r = sum.getBoundingClientRect();
      const w = list.offsetWidth;
      list.style.left = Math.min(Math.max(8, r.right - w), Math.max(8, vw - w - 8)) + 'px';
    }
  }
  function closeMenu(d, returnFocus) {
    if (!d.open) return;
    d.open = false;
    const sum = d.querySelector('summary');
    if (sum) sum.setAttribute('aria-expanded', 'false');
    if (returnFocus && sum) sum.focus();
  }
  function closeMenus(except) {
    document.querySelectorAll('details.more-menu[open]').forEach((d) => { if (d !== except) closeMenu(d, false); });
  }
  // Every More menu announces itself as a popup from first paint, not only after it was opened.
  function initMenus(root) {
    (root || document).querySelectorAll('details.more-menu').forEach((d) => {
      const sum = d.querySelector('summary');
      if (sum && !sum.hasAttribute('aria-haspopup')) { sum.setAttribute('aria-haspopup', 'menu'); sum.setAttribute('aria-expanded', d.open ? 'true' : 'false'); }
      const list = d.querySelector('.more-menu-list');
      if (list && !list.hasAttribute('role')) list.setAttribute('role', 'menu');
    });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => initMenus());
  else initMenus();
  // Rescan only when an added node is, or contains, a More menu (not on every DOM change).
  new MutationObserver((muts) => {
    for (const m of muts) {
      for (const n of m.addedNodes) {
        if (n.nodeType === 1 && ((n.matches && n.matches('details.more-menu')) || (n.querySelector && n.querySelector('details.more-menu')))) { initMenus(); return; }
      }
    }
  }).observe(document.documentElement, { childList: true, subtree: true });
  document.addEventListener('toggle', (e) => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement) || !d.classList.contains('more-menu')) return;
    const sum = d.querySelector('summary');
    const list = d.querySelector('.more-menu-list');
    if (sum) { sum.setAttribute('aria-haspopup', 'menu'); sum.setAttribute('aria-expanded', d.open ? 'true' : 'false'); }
    if (list && !list.hasAttribute('role')) list.setAttribute('role', 'menu');
    if (!d.open) { d.classList.remove('is-placed'); return; }
    closeMenus(d);
    closeTerm(false);
    positionMenu(d);
    d.classList.add('is-placed');
    const items = menuItems(d);
    if (items[0]) items[0].focus({ preventScroll: true });
  }, true);
  document.addEventListener('mousedown', (e) => {
    const t = e.target instanceof Element ? e.target : null;
    document.querySelectorAll('details.more-menu[open]').forEach((d) => { if (!t || !d.contains(t)) closeMenu(d, false); });
  });
  document.addEventListener('focusin', (e) => {
    const t = e.target instanceof Element ? e.target : null;
    document.querySelectorAll('details.more-menu[open]').forEach((d) => { if (!t || !d.contains(t)) closeMenu(d, false); });
  });
  // Run before the delegated data-action dispatch so the opener (for modals) is the
  // visible summary, not a menu item that is about to be hidden.
  document.addEventListener('click', (e) => {
    const t = e.target instanceof Element ? e.target.closest('.more-menu-list [role="menuitem"]') : null;
    if (!t) return;
    const d = t.closest('details.more-menu');
    if (d) closeMenu(d, true);
  }, true);
  document.addEventListener('keydown', (e) => {
    const t = e.target instanceof Element ? e.target : null;
    const d = t && t.closest('details.more-menu');
    if (!d) return;
    if (e.key === 'Escape' && d.open) { e.preventDefault(); e.stopPropagation(); closeMenu(d, true); return; }
    const sum = d.querySelector('summary');
    if (t === sum && (e.key === 'ArrowDown' || e.key === 'ArrowUp') && !d.open) {
      e.preventDefault();
      d.open = true;
      return;
    }
    if (!d.open || !t.closest('.more-menu-list')) return;
    const items = menuItems(d);
    const i = items.indexOf(t);
    let n = -1;
    if (e.key === 'ArrowDown') n = (i + 1) % items.length;
    else if (e.key === 'ArrowUp') n = (i - 1 + items.length) % items.length;
    else if (e.key === 'Home') n = 0;
    else if (e.key === 'End') n = items.length - 1;
    else if (e.key === 'Tab') { if (sum) sum.focus(); closeMenu(d, false); return; }
    if (n >= 0 && items[n]) { e.preventDefault(); items[n].focus({ preventScroll: true }); }
  }, true);
  window.addEventListener('resize', repositionMenus);
  // Scrolling or resizing keeps the open menu attached to its summary.
  function repositionMenus() { document.querySelectorAll('details.more-menu[open]').forEach(positionMenu); }
  window.addEventListener('scroll', (e) => {
    if (e.target instanceof Element && e.target.closest && e.target.closest('.more-menu-list')) return;
    repositionMenus();
  }, true);

  modal.confirm = confirmDialog;
  window.DW = { esc, html, raw, setHTML, SafeHTML, actions, toast, modal, confirm: confirmDialog, terms: TERMS };
  window.showToast = toast;
})();
