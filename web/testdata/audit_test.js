// Pure-module checks for audit.js (line diff, labels, query building).
// Usage: node audit_test.js <static-dir>
'use strict';
const path = require('path');
const assert = require('assert');
const A = require(path.join(process.argv[2], 'audit.js'));
let n = 0;
function check(name, fn) { try { fn(); n++; } catch (e) { console.error('FAIL ' + name + ': ' + e.message); process.exitCode = 1; } }

check('lineDiff identical', () => {
  assert.deepStrictEqual(A.lineDiff('a\nb', 'a\nb').map((l) => l.t), ['same', 'same']);
});
check('lineDiff add and delete', () => {
  const d = A.lineDiff('a\nb\nc', 'a\nx\nc');
  assert.deepStrictEqual(d.map((l) => l.t + ':' + l.s), ['same:a', 'del:b', 'add:x', 'same:c']);
});
check('lineDiff empty sides (create and delete)', () => {
  assert.deepStrictEqual(A.lineDiff('', 'a\nb').map((l) => l.t), ['add', 'add']);
  assert.deepStrictEqual(A.lineDiff('a', '').map((l) => l.t), ['del']);
  assert.deepStrictEqual(A.lineDiff('', ''), []);
});
check('pretty sorts keys and accepts JSON text', () => {
  assert.strictEqual(A.pretty('{"b":1,"a":{"d":2,"c":3}}'), '{\n  "a": {\n    "c": 3,\n    "d": 2\n  },\n  "b": 1\n}');
  assert.strictEqual(A.pretty(null), '');
  assert.strictEqual(A.pretty('not json'), 'not json');
});
check('diff of before/after objects shows only the changed field', () => {
  const d = A.lineDiff(A.pretty({ a: 1, b: 2 }), A.pretty({ a: 1, b: 3 }));
  assert.deepStrictEqual(d.filter((l) => l.t !== 'same').map((l) => l.t + l.s.trim()), ['del"b": 2', 'add"b": 3']);
});
check('labels', () => {
  assert.strictEqual(A.actionLabel('placeholder.acl_update'), 'Acl update');
  assert.strictEqual(A.actorLabel('admin:alice'), 'alice (admin)');
  assert.strictEqual(A.actorLabel('anonymous'), 'Anonymous');
  assert.strictEqual(A.actorLabel(''), 'Anonymous');
  assert.strictEqual(A.typeLabel('api_key'), 'Credential');
  assert.strictEqual(A.typeLabel('weird_thing'), 'weird thing');
});
check('buildQuery skips empty fields and defaults the limit', () => {
  assert.strictEqual(A.buildQuery({ kind: 'rules', q: '', actor: 'admin:a' }), 'kind=rules&actor=admin%3Aa&limit=50');
  assert.strictEqual(A.buildQuery({ resource_type: 'service', resource_id: 's1', cursor: '9', limit: 20 }), 'resource_type=service&resource_id=s1&cursor=9&limit=20');
});
check('sinceFor', () => {
  assert.strictEqual(A.sinceFor('all'), '');
  assert.strictEqual(A.sinceFor('24h', Date.UTC(2026, 10, 16, 12)), '2026-11-15T12:00:00.000Z');
});
check('changeLines shows only changed keys and folds the rest', () => {
  const d = A.changeLines({ id: 'x', name: 'n', enabled: true, rules: { a: 1, b: 2 } }, { id: 'x', name: 'n', enabled: false, rules: { a: 1, b: 3 } });
  assert.deepStrictEqual(d.map((l) => l.t + '|' + l.s), [
    'del|"enabled": true', 'add|"enabled": false',
    'same|"rules": {', 'del|  "b": 2', 'add|  "b": 3', 'fold|  \u2026 1 unchanged: a', 'same|}',
    'fold|\u2026 2 unchanged: id, name']);
});
check('changeLines for create, delete and JSON text', () => {
  assert.deepStrictEqual(A.changeLines(null, '{"a":1}').map((l) => l.t + '|' + l.s), ['add|"a": 1']);
  assert.deepStrictEqual(A.changeLines({ a: [1, 2] }, '').map((l) => l.t), ['del', 'del', 'del', 'del']);
  assert.deepStrictEqual(A.changeLines({ a: 1 }, { a: 1 }).map((l) => l.t), ['fold']);
  assert.deepStrictEqual(A.changeLines('x', 'y').map((l) => l.t), ['del', 'add']); // non-JSON text falls back to a line diff
});
check('changeLines folds a long list of unchanged keys', () => {
  const o = {}; 'abcdefghij'.split('').forEach((k) => { o[k] = 1; });
  const d = A.changeLines(Object.assign({}, o, { z: 1 }), Object.assign({}, o, { z: 2 }));
  assert(d[d.length - 1].s.includes('10 unchanged: a, b, c, d, e, f and 4 more'), d[d.length - 1].s);
});
console.log('audit.js: ' + n + ' checks passed');
