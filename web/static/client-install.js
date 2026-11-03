/* client-install.js - shared helpers for the Clients list and client detail pages.
 * Requires dw.js. Exposes window.DWInstall:
 *   DWInstall.gateway()           -> Promise<{url, valid}>  (configured gateway URL; valid when /install.sh is served)
 *   DWInstall.shellQuote(s)       -> POSIX single-quoted string
 *   DWInstall.installCmd(url)     -> 'curl -fsSL URL/install.sh | sh'
 *   DWInstall.initCmd(url, name, token) -> printf ... | duckway init
 *   DWInstall.copy(text)          -> copies text to the clipboard (async API with textarea fallback)
 *   DWInstall.relTime(s)          -> '2 min ago' style label for a server timestamp ('' -> 'never')
 *   DWInstall.parseTime(s)        -> ms since epoch or NaN
 */
(function () {
  'use strict';

  var cached = null;

  function gateway() {
    if (cached) return cached;
    var p = fetch('/api/settings', {credentials: 'same-origin'})
      .then(function (r) { if (!r.ok) throw new Error('settings ' + r.status); return r.json(); })
      .then(function (s) {
        if (s && s.gateway_url) return {url: s.gateway_url, valid: true};
        return fetch('/install.sh', {method: 'HEAD'})
          .then(function (r) { return {url: window.location.origin, valid: r.ok}; })
          .catch(function () { return {url: window.location.origin, valid: false}; });
      })
      .catch(function () { return {url: window.location.origin, valid: false}; })
      .then(function (g) {
        // Do not memoise a failed lookup: a later call retries.
        if (!g.valid && cached === p) cached = null;
        return g;
      });
    cached = p;
    return p;
  }

  function shellQuote(value) {
    return "'" + String(value).replace(/'/g, "'\\''") + "'";
  }

  function installCmd(url) {
    return 'curl -fsSL ' + url.replace(/\/$/, '') + '/install.sh | sh';
  }

  function initCmd(url, name, token) {
    return "printf '%s\\n' " + [url, name, '1', token].map(shellQuote).join(' ') + ' | duckway init';
  }

  function copy(text) {
    function fallback() {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.className = 'sr-only';
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand('copy'); } catch (e) { /* ignore */ }
      document.body.removeChild(ta);
    }
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).catch(fallback);
    }
    fallback();
    return Promise.resolve();
  }

  function parseTime(s) {
    if (!s) return NaN;
    var t = String(s).trim();
    if (/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}/.test(t)) t = t.replace(' ', 'T');
    if (!/(Z|[+-]\d{2}:?\d{2})$/.test(t)) t += 'Z';
    return Date.parse(t);
  }

  function relTime(s) {
    var ms = parseTime(s);
    if (!isFinite(ms)) return 'never';
    var sec = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (sec < 60) return 'just now';
    var min = Math.round(sec / 60);
    if (min < 60) return min + ' min ago';
    var h = Math.round(min / 60);
    if (h < 48) return h + ' h ago';
    return Math.round(h / 24) + ' d ago';
  }

  window.DWInstall = {gateway: gateway, shellQuote: shellQuote, installCmd: installCmd, initCmd: initCmd, copy: copy, relTime: relTime, parseTime: parseTime};
})();
