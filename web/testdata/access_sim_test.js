// Pure-module checks for the "Test access" tab and the refusal-reason adapter.
// Usage: node access_sim_test.js <static-dir>
'use strict';
const path = require('path');
const assert = require('assert');
const dir = process.argv[2];
const H = require(path.join(dir, 'access-sim-hex.js'));
const A = require(path.join(dir, 'access-sim-api.js'));
const R = require(path.join(dir, 'refusal-log.js'));
let n = 0;
function check(name, fn) { try { fn(); n++; } catch (e) { console.error('FAIL ' + name + ': ' + e.message); process.exitCode = 1; } }
const u8 = (...a) => Uint8Array.from(a);
const eq = (a, b) => assert.deepStrictEqual(Array.from(a), Array.from(b));

// ---- hex parse / format ----
check('round trip all byte values', () => {
  const b = new Uint8Array(256).map((_, i) => i);
  const p = H.parseHex(H.formatHex(b));
  assert(p.bytes, JSON.stringify(p));
  eq(p.bytes, b);
});
check('round trip odd sizes', () => {
  [0, 1, 15, 16, 17, 33].forEach((len) => {
    const b = new Uint8Array(len).map((_, i) => (i * 7) & 255);
    eq(H.parseHex(H.formatHex(b)).bytes, b);
  });
});
check('whitespace and case accepted', () => eq(H.parseHex(' 0A\tff\n 7e ').bytes, u8(10, 255, 126)));
check('empty is empty', () => assert.strictEqual(H.parseHex('  \n').bytes.length, 0));
check('invalid digit reports offset', () => {
  const p = H.parseHex('00 1f 4g');
  assert.strictEqual(p.bytes, undefined);
  assert.strictEqual(p.offset, 7);
  assert.strictEqual(p.byte, 2);
});
check('odd length reports the dangling digit', () => {
  const p = H.parseHex('00 11 2');
  assert.strictEqual(p.offset, 6);
  assert.strictEqual(p.byte, 2);
  assert(/odd/i.test(p.error));
});
check('0x prefix is not hex', () => assert.strictEqual(H.parseHex('0x00').offset, 1));
check('digit pair split by whitespace is joined', () => eq(H.parseHex('1 2').bytes, u8(0x12)));

// ---- dump ----
check('dump rows', () => {
  const b = new Uint8Array(20).map((_, i) => 0x41 + i);
  const rows = H.dumpRows(b);
  assert.strictEqual(rows.length, 2);
  assert.strictEqual(rows[0].offset, '00000000');
  assert.strictEqual(rows[1].offset, '00000010');
  assert.strictEqual(rows[0].ascii, 'ABCDEFGHIJKLMNOP');
  assert.strictEqual(rows[1].ascii, 'QRST');
  assert.strictEqual(rows[0].hex.length, rows[1].hex.length, 'columns align');
  assert.strictEqual(rows[1].hex.trim(), '51 52 53 54');
  assert.strictEqual(H.dumpRows(u8(0, 10, 0x7f, 0x80))[0].ascii, '....');
  assert.strictEqual(H.dumpRows(b, 1).length, 1);
  const r8 = H.dumpRows(b, 0, 8);
  assert.strictEqual(r8.length, 3);
  assert.strictEqual(r8[2].ascii, 'QRST');
  assert.strictEqual(r8[1].offset, '00000008');
  assert.strictEqual(r8[0].hex.length, r8[2].hex.length);
});

// ---- UTF-8 detection ----
check('valid utf-8', () => {
  assert(H.isValidUTF8(H.encodeUTF8('héllo € 😀')));
  assert(H.isValidUTF8(u8()));
  assert(H.isValidUTF8(u8(0x7f)));
});
check('encode/decode round trip', () => {
  const s = 'aé€😀\n\t';
  assert.strictEqual(H.decodeUTF8(H.encodeUTF8(s)), s);
});
check('overlong forms rejected', () => {
  assert(!H.isValidUTF8(u8(0xc0, 0x80)));
  assert(!H.isValidUTF8(u8(0xc1, 0xbf)));
  assert(!H.isValidUTF8(u8(0xe0, 0x80, 0x80)));
  assert(!H.isValidUTF8(u8(0xe0, 0x9f, 0xbf)));
  assert(!H.isValidUTF8(u8(0xf0, 0x80, 0x80, 0x80)));
  assert(!H.isValidUTF8(u8(0xf0, 0x8f, 0xbf, 0xbf)));
  assert(H.isValidUTF8(u8(0xe0, 0xa0, 0x80)));
  assert(H.isValidUTF8(u8(0xf0, 0x90, 0x80, 0x80)));
});
check('surrogates rejected', () => {
  assert(!H.isValidUTF8(u8(0xed, 0xa0, 0x80)));
  assert(!H.isValidUTF8(u8(0xed, 0xbf, 0xbf)));
  assert(H.isValidUTF8(u8(0xed, 0x9f, 0xbf)));
});
check('beyond U+10FFFF and stray bytes rejected', () => {
  assert(!H.isValidUTF8(u8(0xf4, 0x90, 0x80, 0x80)));
  assert(!H.isValidUTF8(u8(0xf5, 0x80, 0x80, 0x80)));
  assert(!H.isValidUTF8(u8(0xff)));
  assert(!H.isValidUTF8(u8(0x80)));
  assert(!H.isValidUTF8(u8(0xc3)));
  assert(!H.isValidUTF8(u8(0xe2, 0x82)));
  assert(!H.isValidUTF8(u8(0xc3, 0x28)));
});

