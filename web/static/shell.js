/* shell.js - admin shell behavior (theme, drawer, logout, what's-new dialog). Requires dw.js. */
(function () {
  'use strict';
  var root = document.documentElement;

  function applyTheme(mode) {
    root.setAttribute('data-theme', mode);
  }
  function syncThemeButtons() {
    var mode = root.getAttribute('data-theme') || 'auto';
    document.querySelectorAll('[data-theme-choice]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(b.dataset.themeChoice === mode));
    });
  }
  DW.actions.setTheme = function (el) {
    var mode = el.dataset.themeChoice;
    if (['auto', 'light', 'dark'].indexOf(mode) < 0) return;
    try { localStorage.setItem('dw-theme', mode); } catch (e) {}
    applyTheme(mode);
    syncThemeButtons();
  };
  syncThemeButtons();

  function setDrawer(open) {
    document.body.classList.toggle('nav-open', open);
    var b = document.getElementById('menu-btn');
    if (b) b.setAttribute('aria-expanded', String(open));
  }
  DW.actions.toggleNav = function () { setDrawer(!document.body.classList.contains('nav-open')); };
  DW.actions.closeNav = function () { setDrawer(false); };
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && document.body.classList.contains('nav-open')) { setDrawer(false); var b = document.getElementById('menu-btn'); if (b) b.focus(); }
  });

  DW.actions.logout = async function (el) {
    el.disabled = true;
    try {
      var res = await fetch('/api/auth/logout', { method: 'POST', credentials: 'same-origin' });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      window.location.href = '/admin/login';
    } catch (err) {
      el.disabled = false;
      DW.toast('Logout failed', 'error');
    }
  };

  /* ---- What's-new dialog ---- */
  var wn = document.getElementById('wn-dialog');
  async function whatsNewFinish() {
    var box = document.getElementById('wn-dismiss');
    if (box && box.checked) {
      try {
        var res = await fetch('/api/whats-new/dismiss', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ version: wn.dataset.version })
        });
        if (!res.ok) throw new Error('HTTP ' + res.status);
      } catch (err) {
        DW.toast('Could not save your choice. Try again.', 'error');
        return false;
      }
    }
    DW.modal.close(wn);
    return true;
  }
  DW.actions.whatsNewOpen = function () { if (wn) DW.modal.open(wn); };
  DW.actions.whatsNewClose = function () { return whatsNewFinish(); };
  DW.actions.whatsNewNotes = async function (el) {
    if (await whatsNewFinish()) window.location.href = el.getAttribute('href');
  };
  if (wn && wn.dataset.autoopen === '1') {
    var openWn = function () { DW.modal.open(wn); };
    if (document.readyState === 'complete') openWn(); else window.addEventListener('load', openWn);
  }
})();
