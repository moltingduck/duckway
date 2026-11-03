/*
 * phantom-common.js - helpers shared by the Phantom tokens list and detail pages.
 * Plain ES2020. Exposes window.PT. No DOM writes here, no inline handlers.
 */
(function () {
  'use strict';

  function api(method, url, body) {
    var opts = { method: method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
    return fetch(url, opts).then(function (r) {
      if (r.status === 401) { window.location = '/admin/login'; return new Promise(function () {}); }
      return r.text().then(function (t) {
        var data = null;
        if (t) { try { data = JSON.parse(t); } catch (e) { data = null; } }
        if (!r.ok) {
          var err = new Error((data && data.error) || ('Request failed (' + r.status + ')'));
          err.status = r.status;
          throw err;
        }
        return data;
      });
    });
  }

  /* ---- GitHub repository grants (schema version 2) ---- */
  function normalizeGitRepo(value) {
    var repo = (value || '').trim()
      .replace(/^https:\/\/github\.com\//, '')
      .replace(/^\/+|\/+$/g, '');
    repo = repo
      .replace(/^git@github\.com:/, '')
      .replace(/^\/+|\/+$/g, '')
      .replace(/\.git$/, '')
      .replace(/^\/+|\/+$/g, '');
    return repo || 'ORG/REPO';
  }

  function parseGitRepoList(value) {
    var seen = {};
    return (value || '').split(/[\n,]+/).map(normalizeGitRepo).filter(function (repo) {
      if (repo === 'ORG/REPO' || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo) || seen[repo.toLowerCase()]) return false;
      seen[repo.toLowerCase()] = true;
      return true;
    });
  }

  function parseGitPolicyList(value) {
    return (value || '').split(/[\n,]+/).map(function (v) { return v.trim(); }).filter(Boolean);
  }

  function gitACLForRepoGrants(grants) {
    var repositories = {};
    grants.forEach(function (grant) {
      var preset = grant.mode || (grant.push ? 'dev' : 'deploy');
      var c = { clone: true, push: false, issues_read: true, issues_write: false, pull_requests_read: true, pull_requests_write: false, releases_read: true, releases_write: false, actions_read: true, workflow_dispatch: false, workflow_rerun: false, workflow_cancel: false };
      if (preset === 'dev') { c.push = true; c.issues_write = true; c.pull_requests_write = true; }
      if (preset === 'cicd') { c.workflow_dispatch = true; c.workflow_rerun = true; c.workflow_cancel = true; }
      repositories[grant.repo] = { capabilities: c };
      if (preset === 'cicd') {
        repositories[grant.repo].workflow_allowlist = (grant.workflow_allowlist || []).length ? grant.workflow_allowlist : ['ci.yml'];
        repositories[grant.repo].ref_allowlist = (grant.ref_allowlist || []).length ? grant.ref_allowlist : ['main'];
      }
      if (grant.environment_allowlist && grant.environment_allowlist.length) repositories[grant.repo].environment_allowlist = grant.environment_allowlist;
    });
    return { version: '2', provider: 'github', repositories: repositories };
  }

  function gitACLForRepos(repos, mode, workflows, refs, environments) {
    return gitACLForRepoGrants(repos.map(function (repo) {
      return { repo: repo, pull: true, push: mode === 'dev', mode: mode, workflow_allowlist: parseGitPolicyList(workflows), ref_allowlist: parseGitPolicyList(refs), environment_allowlist: parseGitPolicyList(environments) };
    }));
  }

  function repoFromGitACLPath(path) {
    path = (path || '').trim();
    var suffixes = ['.git/info/refs', '.git/git-upload-pack', '.git/git-receive-pack'];
    for (var i = 0; i < suffixes.length; i++) {
      if (path.endsWith(suffixes[i])) return normalizeGitRepo(path.slice(0, -suffixes[i].length));
    }
    if (path.indexOf('/repos/') === 0) {
      var parts = path.replace(/^\/repos\//, '').split('/');
      if (parts.length >= 2) return normalizeGitRepo(parts[0] + '/' + parts[1]);
    }
    return '';
  }

  // Repository grants found in a config string (version 2 capabilities or legacy version 1 git paths).
  function gitGrantsFromACL(configJSON) {
    if (!configJSON) return [];
    var cfg;
    try { cfg = typeof configJSON === 'string' ? JSON.parse(configJSON) : configJSON; } catch (e) { return []; }
    if (!cfg || typeof cfg !== 'object') return [];
    if (cfg.version === '2' && cfg.provider === 'github') {
      return Object.keys(cfg.repositories || {}).sort().map(function (repo) {
        var p = cfg.repositories[repo] || {}, c = p.capabilities || {};
        return { repo: repo, capabilities: c, pull: !!c.clone, push: !!c.push, workflow_allowlist: p.workflow_allowlist || [], ref_allowlist: p.ref_allowlist || [], environment_allowlist: p.environment_allowlist || [] };
      });
    }
    var byRepo = {};
    (cfg.rules || []).forEach(function (rule) {
      (rule.endpoints || []).forEach(function (ep) {
        if (!ep || !ep.allow || !ep.path) return;
        var repo = repoFromGitACLPath(ep.path);
        if (!repo) return;
        if (!byRepo[repo]) byRepo[repo] = { repo: repo, pull: false, push: false };
        if (ep.path.indexOf('.git/git-upload-pack') !== -1) byRepo[repo].pull = true;
        if (ep.path.indexOf('.git/git-receive-pack') !== -1) byRepo[repo].push = true;
      });
    });
    return Object.keys(byRepo).sort(function (a, b) { return a.localeCompare(b); }).map(function (repo) { return byRepo[repo]; });
  }

  function grantMode(grant) {
    if (grant && grant.capabilities && grant.capabilities.workflow_dispatch) return 'cicd';
    return grant && grant.push ? 'dev' : 'deploy';
  }

  function grantCapabilities(grant) {
    if (grant.capabilities && Object.keys(grant.capabilities).length) {
      return Object.keys(grant.capabilities).filter(function (k) { return grant.capabilities[k]; }).map(function (k) { return k.replace(/_/g, ' '); });
    }
    var parts = [];
    if (grant.pull) parts.push('pull');
    if (grant.push) parts.push('push');
    return parts;
  }

  function grantLabel(grant) {
    var caps = grantCapabilities(grant);
    return caps.length ? caps.join(', ') : 'none';
  }

  function maskToken(tok) {
    tok = (tok || '').trim();
    if (tok.length <= 12) return '••••';
    return tok.slice(0, 6) + '…' + tok.slice(-4);
  }

  window.PT = {
    api: api,
    normalizeGitRepo: normalizeGitRepo,
    parseGitRepoList: parseGitRepoList,
    parseGitPolicyList: parseGitPolicyList,
    gitACLForRepoGrants: gitACLForRepoGrants,
    gitACLForRepos: gitACLForRepos,
    gitGrantsFromACL: gitGrantsFromACL,
    grantMode: grantMode,
    grantCapabilities: grantCapabilities,
    grantLabel: grantLabel,
    maskToken: maskToken
  };
})();