// ---- control bytes / hex-first rule ----
check('needsHex', () => {
  assert(!H.needsHex(H.encodeUTF8('{"a": 1}\r\n\tok é')));
  assert(!H.needsHex(u8()));
  assert(H.needsHex(u8(0x00)));
  assert(H.needsHex(u8(0x41, 0x01)));
  assert(H.needsHex(u8(0x1b)));
  assert(H.needsHex(u8(0x7f)));
  assert(H.needsHex(u8(0xff)));
  assert(H.needsHex(u8(0xc0, 0x80)));
});
check('text view escapes non-printable bytes', () => {
  assert.strictEqual(H.textView(u8(0x61, 0x00, 0xff, 0x0a, 0x09, 0x7f)), 'a\\x00\\xff\n\t\\x7f');
  assert.strictEqual(H.textView(H.encodeUTF8('hé')), 'hé');
  assert.strictEqual(H.textView(u8(0xc3)), '\\xc3');
  assert.strictEqual(H.textView(u8(0xed, 0xa0, 0x80)), '\\xed\\xa0\\x80');
});

// ---- presets ----
check('json preset is text', () => {
  const p = H.presetJSON('acme/api');
  assert.strictEqual(p.path, '/repos/acme/api/git/refs/heads/main');
  assert(!H.needsHex(p.bytes));
  assert.strictEqual(JSON.parse(H.decodeUTF8(p.bytes)).force, true);
});
check('push preset is real pkt-line and binary', () => {
  const p = H.presetPush('acme/api');
  assert.strictEqual(p.path, '/acme/api.git/git-receive-pack');
  assert(H.needsHex(p.bytes), 'contains NUL and pack bytes');
  const s = Buffer.from(p.bytes).toString('latin1');
  const l1 = parseInt(s.slice(0, 4), 16);
  const l2 = parseInt(s.slice(l1, l1 + 4), 16);
  assert.strictEqual(s.slice(l1 + l2, l1 + l2 + 4), '0000', 'flush after the commands');
  assert.strictEqual(s.slice(l1 + l2 + 4, l1 + l2 + 8), 'PACK');
  assert(s.slice(0, l1).indexOf('\0') > 0);
  eq(H.parseHex(H.formatHex(p.bytes)).bytes, p.bytes);
});

// ---- headers ----
check('headers', () => {
  assert.deepStrictEqual(H.parseHeaders('A: b\n\nContent-Type: x: y\r\n').headers, { A: 'b', 'Content-Type': 'x: y' });
  assert(H.parseHeaders('nocolon').error);
  assert(H.parseHeaders('bad name: v').error);
});

