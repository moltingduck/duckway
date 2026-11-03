/*
 * palette.js - Ctrl/Cmd-K command palette. Requires dw.js (loaded after shell.js).
 *
 * ARIA 1.2 combobox + listbox: the text input keeps DOM focus, the highlighted
 * option is exposed with aria-activedescendant. Pages come from the sidebar
 * nav; entities come from GET /api/search?q= (names only, debounced, stale
 * responses dropped). Every name is rendered through DW.html (escaped) and
 * navigation uses only same-origin paths returned by the server.
 */
(function () {
  'use strict';
  var root = document.getElementById('palette');
  var input = document.getElementById('pal-input');
  var list = document.getElementById('pal-list');
  var emptyEl = document.getElementById('pal-empty');
  var statusEl = document.getElementById('pal-status');
  var kbd = document.getElementById('pal-kbd');
  if (!root || !input || !list) return;

  var KIND = {
    page: 'Page', client: 'Client', credential: 'Credential', key_pool: 'Key pool',
    bundle: 'Bundle', phantom_token: 'Phantom token', service: 'Service', notification: 'Notification'
  };
  var DEBOUNCE_MS = 200;

  var isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent || '');
  if (kbd) kbd.textContent = isMac ? '⌘K' : 'Ctrl K';

  function pagesFromNav() {
    var out = [];
    document.querySelectorAll('#sidebar .nav-group').forEach(function (g) {
      var label = g.querySelector('.nav-group-label');
      var group = label ? label.textContent.trim() : '';
      g.querySelectorAll('a[href]').forEach(function (a) {
        var c = a.cloneNode(true);
        c.querySelectorAll('.nav-badge').forEach(function (n) { n.remove(); });
        var name = c.textContent.replace(/\s+/g, ' ').trim();
        if (name) out.push({ kind: 'page', id: a.getAttribute('href'), name: name, url: a.getAttribute('href'), group: group });
      });
    });
    return out;
  }
  var PAGES = pagesFromNav();

  var items = [];      // currently rendered options
  var active = -1;     // index into items
  var entities = [];   // latest entity hits for the current query
  var seq = 0;         // request counter: only the newest response is applied
  var timer = null;
  var ctl = null;
  var entityState = ''; // '', 'loading', 'error'

  function safeURL(u) { return typeof u === 'string' && u.charAt(0) === '/' && u.charAt(1) !== '/' && u.indexOf('\\') < 0; }

  function matchPages(q) {
    var n = q.trim().toLowerCase();
    if (!n) return PAGES.slice();
    return PAGES.filter(function (p) { return (p.name + ' ' + p.group).toLowerCase().indexOf(n) >= 0; });
  }

  function setActive(i, scroll) {
    if (active >= 0 && items[active]) {
      var prev = document.getElementById('pal-opt-' + active);
      if (prev) prev.setAttribute('aria-selected', 'false');
    }
    active = i;
    if (i >= 0 && items[i]) {
      var el = document.getElementById('pal-opt-' + i);
      if (el) {
        el.setAttribute('aria-selected', 'true');
        input.setAttribute('aria-activedescendant', el.id);
        if (scroll !== false && el.scrollIntoView) el.scrollIntoView({ block: 'nearest' });
      }
    } else {
      input.removeAttribute('aria-activedescendant'); // never an empty reference
    }
  }

  // keepActive: results of the SAME query arrived (entities), so the highlighted
  // item stays highlighted if it is still listed; a new query starts at the top.
  function render(keepActive) {
    var q = input.value;
    var prevKey = keepActive && active >= 0 && items[active] ? items[active].kind + '|' + items[active].url : '';
    var pages = matchPages(q);
    items = pages.concat(entities);
    DW.setHTML(list, DW.html`${items.map(function (it, i) {
      var meta = it.kind === 'page' ? it.group : '';
      return DW.html`<li class="pal-opt" id="${'pal-opt-' + i}" role="option" aria-selected="false" data-idx="${i}"><span class="pal-kind">${KIND[it.kind] || it.kind}</span><span class="pal-name">${it.name}</span>${meta ? DW.html`<span class="pal-meta">${meta}</span>` : ''}</li>`;
    })}`);
    emptyEl.hidden = items.length > 0 || entityState === 'loading';
    input.setAttribute('aria-expanded', items.length > 0 ? 'true' : 'false');
    var keepIdx = -1;
    if (prevKey) items.forEach(function (it, i) { if (keepIdx < 0 && it.kind + '|' + it.url === prevKey) keepIdx = i; });
    setActive(items.length ? (keepIdx >= 0 ? keepIdx : 0) : -1, false);
    var msg;
    if (entityState === 'error') msg = items.length + ' results. Item search is unavailable.';
    else if (entityState === 'loading') msg = 'Searching';
    else msg = items.length === 1 ? '1 result' : items.length + ' results';
    statusEl.textContent = msg;
  }

  function search() {
    var q = input.value.trim();
    if (timer) { clearTimeout(timer); timer = null; }
    if (ctl) { ctl.abort(); ctl = null; }
    entities = [];
    entityState = '';
    seq++;
    if (!q) { render(); return; }
    var mine = seq;
    entityState = 'loading';
    render();
    timer = setTimeout(function () {
      timer = null;
      ctl = typeof AbortController === 'function' ? new AbortController() : null;
      fetch('/api/search?q=' + encodeURIComponent(q), { credentials: 'same-origin', signal: ctl ? ctl.signal : undefined })
        .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
        .then(function (data) {
          if (mine !== seq) return; // stale: a newer query is in flight or the palette closed
          entities = (data.results || []).filter(function (r) { return r && safeURL(r.url) && typeof r.name === 'string'; });
          entityState = '';
          render(true);
        })
        .catch(function (err) {
          if (mine !== seq || (err && err.name === 'AbortError')) return;
          entities = [];
          entityState = 'error';
          render(true);
        });
    }, DEBOUNCE_MS);
  }

  function go(i) {
    var it = items[i];
    if (!it || !safeURL(it.url)) return;
    DW.modal.close(root);
    window.location.assign(it.url);
  }

  function open() {
    if (root.style.display === 'flex' && !root.hidden) return;
    input.value = '';
    entities = []; entityState = ''; seq++;
    render();
    DW.modal.open(root);
    input.focus();
  }
  function close() { DW.modal.close(root); }

  // Reset in-flight work whenever the dialog closes (Esc, backdrop, button, navigation).
  new MutationObserver(function () {
    if (root.style.display === 'none' || root.hidden) {
      seq++;
      if (timer) { clearTimeout(timer); timer = null; }
      if (ctl) { ctl.abort(); ctl = null; }
    }
  }).observe(root, { attributes: true, attributeFilter: ['style', 'hidden'] });

  DW.actions.openPalette = function () { open(); };

  document.addEventListener('keydown', function (e) {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && (e.key === 'k' || e.key === 'K')) {
      e.preventDefault();
      if (root.style.display === 'flex' && !root.hidden) close(); else open();
    }
  });

  input.addEventListener('input', search);
  input.addEventListener('keydown', function (e) {
    if (e.isComposing) return;
    var n = items.length;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (n) setActive(active < 0 ? 0 : (active + 1) % n);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (n) setActive(active <= 0 ? n - 1 : active - 1);
    } else if (e.key === 'Home' && e.ctrlKey) {
      e.preventDefault(); if (n) setActive(0);
    } else if (e.key === 'End' && e.ctrlKey) {
      e.preventDefault(); if (n) setActive(n - 1);
    } else if (e.key === 'Enter') {
      e.preventDefault();
      if (active >= 0) go(active);
    }
  });

  list.addEventListener('mousemove', function (e) {
    var li = e.target instanceof Element ? e.target.closest('[role=option]') : null;
    if (li && Number(li.dataset.idx) !== active) setActive(Number(li.dataset.idx), false);
  });
  list.addEventListener('click', function (e) {
    var li = e.target instanceof Element ? e.target.closest('[role=option]') : null;
    if (li) go(Number(li.dataset.idx));
  });

  render();
})();
