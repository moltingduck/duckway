/*
 * phantom-acl.js - pure model for the phantom token access-rule builder.
 *
 * Maps the visual rule list onto the real permission_config schema that the
 * gateway's ACL engine evaluates (internal/server/services/permission.go):
 *
 *   {"version":"1","provider":"<service>","rules":[{"name":"..","endpoints":[
 *      {"method":"GET","path":"/v1/*","allow":true}], "deny_all_other":false}]}
 *
 * Engine semantics the builder mirrors: rules are evaluated in order; the first
 * rule with a matching endpoint decides (allow:true -> allowed, allow:false ->
 * denied, approve:true -> held until an admin approves); if no rule matches the
 * request is allowed. Within one rule a matching deny endpoint always wins. Path patterns support only "*" as a whole
 * segment or as a trailing segment (matches everything below it).
 *
 * A builder row ("simple" rule) is exactly one engine rule holding one
 * endpoint per selected method, all sharing one path and one allow flag. Any
 * rule using anything else (rate_limit, constraints, deny_all_other, several
 * paths, unknown keys, a missing allow flag, non-standard methods) is a
 * "custom" rule: it is kept verbatim and never rewritten by the builder.
 *
 * A "github" rule (t:'g') is the single rule the GitHub repositories card
 * generates from per-repository capability rows. It carries a
 * "github_capabilities" marker and is only recognised when its endpoints are
 * exactly what the capability table compiles from that marker (the table comes
 * from the server, services.GitHubCapabilityTable), so a hand-edited rule stays
 * a custom rule. It is always kept last, because it ends with deny_all_other.
 *
 * Exposes window.PTACL. No DOM access.
 */
