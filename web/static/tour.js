/*
 * tour.js - guided shell tour ("Show me around"). Requires dw.js. Exposes DW.tour.start().
 * Anchored, non-modal popover (role=dialog) over real shell landmarks. Steps whose
 * target is missing are skipped. No persistence. Esc ends the tour; focus returns
 * to where it was. On narrow screens the sidebar drawer is opened for sidebar steps
 * and closed again when the tour ends.
 */
(function () {
  'use strict';
  var STEPS = [
    { sel: '#sidebar', title: 'Navigation groups', text: 'Pages are grouped by job: Overview, Fleet, Credentials, Access, Observe, Security and System.', drawer: true },
    { sel: '#sidebar a[href="/admin/approvals"]', title: 'Approvals', text: 'Requests waiting for your decision land here. The badge shows how many are pending.', drawer: true },
    { sel: '#nav-search', title: 'Search', text: 'Jump to any page, or find a client, credential or token by name. The shortcut works from anywhere.', drawer: true },
    { sel: '#sidebar .theme-switch', title: 'Theme', text: 'Choose Auto, Light or Dark. The choice is remembered in this browser.', drawer: true },
    { sel: '#sidebar a[href="/admin/docs"]', title: 'Docs', text: 'The full manual lives here, including What’s new, where you can take this tour again.', drawer: true }
  ];
  var narrowMQ = window.matchMedia('(max-width: 900px)');
  var st = null; // {steps, i, pop, opener, openedDrawer, hl, live, timer}

  function visible(el) {
    if (!el || !el.isConnected) return false;
    var r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  }
  function drawerOpen() { return document.body.classList.contains('nav-open'); }

  function setDrawer(open) {
    if (open === drawerOpen()) return false;
    (open ? DW.actions.toggleNav : DW.actions.closeNav)();
    return true;
  }

  function prepare(step) {
    // returns the target (or null) once it is laid out
    if (step.drawer && narrowMQ.matches && !drawerOpen()) {
      setDrawer(true);
      st.openedDrawer = true;
      return 260; // wait for the slide-in transition
    }
    return 0;
  }

  function findTarget(step) {
    var el = document.querySelector(step.sel);
    return el || null;
  }

  function position(target) {
    var pop = st.pop;
    var vw = document.documentElement.clientWidth, vh = window.innerHeight;
    var m = 12;
    var r = target.getBoundingClientRect();
    var w = pop.offsetWidth, h = pop.offsetHeight;
    var top, left;
    if (r.right + m + w + m <= vw) { // right of target
      left = r.right + m;
      top = r.top + (r.height - h) / 2;
    } else if (r.bottom + m + h + m <= vh) { // below
      top = r.bottom + m;
      left = r.left;
    } else if (r.top - m - h - m >= 0) { // above
      top = r.top - m - h;
      left = r.left;
    } else { // fallback: pinned to the bottom of the viewport
      top = vh - h - m;
      left = (vw - w) / 2;
    }
    left = Math.max(m, Math.min(left, vw - w - m));
    top = Math.max(m, Math.min(top, vh - h - m));
    pop.style.left = Math.round(left) + 'px';
    pop.style.top = Math.round(top) + 'px';
  }

  function clearHL() {
    if (st && st.hl) { st.hl.classList.remove('tour-hl'); st.hl = null; }
  }

  function render() {
    var step = st.steps[st.i];
    var n = st.steps.length;
    var last = st.i === n - 1;
    var wait = prepare(step);
    var go = function () {
      if (!st) return;
      var target = findTarget(step);
      if (!target || !visible(target)) { // skipped at run time: move on
        st.steps.splice(st.i, 1);
        if (!st.steps.length) { end(); return; }
        if (st.i >= st.steps.length) st.i = st.steps.length - 1;
        render();
        return;
      }
      n = st.steps.length;
      last = st.i === n - 1;
      clearHL();
      if (target.scrollIntoView) target.scrollIntoView({ block: 'nearest' });
      target.classList.add('tour-hl');
      st.hl = target;
      DW.setHTML(st.pop, DW.html`<h2 class="tour-title" id="tour-title">${step.title}</h2><p class="tour-text">${step.text}</p><div class="tour-foot"><span class="tour-count">Step ${st.i + 1} of ${n}</span><span class="tour-btns">${st.i > 0 ? DW.html`<button type="button" class="btn" data-action="tourBack">Back</button>` : ''}${last ? DW.html`<button type="button" class="btn btn-primary" data-action="tourDone">Done</button>` : DW.html`<button type="button" class="btn btn-primary" data-action="tourNext">Next</button>`}</span></div>`);
      st.live.textContent = 'Step ' + (st.i + 1) + ' of ' + n + '. ' + step.title + '. ' + step.text;
      st.pop.hidden = false;
      position(target);
      var btn = st.pop.querySelector('.btn-primary');
      if (btn) btn.focus();
    };
    if (st.timer) clearTimeout(st.timer);
    if (wait) st.timer = setTimeout(go, wait); else go();
  }

  function onKey(e) {
    if (!st) return;
    // Ctrl/Cmd-K ends the tour first; the palette's own handler then opens it.
    if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && (e.key === 'k' || e.key === 'K')) { end(); return; }
    if (e.key === 'Escape') {
      e.preventDefault(); e.stopImmediatePropagation();
      end();
    } else if (e.key === 'Tab') {
      var f = Array.prototype.filter.call(st.pop.querySelectorAll('button'), visible);
      if (!f.length) return;
      var first = f[0], lastB = f[f.length - 1];
      if (!st.pop.contains(document.activeElement)) { e.preventDefault(); first.focus(); }
      else if (e.shiftKey && document.activeElement === first) { e.preventDefault(); lastB.focus(); }
      else if (!e.shiftKey && document.activeElement === lastB) { e.preventDefault(); first.focus(); }
    }
  }
  function onClickCapture(e) {
    // Tapping the drawer scrim would close the drawer under the popover: end instead.
    if (st && e.target instanceof Element && e.target.classList.contains('nav-scrim')) end();
  }
  function onResize() { if (st && st.hl && st.pop && !st.pop.hidden) position(st.hl); }

  function start() {
    if (st) return;
    var opener = document.activeElement;
    var pop = document.createElement('div');
    pop.className = 'tour-pop';
    pop.setAttribute('role', 'dialog');
    pop.setAttribute('aria-labelledby', 'tour-title');
    pop.setAttribute('aria-modal', 'true'); // focus is trapped inside while the tour runs
    pop.hidden = true;
    var live = document.createElement('div');
    live.className = 'sr-only';
    live.setAttribute('role', 'status');
    live.setAttribute('aria-live', 'polite');
    document.body.appendChild(pop);
    document.body.appendChild(live);
    st = { steps: STEPS.slice(), i: 0, pop: pop, live: live, opener: opener, openedDrawer: false, hl: null, timer: null };
    document.addEventListener('keydown', onKey, true);
    document.addEventListener('click', onClickCapture, true);
    window.addEventListener('resize', onResize);
    render();
  }

  function end() {
    if (!st) return;
    var s = st;
    st = null;
    if (s.timer) clearTimeout(s.timer);
    if (s.hl) s.hl.classList.remove('tour-hl');
    document.removeEventListener('keydown', onKey, true);
    document.removeEventListener('click', onClickCapture, true);
    window.removeEventListener('resize', onResize);
    s.pop.remove();
    s.live.remove();
    var wasOpen = s.openedDrawer && drawerOpen();
    if (wasOpen) setDrawer(false);
    var o = s.opener;
    var back = o && o !== document.body && o.isConnected && !(wasOpen && o.closest && o.closest('#sidebar')) && o.offsetParent !== null ? o : null;
    if (!back && narrowMQ.matches && wasOpen) back = document.getElementById('menu-btn');
    if (!back) back = document.getElementById('main');
    if (back && back.focus) back.focus();
  }

  DW.actions.tourStart = function () { start(); };
  DW.actions.tourNext = function () { if (st && st.i < st.steps.length - 1) { st.i++; render(); } };
  DW.actions.tourBack = function () { if (st && st.i > 0) { st.i--; render(); } };
  DW.actions.tourDone = function () { end(); };
  DW.tour = { start: start, end: end };
})();
