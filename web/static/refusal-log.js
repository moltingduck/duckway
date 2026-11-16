/*
 * refusal-log.js - request-log refusal reasons (rlog1116). Pure helpers, no DOM.
 * Exposed as window.RLOG (and module.exports under node tests).
 *
 * ADAPTER: every field read from the logs API for refusal reasons is in adapt()
 * below. Row fields used: deny_detail, repeat_count. deny_detail fields used:
 * kind, layer, rule, reason, refs[].{ref, verdict, pattern, role, reason}, sees, more.
 * Filter query parameter: deny_kind.
 */
(function (root) {
  'use strict';

  var KINDS = [
    ['branch_rules', 'Branch rules'],
    ['capability', 'Capability off'],
    ['token_acl', 'Token ACL rule'],
    ['api_key_acl', 'Credential ACL'],
    ['service_acl', 'Service ACL'],
    ['invalid_policy', 'Invalid policy'],
    ['uncheckable', 'Could not check']
  ];
  var LABEL = {};
  KINDS.forEach(function (k) { LABEL[k[0]] = k[1]; });

  function kindLabel(kind) { return LABEL[kind] || (kind ? String(kind).replace(/_/g, ' ') : 'Refused'); }
  function validKind(kind) { return Object.prototype.hasOwnProperty.call(LABEL, kind); }

  function str(v) { return typeof v === 'string' ? v : (v === null || v === undefined ? '' : String(v)); }

  function refOK(v) { return /^(ok|allow|allowed|pass|accepted)/i.test(str(v)); }

  // adapt(row) -> null when the row carries no reason (older rows), else a view model.
  function adapt(row) {
    if (!row) return null;
    var d = row.deny_detail;
    if (typeof d === 'string') {
      if (!d) d = null; else { try { d = JSON.parse(d); } catch (e) { d = null; } }
    }
    var repeats = parseInt(row.repeat_count, 10);
    if (!(repeats > 1)) repeats = 1;
    if (!d || typeof d !== 'object') return null;
    var refs = Array.isArray(d.refs) ? d.refs : [];
    var more = parseInt(d.more, 10);
    return {
      kind: str(d.kind),
      kindLabel: kindLabel(d.kind),
      layer: str(d.layer),
      rule: str(d.rule),
      summary: str(d.reason),
      refs: refs.map(function (r) {
        r = r || {};
        return { ref: str(r.ref), ok: refOK(r.verdict), pattern: str(r.pattern), role: str(r.role), reason: str(r.reason) };
      }),
      more: more > 0 ? more : 0,
      sees: str(d.sees),
      repeats: repeats
    };
  }

  // repeatsOf: the x N count of a row even when it has no stored reason.
  function repeatsOf(row) {
    var n = parseInt(row && row.repeat_count, 10);
    return n > 1 ? n : 1;
  }

  // simulatorHref: deep link into the phantom token's Test access tab. The log
  // stores no body, so only method and path travel.
  function simulatorHref(row) {
    if (!row || !row.placeholder_id) return '';
    var p = new URLSearchParams();
    p.set('tab', 'test');
    if (row.method) p.set('method', row.method);
    if (row.path) p.set('path', row.path);
    return '/admin/phantom-tokens/' + encodeURIComponent(row.placeholder_id) + '?' + p.toString();
  }

  var api = { KINDS: KINDS, kindLabel: kindLabel, validKind: validKind, adapt: adapt, repeatsOf: repeatsOf, simulatorHref: simulatorHref };
  if (typeof module === 'object' && module && module.exports) module.exports = api;
  else root.RLOG = api;
})(typeof window !== 'undefined' ? window : this);
