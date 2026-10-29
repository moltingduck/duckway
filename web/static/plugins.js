(function () {
  'use strict';
  const root = document.getElementById('plugins-page');
  if (!root) return;
  const form = document.getElementById('plugin-import-form');
  const status = document.getElementById('plugins-status');
  const errorBox = document.getElementById('plugins-error');
  const catalog = document.getElementById('plugins-catalog');
  const rows = document.getElementById('plugins-rows');
  let busy = false;
  let loaded = false;

  function element(tag, text, className) {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = String(text);
    if (className) node.className = className;
    return node;
  }
  function setBusy(value) {
    busy = value;
    root.querySelectorAll('button, input').forEach(function (control) { control.disabled = value; });
    catalog.setAttribute('aria-busy', String(value));
  }
  function showError(message) {
    errorBox.textContent = message;
    errorBox.hidden = !message;
  }
  async function request(path, body) {
    const options = {credentials: 'same-origin', headers: {Accept: 'application/json'}};
    if (body !== undefined) {
      options.method = 'POST';
      options.headers['Content-Type'] = 'application/json';
      options.body = JSON.stringify(body);
    }
    const response = await fetch(path, options);
    let data;
    try { data = await response.json(); }
    catch (_) { throw new Error('The server returned an unexpected response. Your session may have expired; sign in again and refresh.'); }
    if (!response.ok || (data && data.error)) {
      throw new Error(data && typeof data.error === 'string' ? data.error : 'Request failed (HTTP ' + response.status + ').');
    }
    return data;
  }
  function actionButton(label, description, action, danger) {
    const button = element('button', label, 'btn btn-sm' + (danger ? ' btn-danger' : ''));
    button.type = 'button';
    button.setAttribute('aria-label', description);
    button.addEventListener('click', action);
    return button;
  }
  function timestamp(raw) {
    if (!raw) return 'Not reported';
    const date = new Date(raw);
    return Number.isNaN(date.getTime()) ? 'Unknown time' : date.toLocaleString();
  }
  function artifactDetails(artifacts, label) {
    const details = element('details');
    const list = Array.isArray(artifacts) ? artifacts : [];
    details.appendChild(element('summary', label + ': ' + list.length + ' platform' + (list.length === 1 ? '' : 's')));
    list.forEach(function (artifact) {
      const item = element('div', undefined, 'mb-1 text-sm');
      item.appendChild(element('strong', artifact.os + '/' + artifact.arch));
      item.appendChild(element('p', artifact.size.toLocaleString() + ' bytes'));
      const hash = element('code', artifact.sha256);
      hash.style.overflowWrap = 'anywhere';
      item.appendChild(hash);
      details.appendChild(item);
    });
    return details;
  }
  async function showClients(plugin, panel, button) {
    if (busy) return;
    if (!panel.hidden) {
      panel.hidden = true;
      button.setAttribute('aria-expanded', 'false');
      return;
    }
    panel.hidden = false;
    button.setAttribute('aria-expanded', 'true');
    panel.replaceChildren(element('p', 'Loading reported client versions…', 'text-sm'));
    setBusy(true);
    try {
      const clients = await request('/api/plugins/' + encodeURIComponent(plugin.name) + '/clients');
      if (!Array.isArray(clients)) throw new Error('The server returned invalid client version data.');
      panel.replaceChildren(element('p', 'Client version reports are snapshots, not running-process telemetry. Different version means the reported version differs from the published hash; it does not establish version order.', 'text-sm text-muted mb-1'));
      if (!clients.length) {
        panel.appendChild(element('p', 'No known clients.', 'text-sm'));
        return;
      }
      const table = element('table');
      const header = element('tr');
      ['Client', 'Installed version', 'Published version', 'Status', 'Last seen', 'Last update attempt'].forEach(function (title) {
        const heading = element('th', title);
        heading.scope = 'col';
        header.appendChild(heading);
      });
      const head = element('thead');
      head.appendChild(header);
      table.appendChild(head);
      const body = element('tbody');
      const labels = {unknown: 'Not reported', stale: 'Stale report', not_installed: 'Not installed', up_to_date: 'Up to date', different: 'Different version', update_failed: 'Update failed', unpublished: 'Not published'};
      clients.forEach(function (client) {
        const row = element('tr');
        row.appendChild(element('td', client.client_name || client.client_id));
        [client.installed_version, client.published_version].forEach(function (version) {
          const cell = element('td', version || '—', 'text-sm');
          cell.style.overflowWrap = 'anywhere';
          row.appendChild(cell);
        });
        row.appendChild(element('td', labels[client.status] || 'Unknown status'));
        row.appendChild(element('td', timestamp(client.seen_at), 'text-sm'));
        let result = timestamp(client.last_update_at);
        if (client.last_update) result += ' · ' + client.last_update.operation + ': ' + client.last_update.status + (client.last_update.error_code ? ' (' + client.last_update.error_code + ')' : '');
        row.appendChild(element('td', result, 'text-sm'));
        body.appendChild(row);
      });
      table.appendChild(body);
      panel.appendChild(table);
    } catch (err) {
      panel.replaceChildren(element('p', err.message, 'text-sm'));
    } finally { setBusy(false); }
  }
  function render(list) {
    rows.replaceChildren();
    list.forEach(function (plugin, index) {
      const row = element('tr');
      const identity = element('td');
      identity.appendChild(element('strong', plugin.name));
      identity.appendChild(element('p', plugin.description || plugin.available_description || 'No description supplied.', 'text-sm text-muted'));
      const source = element('p', 'Source: ' + plugin.manifest_url, 'text-sm text-muted');
      source.style.overflowWrap = 'anywhere';
      identity.appendChild(source);
      identity.appendChild(element('p', 'Install: duckway plugin install ' + plugin.name, 'mono text-sm'));
      identity.appendChild(element('p', 'Update: duckway plugin update ' + plugin.name, 'mono text-sm'));
      row.appendChild(identity);

      const version = element('td');
      [['Published', plugin.version], ['Available', plugin.available_version]].forEach(function (item) {
        version.appendChild(element('p', item[0], 'text-sm text-muted'));
        const value = element('code', item[1] || (item[0] === 'Published' ? 'Not published' : 'No different version reported'));
        value.style.overflowWrap = 'anywhere';
        version.appendChild(value);
      });
      version.appendChild(element('p', 'Last check: ' + timestamp(plugin.last_checked_at), 'text-sm text-muted'));
      version.appendChild(element('p', 'Last successful check: ' + timestamp(plugin.last_successful_check_at), 'text-sm text-muted'));
      version.appendChild(element('p', 'Next check: ' + timestamp(plugin.next_check_at), 'text-sm text-muted'));
      if (plugin.check_error) version.appendChild(element('p', 'Check failed: ' + plugin.check_error, 'text-sm'));
      row.appendChild(version);

      const platforms = element('td');
      if (plugin.version) platforms.appendChild(artifactDetails(plugin.artifacts, 'Published size and SHA-256'));
      if (plugin.available_version) {
        platforms.appendChild(element('p', plugin.available_description || 'No available description supplied.', 'text-sm text-muted'));
        platforms.appendChild(artifactDetails(plugin.available_artifacts, 'Available size and SHA-256'));
      }
      row.appendChild(platforms);
      const state = element('td');
      state.appendChild(element('span', !plugin.version ? 'Unpublished' : (plugin.enabled ? 'Enabled' : 'Disabled'), 'badge ' + (plugin.enabled ? 'badge-green' : 'badge-gray')));
      row.appendChild(state);
      const actions = element('td');
      const buttons = element('div');
      buttons.style.display = 'flex';
      buttons.style.flexWrap = 'wrap';
      buttons.style.gap = '0.5rem';
      buttons.appendChild(actionButton('Check now', 'Check ' + plugin.name + ' for an available version', function () {
        mutate('/api/plugins/' + encodeURIComponent(plugin.name) + '/check', {}, 'Checking ' + plugin.name + '…', 'Checked ' + plugin.name + '. Publish an available version explicitly to distribute it.');
      }));
      if (plugin.available_version) buttons.appendChild(actionButton('Publish available version', 'Publish available version of ' + plugin.name, function () {
        if (busy || !window.confirm('Publish ' + plugin.name + ' version ' + plugin.available_version + ' and enable distribution? Only continue if you trust this source. All artifacts will be downloaded and verified. Installed clients will update only when explicitly requested.')) return;
        mutate('/api/plugins/' + encodeURIComponent(plugin.name) + '/publish', {version: plugin.available_version, manifest_digest: plugin.available_manifest_digest}, 'Downloading and verifying ' + plugin.name + '…', 'Published and enabled ' + plugin.name + '. Clients can install or update explicitly.');
      }));
      if (plugin.version) {
        const action = plugin.enabled ? 'disable' : 'enable';
        buttons.appendChild(actionButton(plugin.enabled ? 'Disable' : 'Enable', (plugin.enabled ? 'Disable' : 'Enable') + ' distribution for ' + plugin.name, function () {
          const prompt = plugin.enabled
            ? 'Disable future distribution of ' + plugin.name + '? Already installed plugins will keep running and will not be removed.'
            : 'Enable distribution of the previously published ' + plugin.name + '? Clients will be able to install this native plugin.';
          if (busy || !window.confirm(prompt)) return;
          mutate('/api/plugins/' + encodeURIComponent(plugin.name) + '/' + action, {}, 'Changing distribution for ' + plugin.name + '…', 'Distribution ' + (plugin.enabled ? 'disabled' : 'enabled') + ' for ' + plugin.name + '.');
        }, plugin.enabled));
      }
      const clientRow = element('tr');
      const panel = element('td');
      panel.colSpan = 5;
      panel.hidden = true;
      panel.id = 'plugin-clients-' + index;
      panel.setAttribute('aria-label', 'Reported client versions for ' + plugin.name);
      clientRow.appendChild(panel);
      const clientButton = actionButton('Client versions', 'View reported client versions for ' + plugin.name, function () { showClients(plugin, panel, clientButton); });
      clientButton.setAttribute('aria-controls', panel.id);
      clientButton.setAttribute('aria-expanded', 'false');
      buttons.appendChild(clientButton);
      actions.appendChild(buttons);
      row.appendChild(actions);
      rows.appendChild(row);
      rows.appendChild(clientRow);
    });
    document.getElementById('plugins-empty').hidden = list.length !== 0;
    document.getElementById('plugins-table-wrap').hidden = list.length === 0;
    loaded = true;
  }
  async function load() {
    const data = await request('/api/plugins');
    if (!Array.isArray(data)) throw new Error('The server returned an invalid plugin catalog.');
    render(data);
  }
  async function refresh() {
    if (busy) return;
    setBusy(true);
    showError('');
    status.textContent = 'Loading plugins…';
    try {
      await load();
      status.textContent = 'Catalog refreshed.';
    } catch (err) {
      showError(err.message);
      status.textContent = loaded ? 'Could not refresh. The displayed catalog may be out of date.' : 'Could not load plugins. Use Refresh to retry.';
    } finally { setBusy(false); }
  }
  async function mutate(path, body, pendingMessage, successMessage) {
    if (busy) return;
    setBusy(true);
    showError('');
    status.textContent = pendingMessage;
    let applied = false;
    try {
      await request(path, body);
      applied = true;
      await load();
      status.textContent = successMessage;
    } catch (err) {
      showError(err.message);
      status.textContent = applied ? successMessage + ' Catalog refresh failed; use Refresh to see current state.' : 'The request did not complete successfully. Refresh to confirm the server state before retrying.';
    } finally { setBusy(false); }
  }
  form.addEventListener('submit', function (event) {
    event.preventDefault();
    if (busy || !form.reportValidity()) return;
    const name = document.getElementById('plugin-name').value.trim();
    const source = document.getElementById('plugin-manifest-url').value.trim();
    let url;
    try { url = new URL(source); } catch (_) { showError('Enter an absolute HTTP or HTTPS manifest URL.'); return; }
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash || source.includes('?') || source.includes('#')) {
      showError('Use an HTTP or HTTPS URL without credentials, query parameters, or a fragment.');
      return;
    }
    mutate('/api/plugins/import', {name: name, manifest_url: source}, 'Registering and checking ' + name + '…', 'Registered and checked ' + name + '. Review and publish an available version to enable distribution.');
  });
  document.getElementById('plugins-refresh').addEventListener('click', refresh);
  refresh();
}());
