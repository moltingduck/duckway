/* shell.js - admin shell behavior (theme, drawer, logout, htmx errors). Requires dw.js. */
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

  document.body.addEventListener('htmx:afterRequest', function (e) {
    if (e.detail.xhr && e.detail.xhr.status >= 400) {
      try {
        var err = JSON.parse(e.detail.xhr.responseText);
        DW.toast(err.error || 'Request failed', 'error');
      } catch (ex) {
        DW.toast('Request failed', 'error');
      }
    }
  });
})();
