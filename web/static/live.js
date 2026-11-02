/* live.js - admin live updates over Server-Sent Events. Requires dw.js.
 *
 *   DW.live.on(topic, fn[, {defer:true}])  fn(detail) runs when `topic` is
 *                           invalidated; with defer it waits while a modal is
 *                           open or the tab is hidden.
 *   DW.live.off(topic, fn)
 *   DW.live.modalOpen()     true while a .modal-backdrop is shown.
 *   DW.live.state()         'connecting' | 'live' | 'reconnecting' | 'polling'
 *
 * Topics: approvals, logs, rollouts, badge. Events are invalidation hints
 * only; handlers refetch through the normal authenticated JSON APIs. After
 * 3 consecutive failures (a connection must stay up 10s to reset the count) (or without EventSource) the module switches to
 * polling mode: synthetic invalidations ({synthetic:true}) every 15s while SSE
 * is retried every 60s.
 */
(function () {
  'use strict';
  var TOPICS = ['approvals', 'logs', 'rollouts', 'badge'];
  var URL_ = '/api/events';
  var POLL_MS = 15000;
  var SSE_RETRY_POLLING_MS = 60000;
  var MAX_BACKOFF_MS = 30000;
  var FAIL_LIMIT = 3;
  var DEBOUNCE_MS = 250;
  var STABLE_MS = 10000;     // a connection must stay up this long to reset failures
  var BASELINE_MIN_MS = 5000; // min gap between (re)connect catch-up invalidations

  var listeners = {};
  TOPICS.forEach(function (t) { listeners[t] = []; });
  var timers = {};
  var es = null;
  var retryTimer = null;
  var pollTimer = null;
  var failures = 0;
  var state = 'connecting';
  var everConnected = false;
  var upSince = 0;
  var lastBaseline = 0;
  var baselineTimer = null;
  var started = false;

  function statusEl() { return document.getElementById('live-status'); }
  var ANNOUNCE = {
    reconnecting: 'Live updates disconnected. Reconnecting.',
    polling: 'Live updates unavailable. Refreshing periodically.'
  };
  // Announce only degraded states and the return to Live from one; never
  // "Live" on a normal page load.
  function announce(prev, next) {
    if (prev === next) return;
    var msg = ANNOUNCE[next];
    if (!msg && next === 'live' && (prev === 'reconnecting' || prev === 'polling')) msg = 'Live updates restored.';
    if (!msg) return;
    var a = document.getElementById('live-announce');
    if (a) a.textContent = msg;
  }
  function setState(next) {
    var prev = state;
    state = next;
    announce(prev, next);
    var el = statusEl();
    if (!el) return;
    var map = {
      connecting: ['◌', 'Connecting…'],
      live: ['●', 'Live'],
      reconnecting: ['↻', 'Reconnecting…'],
      polling: ['⏱', 'Polling']
    };
    var m = map[next] || map.connecting;
    el.setAttribute('data-state', next);
    var icon = el.querySelector('.live-icon');
    var text = el.querySelector('.live-text');
    if (icon) icon.textContent = m[0];
    if (text) text.textContent = m[1];
    el.title = next === 'polling' ? 'Live stream unavailable; refreshing every 15 seconds' : '';
  }

  var held = []; // [{entry, topic, detail}] deferred while blocked
  var heldTimer = null;
  function run(entry, topic, detail) {
    try { entry.fn(detail); } catch (e) { if (window.console) console.error('live handler failed', topic, e); }
  }
  function flushHeld() {
    if (!held.length) { if (heldTimer) { clearInterval(heldTimer); heldTimer = null; } return; }
    if (document.hidden || modalOpen()) return;
    var todo = held;
    held = [];
    if (heldTimer) { clearInterval(heldTimer); heldTimer = null; }
    todo.forEach(function (h) { run(h.entry, h.topic, h.detail); });
  }
  function hold(entry, topic, detail) {
    for (var i = 0; i < held.length; i++) {
      if (held[i].entry === entry) { held[i].detail = detail; return; }
    }
    held.push({ entry: entry, topic: topic, detail: detail });
    if (!heldTimer) heldTimer = setInterval(flushHeld, 500);
  }
  function dispatch(topic, detail) {
    (listeners[topic] || []).slice().forEach(function (entry) {
      if (entry.defer && (document.hidden || modalOpen())) hold(entry, topic, detail);
      else run(entry, topic, detail);
    });
  }
  // Coalesce bursts: the latest detail for a topic wins within DEBOUNCE_MS.
  function schedule(topic, detail) {
    if (!(listeners[topic] || []).length) return;
    if (timers[topic]) clearTimeout(timers[topic].id);
    timers[topic] = { id: setTimeout(function () { delete timers[topic]; dispatch(topic, detail); }, DEBOUNCE_MS) };
  }
  function invalidateAll(detail) {
    TOPICS.forEach(function (t) { schedule(t, detail); });
  }
  // Catch-up after (re)connect: anything may have changed while disconnected
  // (or before the stream began), so refresh once, at most every
  // BASELINE_MIN_MS. A request inside the window is deferred to its end.
  function baselineInvalidate() {
    var now = Date.now();
    var wait = lastBaseline + BASELINE_MIN_MS - now;
    if (wait <= 0) {
      lastBaseline = now;
      invalidateAll({ synthetic: true });
      return;
    }
    if (baselineTimer) return;
    baselineTimer = setTimeout(function () {
      baselineTimer = null;
      lastBaseline = Date.now();
      invalidateAll({ synthetic: true });
    }, wait);
  }

  function modalOpen() {
    var list = document.getElementsByClassName('modal-backdrop');
    for (var i = 0; i < list.length; i++) {
      var b = list[i];
      if (!b.hidden && b.style.display !== 'none' && getComputedStyle(b).display !== 'none') return true;
    }
    return false;
  }

  function closeES() {
    if (es) { try { es.close(); } catch (e) {} es = null; }
  }
  function clearRetry() { if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; } }

  function startPolling() {
    setState('polling');
    if (pollTimer) return;
    pollTimer = setInterval(function () {
      if (!document.hidden) invalidateAll({ synthetic: true });
    }, POLL_MS);
  }
  function stopPolling() {
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  }

  function backoff() {
    var base = Math.min(MAX_BACKOFF_MS, 1000 * Math.pow(2, Math.max(0, failures - 1)));
    return base / 2 + Math.random() * base / 2;
  }

  // silent: a planned reconnect (server "bye") keeps the status as is.
  function connect(silent) {
    clearRetry();
    closeES();
    if (typeof window.EventSource !== 'function') { startPolling(); return; }
    if (state !== 'polling' && silent !== true) setState(everConnected ? 'reconnecting' : 'connecting');
    var src;
    try { src = new EventSource(URL_); } catch (e) { onFail(); return; }
    es = src;
    src.addEventListener('hello', function () {
      // failures is NOT reset here: only a connection that stays up for
      // STABLE_MS counts as healthy (see onFail), so a proxy that drops the
      // stream right after hello still reaches the polling fallback.
      everConnected = true;
      upSince = Date.now();
      stopPolling();
      setState('live');
      baselineInvalidate();
    });
    // Planned close at the server's stream lifetime limit: reconnect right
    // away; this is not a failure and never triggers the polling fallback.
    src.addEventListener('bye', function () {
      if (es !== src) return;
      closeES();
      upSince = 0;
      clearRetry();
      retryTimer = setTimeout(function () { connect(true); }, 200);
    });
    TOPICS.forEach(function (topic) {
      src.addEventListener(topic, function (ev) {
        var d = {};
        try { d = JSON.parse(ev.data) || {}; } catch (e) {}
        schedule(topic, d);
      });
    });
    src.addEventListener('error', function () {
      // Take over from the browser's built-in reconnect so backoff, the
      // failure count and the polling fallback stay under our control.
      if (es !== src) return;
      closeES();
      onFail();
    });
  }

  function onFail() {
    if (upSince && Date.now() - upSince >= STABLE_MS) failures = 0;
    upSince = 0;
    failures += 1;
    var delay;
    if (failures >= FAIL_LIMIT) {
      startPolling();
      delay = SSE_RETRY_POLLING_MS;
    } else {
      setState('reconnecting');
      delay = backoff();
    }
    clearRetry();
    retryTimer = setTimeout(connect, delay);
  }

  function start() {
    if (started) return;
    started = true;
    connect();
  }

  // opts.defer: hold the callback (latest detail only) while a modal is open
  // or the tab is hidden, then run it once when that ends.
  function on(topic, fn, opts) {
    if (!listeners[topic]) listeners[topic] = [];
    var l = listeners[topic];
    for (var i = 0; i < l.length; i++) if (l[i].fn === fn) return;
    l.push({ fn: fn, defer: !!(opts && opts.defer) });
  }
  function off(topic, fn) {
    var l = listeners[topic] || [];
    for (var i = 0; i < l.length; i++) {
      if (l[i].fn === fn) {
        var entry = l.splice(i, 1)[0];
        held = held.filter(function (h) { return h.entry !== entry; });
        return;
      }
    }
  }

  /* ---- nav badge (every page) ---- */
  function setBadge(n) {
    var link = document.querySelector('.sidebar a[href="/admin/approvals"]');
    if (!link) return;
    var badge = link.querySelector('.nav-badge');
    if (!(n > 0)) { if (badge) badge.remove(); return; }
    if (!badge) {
      badge = document.createElement('span');
      badge.className = 'nav-badge';
      link.appendChild(badge);
    }
    badge.textContent = String(n);
    var sr = document.createElement('span');
    sr.className = 'sr-only';
    sr.textContent = ' pending';
    badge.appendChild(sr);
  }
  on('badge', function (d) {
    if (d && typeof d.pending === 'number') { setBadge(d.pending); return; }
    fetch('/api/approvals', { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (list) { if (Array.isArray(list)) setBadge(list.length); })
      .catch(function () {});
  });

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) return;
    if (state === 'polling') invalidateAll({ synthetic: true });
    flushHeld();
  });
  window.addEventListener('pagehide', function () {
    clearRetry(); stopPolling(); closeES();
    if (baselineTimer) { clearTimeout(baselineTimer); baselineTimer = null; }
    upSince = 0;
    setState('reconnecting');
  });
  // Restored from the back/forward cache: the stream was closed on pagehide
  // and the data is stale.
  window.addEventListener('pageshow', function (e) {
    if (!e.persisted || !started) return;
    failures = 0;
    connect();
    baselineInvalidate();
  });

  window.DW = window.DW || {};
  DW.live = {
    on: on,
    off: off,
    state: function () { return state; },
    modalOpen: modalOpen,
    start: start
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();
