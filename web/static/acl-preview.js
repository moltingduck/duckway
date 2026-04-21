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
      preview.innerHTML = '<span class="text-muted text-sm">Inherited from ' + textarea.dataset.inheritedFrom + ':</span>' +
        renderACLSummary(inherited);
    } else {
      preview.innerHTML = '<span class="badge badge-green">allow-all</span> <span class="text-muted text-sm">(no ACL set at any layer)</span>';
    }
    return;
  }

  try {
    var config = JSON.parse(json);
    preview.innerHTML = renderACLSummary(json);
  } catch(e) {
    preview.innerHTML = '<span class="badge badge-red">Invalid JSON</span> <span class="text-muted text-sm">' + e.message + '</span>';
  }
}

function renderACLSummary(json) {
  if (!json) return '<span class="badge badge-green">allow-all</span>';

  var config;
  try { config = JSON.parse(json); } catch(e) { return '<span class="badge badge-red">parse error</span>'; }

  if (!config.rules || config.rules.length === 0) return '<span class="badge badge-green">allow-all</span>';

  var html = '<div class="acl-preview-rules">';

  config.rules.forEach(function(rule) {
    var name = rule.name || 'unnamed';
    html += '<div class="acl-rule">';
    html += '<strong class="text-sm">' + name + '</strong>';
    if (rule.deny_all_other) html += ' <span class="badge badge-yellow" style="font-size:0.6rem">deny unlisted</span>';
    html += '<table style="margin:0.25rem 0;font-size:0.75rem"><tbody>';

    (rule.endpoints || []).forEach(function(ep) {
      var icon = ep.allow !== false ? '✅' : '🚫';
      var cls = ep.allow !== false ? '' : 'style="opacity:0.6"';
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
        if (parts.length) constraints = ' <span class="text-muted">(' + parts.join(', ') + ')</span>';
      }
      html += '<tr ' + cls + '><td>' + icon + '</td><td><code>' + ep.method + '</code></td><td><code>' + ep.path + '</code></td><td>' + constraints + '</td></tr>';
    });

    html += '</tbody></table>';

    if (rule.rate_limit) {
      var rl = [];
      if (rule.rate_limit.requests_per_minute) rl.push(rule.rate_limit.requests_per_minute + '/min');
      if (rule.rate_limit.requests_per_hour) rl.push(rule.rate_limit.requests_per_hour + '/hr');
      if (rule.rate_limit.requests_per_day) rl.push(rule.rate_limit.requests_per_day + '/day');
      if (rl.length) html += '<span class="text-muted text-sm">Rate limit: ' + rl.join(', ') + '</span>';
    }
    html += '</div>';
  });

  html += '</div>';
  return html;
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
