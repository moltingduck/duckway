/*
 * phantom-tokens.js - Phantom tokens list page: filters, generate dialog, unassign dialog.
 * Depends on dw.js and phantom-common.js. No inline handlers; delegated data-action only.
 */
(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var pendingDelete = null;

  /* ---- filters ---- */
  function applyFilters() {
    var q = $('pt-search').value.trim().toLowerCase();
    var client = $('pt-f-client').value, service = $('pt-f-service').value;
    var source = $('pt-f-source').value, approval = $('pt-f-approval').value;
    var rows = document.querySelectorAll('#pt-list tr.pt-row');
    var shown = 0;
    rows.forEach(function (tr) {
      var d = tr.dataset;
      var ok = (!q || (d.text || '').toLowerCase().indexOf(q) !== -1) &&
        (!client || d.client === client) && (!service || d.service === service) &&
        (!source || d.source === source) && (!approval || d.approval === approval);
      tr.hidden = !ok;
      if (ok) shown++;
    });
    $('pt-count').textContent = rows.length ? 'Showing ' + shown + ' of ' + rows.length + ' phantom tokens' : '';
    var empty = $('pt-empty');
    empty.hidden = shown > 0;
    if (!rows.length) {
      $('pt-empty-title').textContent = 'No phantom tokens yet';
      $('pt-empty-text').textContent = 'Add a service and a credential, register a client, then generate a phantom token for it.';
    } else {
      $('pt-empty-title').textContent = 'No phantom tokens match';
      $('pt-empty-text').textContent = 'Clear a filter to see more.';
    }
  }
  DW.actions.ptFilter = applyFilters;

  /* ---- generate ---- */
  function selectedServiceName() {
    var sel = $('pt-add-svc');
    return sel.selectedOptions[0] ? (sel.selectedOptions[0].dataset.name || '') : '';
  }
  function filterBindings() {
    var svcId = $('pt-add-svc').value;
    $('pt-add-github').hidden = selectedServiceName() !== 'github';
    ['pt-add-key', 'pt-add-group'].forEach(function (id) {
      var sel = $(id);
      Array.prototype.forEach.call(sel.options, function (opt) {
        if (!opt.value) { opt.hidden = false; opt.disabled = false; return; }
        var off = !!(svcId && opt.dataset.service && opt.dataset.service !== svcId);
        opt.hidden = off; opt.disabled = off;
      });
      if (sel.selectedOptions[0] && sel.selectedOptions[0].disabled) sel.value = '';
    });
  }
  DW.actions.ptAddService = filterBindings;
  DW.actions.ptBindType = function () {
    var group = document.querySelector('#pt-add-form input[name="bind_type"]:checked').value === 'group';
    $('pt-add-key-row').hidden = group;
    $('pt-add-group-row').hidden = !group;
    if (group) $('pt-add-key').value = ''; else $('pt-add-group').value = '';
  };
  function addError(msg) {
    var el = $('pt-add-err');
    el.textContent = msg || '';
    el.hidden = !msg;
  }
  DW.actions.ptAddSubmit = function () {
    addError('');
    var form = $('pt-add-form');
    var body = {
      service_id: $('pt-add-svc').value,
      client_id: $('pt-add-client').value,
      env_name: $('pt-add-env').value.trim(),
      requires_approval: form.elements.requires_approval.checked
    };
    if (document.querySelector('#pt-add-form input[name="bind_type"]:checked').value === 'group') {
      body.group_id = $('pt-add-group').value;
      if (!body.group_id) { addError('Pick a key pool.'); return; }
    } else {
      body.api_key_id = $('pt-add-key').value;
      if (!body.api_key_id) { addError('Pick a credential.'); return; }
    }
    if (selectedServiceName() === 'github') {
      var repos = PT.parseGitRepoList($('pt-add-repos').value);
      if (!repos.length) { addError('Add at least one allowed repository as ORG/REPO.'); return; }
      body.permission_config = JSON.stringify(PT.gitACLForRepos(repos, $('pt-add-mode').value || 'deploy', $('pt-add-wf').value, $('pt-add-refs').value, $('pt-add-envs').value));
    }
    var btn = $('pt-add-submit');
    btn.disabled = true;
    return PT.api('POST', '/api/placeholders', body).then(function (created) {
      DW.toast('Phantom token generated', 'success');
      window.location = created && created.id ? '/admin/phantom-tokens/' + encodeURIComponent(created.id) : '/admin/phantom-tokens';
    }).catch(function (e) {
      addError(e.message);
      btn.disabled = false;
    });
  };

  /* ---- unassign ---- */
  DW.actions.ptAskUnassign = function (el) {
    pendingDelete = el.dataset.id;
    $('pt-del-env').textContent = el.dataset.env;
    $('pt-del-client').textContent = el.dataset.client;
    $('pt-del-bundle').hidden = el.dataset.source !== 'bundle';
    $('pt-del-err').hidden = true;
    $('pt-del-go').disabled = false;
    DW.modal.open('#pt-del-modal');
  };
  DW.actions.ptDoUnassign = function () {
    if (!pendingDelete) return;
    var id = pendingDelete, btn = $('pt-del-go');
    btn.disabled = true;
    return PT.api('DELETE', '/api/placeholders/' + encodeURIComponent(id)).then(function () {
      var row = $('pt-' + id);
      if (row) row.remove();
      pendingDelete = null;
      DW.modal.close('#pt-del-modal');
      DW.toast('Phantom token unassigned', 'success');
      applyFilters();
    }).catch(function (e) {
      var err = $('pt-del-err');
      err.textContent = e.message;
      err.hidden = false;
      btn.disabled = false;
    });
  };

  /* initial state, with optional ?client= / ?service= presets */
  try {
    var p = new URLSearchParams(window.location.search);
    if (p.get('client')) $('pt-f-client').value = p.get('client');
    if (p.get('service')) $('pt-f-service').value = p.get('service');
  } catch (e) { /* presets are optional */ }
  applyFilters();
})();