// ---- ASAPI requests ----
check('action request', () => {
  assert.deepStrictEqual(A.actionRequest({ repo: 'a/b', action: 'push', ref: 'main', workflow: 'x.yml' }), { mode: 'action', repo: 'a/b', action: 'push', ref: 'main' });
  assert.deepStrictEqual(A.actionRequest({ repo: 'a/b', action: 'dispatch', ref: 'main', workflow: 'x.yml' }), { mode: 'action', repo: 'a/b', action: 'dispatch', ref: 'main', workflow: 'x.yml' });
  assert.deepStrictEqual(A.actionRequest({ repo: 'a/b', action: 'clone', ref: 'main' }), { mode: 'action', repo: 'a/b', action: 'clone' });
});
check('raw request picks text or hex', () => {
  const t = A.rawRequest({ method: 'POST', path: '/x', headers: {}, bytes: H.encodeUTF8('hi') });
  assert.deepStrictEqual(t, { mode: 'raw', method: 'POST', path: '/x', body_text: 'hi' });
  const b = A.rawRequest({ method: 'POST', path: '/x', headers: { A: 'b' }, bytes: u8(0, 1, 255) });
  assert.deepStrictEqual(b, { mode: 'raw', method: 'POST', path: '/x', headers: { A: 'b' }, body_hex: '00 01 ff' });
  assert.strictEqual(A.rawRequest({ method: 'GET', path: '/', bytes: u8() }).body_text, undefined);
});
check('adaptResult', () => {
  const r = A.adaptResult({ verdict: 'refused', title: 'T', steps: [{ status: 'stop', label: 'L', detail: 'D' }, { status: '?', label: 'x' }], refs: [{ ref: 'r', verdict: 'ok' }, { ref: 's', verdict: 'refused' }], sees: 'S', depends_on_default_branch: true, per_key: [{ name: 'k1', verdict: 'allowed' }] });
  assert.strictEqual(r.verdictLabel, 'Refused');
  assert.strictEqual(r.steps[1].status, 'skip');
  assert.strictEqual(r.refs[0].ok, true);
  assert.strictEqual(r.refs[1].ok, false);
  assert.strictEqual(r.dependsOnDefault, true);
  assert.strictEqual(r.perKey[0].key, 'k1');
  assert.strictEqual(A.adaptResult({ verdict: 'held' }).verdictLabel, 'Held for approval');
  assert.strictEqual(A.adaptResult(null).verdict, 'refused');
});
check('adaptSummary and ref examples', () => {
  const s = A.adaptSummary({ repos: [{ repo: 'a/b', rules: [{ pattern: 'agent/**', role: 'agent', push: true, create_force_delete: true, pr_into: true, run_workflows: false }, { pattern: '@default', role: 'protected' }, { pattern: '**', role: 'read' }], tags: ['agent-*'], merge: false, approve: false, clone: true }] });
  assert.strictEqual(s[0].rules[0].role, 'Agent-owned');
  assert.strictEqual(s[0].rules[0].createForceDelete, true);
  assert.strictEqual(s[0].tags, 'agent-*');
  assert.strictEqual(s[0].merge, false);
  const ex = A.refExamples(s[0]);
  assert.strictEqual(ex[0], 'agent/sim');
  assert(ex.indexOf('main') > 0);
  assert.strictEqual(new Set(ex).size, ex.length);
  assert.deepStrictEqual(A.adaptSummary(null), []);
});

// ---- refusal log adapter ----
check('refusal adapter', () => {
  const row = { deny_detail: { kind: 'branch_rules', layer: 'placeholder', rule: 'release/** -> Protected', reason: 'sum', refs: [{ ref: 'refs/heads/a', verdict: 'allowed' }, { ref: 'refs/heads/b', verdict: 'refused', pattern: 'release/**', role: 'protected', reason: 'x' }], sees: '! rejected', more: 3 }, repeat_count: 4, placeholder_id: 'ph 1', method: 'POST', path: '/a/b.git/git-receive-pack' };
  const v = R.adapt(row);
  assert.strictEqual(v.kindLabel, 'Branch rules');
  assert.strictEqual(v.summary, 'sum');
  assert.strictEqual(v.refs[0].ok, true);
  assert.strictEqual(v.refs[1].ok, false);
  assert.strictEqual(v.more, 3);
  assert.strictEqual(v.repeats, 4);
  assert.strictEqual(R.simulatorHref(row), '/admin/phantom-tokens/ph%201?tab=test&method=POST&path=%2Fa%2Fb.git%2Fgit-receive-pack');
});
check('refusal adapter tolerates old rows and string detail', () => {
  assert.strictEqual(R.adapt({ deny_detail: '' }), null);
  assert.strictEqual(R.adapt({}), null);
  assert.strictEqual(R.adapt({ deny_detail: 'not json' }), null);
  assert.strictEqual(R.adapt({ deny_detail: JSON.stringify({ kind: 'capability', reason: 'r' }) }).kindLabel, 'Capability off');
  assert.strictEqual(R.repeatsOf({}), 1);
  assert.strictEqual(R.repeatsOf({ repeat_count: 7 }), 7);
  assert.strictEqual(R.simulatorHref({ method: 'GET', path: '/' }), '');
  assert(R.validKind('token_acl') && !R.validKind('bogus'));
  assert.strictEqual(R.kindLabel('weird_kind'), 'weird kind');
});
console.log('access-sim JS: ' + n + ' checks passed');
