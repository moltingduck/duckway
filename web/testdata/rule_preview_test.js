// Pure-module checks for the rule change preview (rule-preview.js).
// Usage: node rule_preview_test.js <static-dir>
'use strict';
const path = require('path');
const assert = require('assert');
const R = require(path.join(process.argv[2], 'rule-preview.js'));
let n = 0;
function check(name, fn) { try { fn(); n++; } catch (e) { console.error('FAIL ' + name + ': ' + e.message); process.exitCode = 1; } }

const result = {
  window_days: 7, total_requests: 120, distinct: 4, truncated: false,
  buckets: { newly_refused: 2, newly_held: 0, newly_allowed: 1, no_longer_held: 0, unknown: 1, unchanged: 0 },
  bucket_hits: { newly_refused: 100, newly_allowed: 5, unknown: 15 },
  items: [
    { bucket: 'newly_refused', placeholder_id: 'ph 1', token_name: 'GH', client: 'laptop', method: 'POST', path: '/o/r.git/git-receive-pack', hits: 60,
      before: { verdict: 'allowed', kind: '', rule: '' }, after: { verdict: 'refused', kind: 'branch_rules', rule: 'main → protected' },
      refs: [{ ref: 'refs/heads/main', before: { verdict: 'allowed' }, after: { verdict: 'refused', rule: 'main → protected' } }],
      simulate_url: '/admin/phantom-tokens/ph%201?tab=test&method=POST&path=%2Fo%2Fr.git%2Fgit-receive-pack' },
    { bucket: 'newly_refused', placeholder_id: 'ph1', method: 'GET', path: '/v1/x', hits: 1, before: { verdict: 'held' }, after: { verdict: 'refused' }, simulate_url: 'javascript:alert(1)' },
    { bucket: 'unknown', method: 'POST', path: '/v1/c', hits: 15, before: { verdict: 'unknown' }, after: { verdict: 'unknown' }, reason: 'body not logged', reason_code: 'body_constraints' }
  ],
  tokens: [{ placeholder_id: 'ph1', token_name: 'GH', client: 'laptop', distinct: 3, buckets: { newly_refused: 2 }, bucket_hits: { newly_refused: 61 } }, { placeholder_id: 'ph2', buckets: {} }]
};