(function () {
  'use strict';

  var METHODS = ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'];
  var RULE_KEYS = { name: 1, endpoints: 1, deny_all_other: 1 };
  var GH_RULE_KEYS = { name: 1, endpoints: 1, deny_all_other: 1, github_capabilities: 1, github_branch_policies: 1 };
  var EP_KEYS = { method: 1, path: 1, allow: 1, approve: 1 };
  var EFFECTS = ['allow', 'deny', 'approve'];
  var GH_REPO = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
  var GH = { table: [] };

  function isObj(v) { return v !== null && typeof v === 'object' && !Array.isArray(v); }
  function nonEmpty(v) { return typeof v === 'string' && v.trim() !== ''; }

  // The effect of one engine endpoint, or '' when the builder cannot show it
  // (no allow flag, a non-boolean flag, or both allow and approve).
  function endpointEffect(ep) {
    var a = ep.allow, p = ep.approve;
    if ('approve' in ep && typeof p !== 'boolean') return '';
    if ('allow' in ep && typeof a !== 'boolean') return '';
    if (p === true) return a === true ? '' : 'approve';
    if (typeof a !== 'boolean') return '';
    return a ? 'allow' : 'deny';
  }

  /* ---------- GitHub capability rule ---------- */
  function setGitHubTable(t) { GH.table = Array.isArray(t) ? t : []; }
  function ghPath(tpl, repo) { return tpl.split('{git}').join('/' + repo + '.git').split('{repo}').join(repo); }
  // [{key,label}] in table order.
  function ghCapabilities() { return GH.table.map(function (c) { return { key: c.key, label: c.label }; }); }
  // Ordered endpoints for caps {repo: {capKey: 'none'|'read'|'write'}}; mirrors services.CompileGitHubCapabilityRule.
  function ghCompile(caps) {
    var out = [];
    Object.keys(caps).sort().forEach(function (repo) {
      GH.table.forEach(function (c) {
        var level = (caps[repo] || {})[c.key];
        if (level !== 'read' && level !== 'write') return;
        var eps = c.read.slice();
        if (level === 'write') eps = eps.concat(c.write);
        eps.forEach(function (e) { out.push({ method: e.method, path: ghPath(e.path, repo), allow: true }); });
      });
    });
    return out;
  }
  function ghCapsValid(caps) {
    if (!isObj(caps) || !Object.keys(caps).length) return false;
    var known = {}, seen = {};
    GH.table.forEach(function (c) { known[c.key] = 1; });
    var repos = Object.keys(caps);
    for (var i = 0; i < repos.length; i++) {
      var low = repos[i].toLowerCase();
      if (!GH_REPO.test(repos[i]) || seen[low] || !isObj(caps[repos[i]])) return false;
      seen[low] = 1;
      var row = caps[repos[i]], keys = Object.keys(row);
      for (var k = 0; k < keys.length; k++) {
        if (!known[keys[k]] || ['none', 'read', 'write'].indexOf(row[keys[k]]) === -1) return false;
      }
    }
    return true;
  }
  function ghEndpointsMatch(eps, want) {
    if (!Array.isArray(eps) || eps.length !== want.length || !eps.length) return false;
    for (var i = 0; i < eps.length; i++) {
      var e = eps[i];
      if (!isObj(e) || Object.keys(e).some(function (k) { return !EP_KEYS[k] || k === 'approve'; })) return false;
      if (e.method !== want[i].method || e.path !== want[i].path || e.allow !== true) return false;
    }
    return true;
  }
  function classifyGitHub(rule) {
    if (!GH.table.length || !isObj(rule) || !isObj(rule.github_capabilities)) return null;
    var keys = Object.keys(rule);
    for (var i = 0; i < keys.length; i++) if (!GH_RULE_KEYS[keys[i]]) return null;
    if ('name' in rule && typeof rule.name !== 'string') return null;
    if (rule.deny_all_other !== true || !ghCapsValid(rule.github_capabilities)) return null;
    if (!ghEndpointsMatch(rule.endpoints, ghCompile(rule.github_capabilities))) return null;
    if ('github_branch_policies' in rule && !isObj(rule.github_branch_policies)) return null;
    var caps = {}, bp = {};
    Object.keys(rule.github_capabilities).forEach(function (r) { caps[r] = Object.assign({}, rule.github_capabilities[r]); });
    var pol = rule.github_branch_policies || {};
    Object.keys(pol).forEach(function (r) { if (isObj(pol[r])) bp[r.toLowerCase()] = JSON.parse(JSON.stringify(pol[r])); });
    return { t: 'g', name: 'name' in rule ? rule.name : 'GitHub repositories', caps: caps, bp: bp };
  }
  function newGitHubRule() { return { t: 'g', name: 'GitHub repositories', caps: {}, bp: {} }; }
  // Branch policies (spec section 5) keyed by lower-case owner/repo, only for repositories in caps.
  function ghPolicies(r) {
    var out = {}, bp = r.bp || {};
    Object.keys(r.caps).forEach(function (repo) {
      var k = repo.toLowerCase();
      if (isObj(bp[k])) out[k] = bp[k];
    });
    return out;
  }

  // Returns a simple-rule model for an engine rule, or null when it must stay custom.
  function classifyRule(rule) {
    if (!isObj(rule)) return null;
    var keys = Object.keys(rule);
    for (var i = 0; i < keys.length; i++) if (!RULE_KEYS[keys[i]]) return null;
    if ('name' in rule && typeof rule.name !== 'string') return null;
    if ('deny_all_other' in rule && rule.deny_all_other !== false) return null;
    if (!Array.isArray(rule.endpoints) || !rule.endpoints.length) return null;
    var path = null, allow = null, methods = [];
    for (var j = 0; j < rule.endpoints.length; j++) {
      var ep = rule.endpoints[j];
      if (!isObj(ep)) return null;
      var ek = Object.keys(ep);
      for (var k = 0; k < ek.length; k++) if (!EP_KEYS[ek[k]]) return null;
      var eff = endpointEffect(ep);
      if (!eff || !nonEmpty(ep.path)) return null;
      if (METHODS.indexOf(ep.method) === -1 && ep.method !== '*') return null;
      if (path === null) { path = ep.path; allow = eff; }
      else if (ep.path !== path || eff !== allow) return null;
      if (methods.indexOf(ep.method) !== -1) return null;
      methods.push(ep.method);
    }
    if (methods.indexOf('*') !== -1 && methods.length > 1) return null;
    return { t: 's', name: 'name' in rule ? rule.name : '', hasName: 'name' in rule, auto: false, methods: methods, path: path, effect: allow };
  }

  function autoName(r) {
    return r.effect + ' ' + (r.methods.indexOf('*') !== -1 ? 'any method' : r.methods.join('/')) + ' ' + r.path;
  }

  function ruleToEngine(r) {
    if (r.t === 'c') return r.raw;
    if (r.t === 'g') {
      var g = { name: r.name, github_capabilities: r.caps };
      var pol = ghPolicies(r);
      if (Object.keys(pol).length) g.github_branch_policies = pol;
      g.endpoints = ghCompile(r.caps);
      g.deny_all_other = true;
      return g;
    }
    var out = {};
    var name = r.auto ? autoName(r) : r.name;
    if (r.auto || r.hasName) out.name = name;
    out.endpoints = r.methods.map(function (m) {
      return r.effect === 'approve' ? { method: m, path: r.path, approve: true } : { method: m, path: r.path, allow: r.effect === 'allow' };
    });
    out.deny_all_other = false;
    return out;
  }

  // Mirrors ValidatePermissionConfig / ParsePermissionConfig. Returns an error message or ''.
  function validate(cfg) {
    if (!isObj(cfg)) return 'The configuration must be a JSON object.';
    if (typeof cfg.version !== 'string') return 'Missing "version". Use "1" for path rules or "2" for GitHub repository grants.';
    if (cfg.version === '2') {
      if (cfg.provider !== 'github') return 'Version "2" requires "provider": "github".';
      if (!isObj(cfg.repositories) || !Object.keys(cfg.repositories).length) return 'Version "2" requires a non-empty "repositories" object.';
      return '';
    }
    if (cfg.version !== '1') return 'Unsupported version "' + cfg.version + '". Use "1" or "2".';
    if (typeof cfg.provider !== 'string') return 'Missing "provider" (the service name, for example "github").';
    if (!Array.isArray(cfg.rules) || !cfg.rules.length) return 'A configuration needs at least one rule. Leave it empty to inherit the service and credential rules.';
    for (var i = 0; i < cfg.rules.length; i++) {
      var r = cfg.rules[i], n = i + 1;
      if (!isObj(r)) return 'Rule ' + n + ' must be an object.';
      if ('name' in r && typeof r.name !== 'string') return 'Rule ' + n + ': "name" must be a string.';
      if ('deny_all_other' in r && typeof r.deny_all_other !== 'boolean') return 'Rule ' + n + ': "deny_all_other" must be true or false.';
      if (!Array.isArray(r.endpoints) || !r.endpoints.length) return 'Rule ' + n + ' must contain at least one endpoint.';
      for (var j = 0; j < r.endpoints.length; j++) {
        var e = r.endpoints[j], m = j + 1;
        if (!isObj(e)) return 'Rule ' + n + ' endpoint ' + m + ' must be an object.';
        if (!nonEmpty(e.method)) return 'Rule ' + n + ' endpoint ' + m + ' is missing "method".';
        if (!nonEmpty(e.path)) return 'Rule ' + n + ' endpoint ' + m + ' is missing "path".';
        if ('allow' in e && typeof e.allow !== 'boolean') return 'Rule ' + n + ' endpoint ' + m + ': "allow" must be true or false.';
        if ('approve' in e && typeof e.approve !== 'boolean') return 'Rule ' + n + ' endpoint ' + m + ': "approve" must be true or false.';
        if (e.allow === true && e.approve === true) return 'Rule ' + n + ' endpoint ' + m + ' cannot both "allow" and "approve". Pick one effect.';
      }
    }
    return '';
  }

  // text -> {ok, err, empty, version, provider, rules, extra, v2}
  function parse(text) {
    if (text == null || String(text).trim() === '') return { ok: true, empty: true, version: '1', provider: '', rules: [], extra: {}, v2: null };
    var cfg;
    try { cfg = JSON.parse(text); } catch (e) { return { ok: false, err: 'Invalid JSON: ' + e.message }; }
    var err = validate(cfg);
    if (err) return { ok: false, err: err };
    if (cfg.version === '2') return { ok: true, version: '2', provider: 'github', rules: [], extra: {}, v2: cfg };
    var extra = {};
    Object.keys(cfg).forEach(function (k) { if (k !== 'version' && k !== 'provider' && k !== 'rules') extra[k] = cfg[k]; });
    var rules = cfg.rules.map(function (r) { return classifyGitHub(r) || classifyRule(r) || { t: 'c', raw: r }; });
    return { ok: true, version: '1', provider: cfg.provider, rules: rules, extra: extra, v2: null };
  }

  // model {version, provider, rules, extra, v2} -> text ('' when there is nothing to enforce)
  function build(m) {
    if (m.v2) return JSON.stringify(m.v2, null, 2);
    if (!m.rules.length) return '';
    var cfg = { version: '1', provider: m.provider };
    cfg.rules = settle(m.rules.slice()).map(ruleToEngine);
    Object.keys(m.extra || {}).forEach(function (k) { cfg[k] = m.extra[k]; });
    return JSON.stringify(cfg, null, 2);
  }

  // The GitHub rule ends with deny_all_other, so it must come last or it would
  // swallow every rule after it. Moves it (stable) and returns the same array.
  function settle(rules) {
    var g = rules.filter(function (r) { return r.t === 'g'; });
    if (!g.length) return rules;
    var rest = rules.filter(function (r) { return r.t !== 'g'; });
    rules.length = 0;
    rest.concat(g).forEach(function (r) { rules.push(r); });
    return rules;
  }

  function normalize(text) {
    if (text == null || String(text).trim() === '') return '';
    try { return JSON.stringify(JSON.parse(text)); } catch (e) { return String(text).trim(); }
  }

  // Problems that would make the draft useless or rejected, per rule index.
  function ruleProblem(r) {
    if (r.t !== 's') return '';
    if (!r.methods.length) return 'Pick at least one method.';
    if (!nonEmpty(r.path)) return 'Enter a path pattern.';
    return '';
  }

  // Non-blocking advice about path pattern syntax the engine does not honour.
  function pathAdvice(p) {
    p = (p || '').trim();
    if (!p) return '';
    if (p.indexOf('**') !== -1) return '"**" is not supported. A trailing "*" already matches everything below that point.';
    if (p.charAt(0) !== '/' && p !== '*') return 'Paths normally start with "/".';
    var segs = p.replace(/^\/+|\/+$/g, '').split('/');
    for (var i = 0; i < segs.length; i++) {
      if (segs[i].indexOf('*') !== -1 && segs[i] !== '*') return '"*" only works as a whole path segment, for example /repos/*/issues. A partial wildcard like "' + segs[i] + '" matches only that exact text.';
    }
    return '';
  }

  var PRESETS = {
    readonly: {
      label: 'Read-only',
      help: 'Allows GET, HEAD and OPTIONS anywhere and denies every other method.',
      rules: function () {
        return [
          { t: 's', name: '', hasName: false, auto: true, methods: ['GET', 'HEAD', 'OPTIONS'], path: '/*', effect: 'allow' },
          { t: 's', name: '', hasName: false, auto: true, methods: ['*'], path: '/*', effect: 'deny' }
        ];
      }
    },
    approvewrites: {
      label: 'Approve writes',
      help: 'Allows GET and HEAD. POST, PUT, PATCH, DELETE and any other method wait for an admin to approve.',
      rules: function () {
        return [
          { t: 's', name: '', hasName: false, auto: true, methods: ['GET', 'HEAD'], path: '/*', effect: 'allow' },
          { t: 's', name: '', hasName: false, auto: true, methods: ['POST', 'PUT', 'PATCH', 'DELETE'], path: '/*', effect: 'approve' },
          // Catch-all so an exotic method (PROPFIND, PURGE, ...) cannot slip through unreviewed.
          { t: 's', name: '', hasName: false, auto: true, methods: ['*'], path: '/*', effect: 'approve' }
        ];
      }
    },
    nodeletes: {
      label: 'No deletes',
      help: 'Denies DELETE requests; everything else stays allowed.',
      rules: function () {
        return [{ t: 's', name: '', hasName: false, auto: true, methods: ['DELETE'], path: '/*', effect: 'deny' }];
      }
    }
  };

  function newRule() {
    return { t: 's', name: '', hasName: false, auto: true, methods: ['GET'], path: '/*', effect: 'allow' };
  }

  // Short plain-language description of one custom engine rule.
  function describeCustom(raw) {
    if (!isObj(raw)) return 'Not a rule object';
    var parts = [];
    var eps = Array.isArray(raw.endpoints) ? raw.endpoints : [];
    parts.push(eps.length + (eps.length === 1 ? ' endpoint' : ' endpoints'));
    if (eps.some(function (e) { return isObj(e) && e.constraints; })) parts.push('body or header constraints');
    if (isObj(raw.rate_limit)) parts.push('rate limit');
    if (raw.deny_all_other === true) parts.push('denies everything else');
    if (eps.some(function (e) { return isObj(e) && e.approve === true; })) parts.push('requires approval for some requests');
    if (eps.some(function (e) { return isObj(e) && !('allow' in e) && e.approve !== true; })) parts.push('an endpoint has no "allow" flag (the engine treats it as deny)');
    return parts.join(', ');
  }

  // One line per layer: what its config does, for the layered explanation.
  function summarize(text) {
    if (text == null || String(text).trim() === '') return { kind: 'none', text: 'allow all' };
    var p = parse(text);
    if (!p.ok) return { kind: 'invalid', text: 'invalid rules' };
    if (p.v2) {
      var n = Object.keys(p.v2.repositories).length;
      return { kind: 'github', text: n + (n === 1 ? ' repository grant' : ' repository grants') };
    }
    var gh = p.rules.filter(function (r) { return r.t === 'g'; });
    if (gh.length && gh.length === p.rules.length) {
      var n2 = gh.reduce(function (a, r) { return a + Object.keys(r.caps).length; }, 0);
      return { kind: 'github', text: n2 + (n2 === 1 ? ' repository grant' : ' repository grants') };
    }
    var c = p.rules.filter(function (r) { return r.t === 'c'; }).length;
    var base = p.rules.length + (p.rules.length === 1 ? ' rule' : ' rules');
    return { kind: c ? 'custom' : 'rules', text: c ? base + ', ' + c + ' custom' : base };
  }

  window.PTACL = {
    METHODS: METHODS, PRESETS: PRESETS,
    parse: parse, build: build, validate: validate, normalize: normalize, classifyRule: classifyRule,
    ruleProblem: ruleProblem, pathAdvice: pathAdvice, newRule: newRule, describeCustom: describeCustom,
    summarize: summarize, autoName: autoName, endpointEffect: endpointEffect, EFFECTS: EFFECTS,
    setGitHubTable: setGitHubTable, ghPolicies: ghPolicies, ghCapabilities: ghCapabilities, ghCompile: ghCompile, newGitHubRule: newGitHubRule, settle: settle
  };
})();
