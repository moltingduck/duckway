/**
 * ACL Preview: renders a human-readable summary of what an ACL config allows/denies.
 * Also handles inherited ACL display.
 */

function renderACLPreview(textareaId, previewId) {
  var textarea = document.getElementById(textareaId);
  var preview = document.getElementById(previewId);
  if (!textarea || !preview) return;

  var json = textarea.value.trim();
  if (!json) {
    // Show inherited info
    var inherited = textarea.dataset.inherited;
    if (inherited) {
      DW.setHTML(preview, DW.html`<span class="text-muted text-sm">Inherited from ${textarea.dataset.inheritedFrom}:</span>${renderACLSummary(inherited)}`);
    } else {
      DW.setHTML(preview, DW.html`<span class="badge badge-green">allow-all</span> <span class="text-muted text-sm">(no ACL set at any layer)</span>`);
    }
    return;
  }

  try {
    JSON.parse(json);
    DW.setHTML(preview, renderACLSummary(json));
  } catch(e) {
    DW.setHTML(preview, DW.html`<span class="badge badge-red">Invalid JSON</span> <span class="text-muted text-sm">${e.message}</span>`);
  }
}

// Returns DW SafeHTML.
function renderACLSummary(json) {
  var allowAll = DW.html`<span class="badge badge-green">allow-all</span>`;
  if (!json) return allowAll;

  var config;
  try { config = JSON.parse(json); } catch(e) { return DW.html`<span class="badge badge-red">parse error</span>`; }

  if (!config.rules || config.rules.length === 0) return allowAll;

  var ruleBlocks = config.rules.map(function(rule) {
    var name = rule.name || 'unnamed';
    var denyBadge = rule.deny_all_other ? DW.html` <span class="badge badge-yellow acl-deny-badge">deny unlisted</span>` : '';

    var rows = (rule.endpoints || []).map(function(ep) {
      var icon = ep.allow !== false ? '✅' : '🚫';
      var rowClass = ep.allow !== false ? '' : 'acl-ep-deny';
      var constraints = '';
      if (ep.constraints && ep.constraints.body) {
        var parts = [];
        for (var field in ep.constraints.body) {
          var c = ep.constraints.body[field];
          if (c.oneOf) parts.push(field + '∈[' + c.oneOf.join(',') + ']');
          if (c.max !== undefined) parts.push(field + '≤' + c.max);
          if (c.min !== undefined) parts.push(field + '≥' + c.min);
          if (c.forbidden) parts.push(field + '🚫');
        }
        if (parts.length) constraints = DW.html` <span class="text-muted">(${parts.join(', ')})</span>`;
      }
      return DW.html`<tr class="${rowClass}"><td>${icon}</td><td><code>${ep.method}</code></td><td><code>${ep.path}</code></td><td>${constraints}</td></tr>`;
    });

    var rateLimit = '';
    if (rule.rate_limit) {
      var rl = [];
      if (rule.rate_limit.requests_per_minute) rl.push(rule.rate_limit.requests_per_minute + '/min');
      if (rule.rate_limit.requests_per_hour) rl.push(rule.rate_limit.requests_per_hour + '/hr');
      if (rule.rate_limit.requests_per_day) rl.push(rule.rate_limit.requests_per_day + '/day');
      if (rl.length) rateLimit = DW.html`<span class="text-muted text-sm">Rate limit: ${rl.join(', ')}</span>`;
    }
    return DW.html`<div class="acl-rule"><strong class="text-sm">${name}</strong>${denyBadge}<table class="acl-rule-table"><tbody>${rows}</tbody></table>${rateLimit}</div>`;
  });

  return DW.html`<div class="acl-preview-rules">${ruleBlocks}</div>`;
}

// Load inherited ACL for a textarea
function loadInheritedACL(textareaId, previewId, serviceId, apiKeyId) {
  var textarea = document.getElementById(textareaId);
  if (!textarea) return;

  // Chain: try API key ACL first, then service default
  var promises = [];

  if (apiKeyId) {
    promises.push(
      fetch('/api/keys/' + apiKeyId + '/acl-templates', {credentials:'same-origin'})
        .then(function(r) { return r.json(); })
        .then(function(data) { return {from: 'api_key', acl: data.current || ''}; })
    );
  }

  if (serviceId) {
    promises.push(
      fetch('/api/services/' + serviceId + '/acl-templates', {credentials:'same-origin'})
        .then(function(r) { return r.json(); })
        .then(function(data) { return {from: 'service', acl: data.current || ''}; })
    );
  }

  Promise.all(promises).then(function(results) {
    // Find first non-empty inherited ACL
    for (var i = 0; i < results.length; i++) {
      if (results[i].acl) {
        textarea.dataset.inherited = results[i].acl;
        textarea.dataset.inheritedFrom = results[i].from;
        break;
      }
    }
    renderACLPreview(textareaId, previewId);
  });
}