check('endpoints and request bodies', () => {
  assert.strictEqual(R.endpoint('token', 'a b'), '/api/placeholders/a%20b/preview-impact');
  assert.strictEqual(R.endpoint('key', 'k1'), '/api/keys/k1/preview-impact');
  assert.strictEqual(R.endpoint('service', 's1'), '/api/services/s1/preview-impact');
  assert.throws(() => R.endpoint('nope', 'x'));
  assert.deepStrictEqual(R.tokenBody({ config: '{"a":1}', approval: true, ttl: 30 }), { permission_config: '{"a":1}', requires_approval: true, approval_ttl_minutes: 30 });
  assert.deepStrictEqual(R.tokenBody({ config: '', approval: false, ttl: 0 }), { permission_config: '', requires_approval: false });
  assert.deepStrictEqual(R.keyBody(null), { acl: '' });
  assert.deepStrictEqual(R.serviceBody('{}'), { default_acl: '{}' });
});
check('tabs follow the fixed bucket order and carry counts', () => {
  const t = R.tabs(result);
  assert.deepStrictEqual(t.map((x) => x.id), ['newly_refused', 'newly_held', 'newly_allowed', 'no_longer_held', 'unknown', 'unchanged']);
  assert.strictEqual(t[0].count, 2); assert.strictEqual(t[0].hits, 100); assert.strictEqual(t[1].count, 0);
  assert.strictEqual(R.tabs(null).every((x) => x.count === 0), true);
});
check('default tab prefers a real change, then unknown', () => {
  assert.strictEqual(R.defaultTab(result), 'newly_refused');
  assert.strictEqual(R.defaultTab({ buckets: { newly_allowed: 1, unknown: 4 } }), 'newly_allowed');
  assert.strictEqual(R.defaultTab({ buckets: { unknown: 4, unchanged: 9 } }), 'unknown');
  assert.strictEqual(R.defaultTab({ buckets: { unchanged: 9 } }), 'unchanged');
  assert.strictEqual(R.defaultTab({}), 'unchanged');
});
check('items per tab and the count hidden by the server cap', () => {
  assert.strictEqual(R.itemsFor(result, 'newly_refused').length, 2);
  assert.strictEqual(R.itemsFor(result, 'newly_held').length, 0);
  assert.strictEqual(R.hiddenCount(result, 'newly_refused'), 0);
  assert.strictEqual(R.hiddenCount({ buckets: { unchanged: 5000 }, items: new Array(200).fill({ bucket: 'unchanged' }) }, 'unchanged'), 4800);
});
check('summary text', () => {
  const s = R.summary(result);
  assert(s.includes('Replayed 120 requests (4 distinct patterns)'), s);
  assert(s.includes('2 request patterns would be refused'), s);
  assert(s.includes('1 pattern would be allowed'), s);
  assert(s.includes('1 pattern could not be decided'), s);
  assert(!s.includes('partial'), s);
  assert(R.summary(Object.assign({}, result, { truncated: true })).includes('partial result'));
  assert(R.summary({ total_requests: 0, distinct: 0, window_days: 7 }).includes('nothing to compare'));
  assert(R.summary({ total_requests: 3, distinct: 1, window_days: 7, buckets: { unchanged: 1 } }).includes('Nothing changes'));
  assert.strictEqual(R.summary(null), '');
});
check('confirm only when something would be refused', () => {
  assert.strictEqual(R.needsConfirm(result), true);
  assert.strictEqual(R.needsConfirm({ buckets: { newly_refused: 0, newly_held: 3 } }), false);
  assert.strictEqual(R.needsConfirm(null), false);
  const c = R.confirmText(result, 'this token’s rules');
  assert(c.body.includes('2 request patterns (100 requests'), c.body);
  assert.strictEqual(c.danger, true);
});
check('row model', () => {
  const m = R.rowModel(result.items[0]);
  assert.strictEqual(m.hitsText, '60 requests');
  assert.strictEqual(m.before.label, 'Allowed'); assert.strictEqual(m.after.label, 'Refused'); assert.strictEqual(m.after.tone, 'bad');
  assert.strictEqual(m.changed, true);
  assert.strictEqual(m.href, '/admin/phantom-tokens/ph%201?tab=test&method=POST&path=%2Fo%2Fr.git%2Fgit-receive-pack');
  assert.strictEqual(m.refs.length, 1); assert.strictEqual(m.refs[0].after.rule, 'main → protected');
  const u = R.rowModel(result.items[2]);
  assert.strictEqual(u.reason, 'body not logged'); assert.strictEqual(u.before.label, 'Unknown'); assert.strictEqual(u.changed, false);
  assert.strictEqual(R.rowModel({ hits: 1, before: {}, after: {} }).hitsText, '1 request');
});
check('simulator links are same-origin token links only', () => {
  assert.strictEqual(R.rowModel(result.items[1]).href, '');
  assert.strictEqual(R.safeHref('https://evil.example/admin/phantom-tokens/x?a=1'), '');
  assert.strictEqual(R.safeHref('//evil.example/admin/phantom-tokens/x?a=1'), '');
  assert.strictEqual(R.safeHref('/admin/phantom-tokens/x?tab=test'), '/admin/phantom-tokens/x?tab=test');
  assert.strictEqual(R.safeHref(undefined), '');
});
check('per-token rows for key and service previews', () => {
  const rows = R.tokenRows(result);
  assert.strictEqual(rows.length, 2);
  assert.deepStrictEqual(rows[0], { id: 'ph1', name: 'GH', client: 'laptop', refused: 2, distinct: 3, refusedHits: 61 });
  assert.strictEqual(rows[1].refused, 0);
  assert.deepStrictEqual(R.tokenRows({}), []);
});
console.log(n + ' checks passed');
