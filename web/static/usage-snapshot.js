// renderUsageSnapshot turns the JSON blob stored in api_keys.usage_snapshot
// into a small HTML block — used by the OAuth detail modal and the Key Group
// member rows. Returns a DW SafeHTML value, or null when there is no snapshot to show
// (pass the non-null result to DW.setHTML / nest it inside DW.html).
//
// Snapshot shape (see internal/server/services/usage_snapshot.go):
//   { updated_at, provider, metrics: { name: { limit, remaining, reset } },
//     subscription?: { "5h_status": "...", "5h_remaining": "...", ... } }
function renderUsageSnapshot(rawJSON) {
  if (!rawJSON) return null;
  var snap;
  try { snap = JSON.parse(rawJSON); } catch (e) { return null; }
  if (!snap || (!snap.metrics && !snap.subscription)) return null;

  var metricsBlock = '';
  if (snap.metrics && Object.keys(snap.metrics).length) {
    var rows = Object.keys(snap.metrics).map(function(name) {
      var m = snap.metrics[name] || {};
      var used = (m.limit > 0 && m.remaining >= 0) ? (m.limit - m.remaining) : 0;
      var pct = (m.limit > 0) ? Math.min(100, Math.round((used / m.limit) * 100)) : 0;
      var label = name.replace(/_/g, ' ');
      var bar = DW.html`<div style="background:var(--line);border-radius:3px;overflow:hidden;height:6px;margin-top:3px"><div style="height:100%;width:${pct}%;background:${barColor(pct)}"></div></div>`;
      return DW.html`<tr><td>${label}${bar}</td><td>${fmtNum(used)}</td><td>${fmtNum(m.remaining)}</td><td>${fmtNum(m.limit)}</td><td class="text-muted text-sm">${fmtReset(m.reset)}</td></tr>`;
    });
    metricsBlock = DW.html`<table class="mt-1" style="width:100%;font-size:0.85rem"><thead><tr><th>Metric</th><th>Used</th><th>Remaining</th><th>Limit</th><th>Reset</th></tr></thead><tbody>${rows}</tbody></table>`;
  }

  var subBlock = '';
  if (snap.subscription && Object.keys(snap.subscription).length) {
    var badges = Object.keys(snap.subscription).map(function(k) {
      return DW.html`<span class="badge badge-gray" style="margin-right:0.25rem">${k}: ${snap.subscription[k]}</span>`;
    });
    subBlock = DW.html`<div class="mt-1 text-sm"><strong>Subscription window</strong><br>${badges}</div>`;
  }

  return DW.html`<div class="card mt-1" style="background:var(--bg-input);padding:0.75rem"><strong class="text-sm">Rate-limit usage</strong> <span class="text-muted text-sm">${snap.provider || ''} · updated ${snap.updated_at || ''}</span>${metricsBlock}${subBlock}</div>`;
}

// renderUsageCompact returns a one-line cell summary (DW SafeHTML) suitable for
// table cells (e.g. the Key Group member list). Picks the most informative
// metric in priority order: input_tokens > output_tokens > tokens > requests.
function renderUsageCompact(rawJSON) {
  var none = DW.html`<span class="text-muted">—</span>`;
  if (!rawJSON) return none;
  var snap;
  try { snap = JSON.parse(rawJSON); } catch (e) { return none; }
  if (!snap || !snap.metrics) return none;

  var pick = ['input_tokens', 'output_tokens', 'tokens', 'requests'];
  var chosen = null, name = '';
  for (var i = 0; i < pick.length; i++) {
    if (snap.metrics[pick[i]] && snap.metrics[pick[i]].limit > 0) {
      chosen = snap.metrics[pick[i]];
      name = pick[i].replace(/_/g, ' ');
      break;
    }
  }
  if (!chosen) return none;
  var used = chosen.limit - chosen.remaining;
  var pct = Math.min(100, Math.round((used / chosen.limit) * 100));
  return DW.html`<span class="text-sm" title="${name}: ${fmtNum(used)} / ${fmtNum(chosen.limit)}">${pct}% (${fmtNum(chosen.remaining)} left)</span>`;
}

function fmtNum(n) {
  if (typeof n !== 'number') return '—';
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
  return String(n);
}

function fmtReset(s) {
  if (!s) return '—';
  // RFC 3339? show countdown.
  var d = new Date(s);
  if (!isNaN(d.getTime())) {
    var diff = Math.round((d.getTime() - Date.now()) / 1000);
    if (diff > 60) return 'in ' + Math.round(diff / 60) + 'm';
    if (diff > 0) return 'in ' + diff + 's';
    return 'now';
  }
  // OpenAI duration string ("1.234s"). Show as-is.
  return s;
}

function barColor(pct) {
  if (pct < 70) return '#3b82f6';   // blue
  if (pct < 90) return '#f59e0b';   // amber
  return '#ef4444';                  // red
}
