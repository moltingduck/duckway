/*
 * access-sim-hex.js - pure byte/hex/UTF-8 helpers for the "Test access" raw mode.
 * No DOM access. Exposed as window.ASHEX (and module.exports under node tests).
 * A body is held as a byte array; Text and Hex are two views of the same bytes.
 */
(function (root) {
  'use strict';

  var MAX_BODY = 16 * 1024 * 1024; // the proxy's inspect cap
  var HEX = '0123456789abcdef';

  function hex2(b) { return HEX.charAt(b >> 4) + HEX.charAt(b & 15); }
  function hexVal(c) {
    if (c >= 48 && c <= 57) return c - 48;
    if (c >= 97 && c <= 102) return c - 87;
    if (c >= 65 && c <= 70) return c - 55;
    return -1;
  }
  function isSpace(c) { return c === 32 || c === 9 || c === 10 || c === 13 || c === 11 || c === 12; }

  // parseHex: hex digits with optional whitespace between them.
  // Ok:    {bytes: Uint8Array}
  // Error: {error, offset (0-based character index in the input), byte (index of the byte being read)}
  function parseHex(str) {
    str = String(str === null || str === undefined ? '' : str);
    var out = [], hi = -1, hiAt = 0;
    for (var i = 0; i < str.length; i++) {
      var c = str.charCodeAt(i);
      if (isSpace(c)) continue;
      var v = hexVal(c);
      if (v < 0) {
        return { error: 'Not a hex digit: ' + JSON.stringify(str.charAt(i)), offset: i, byte: out.length };
      }
      if (hi < 0) { hi = v; hiAt = i; } else { out.push(hi * 16 + v); hi = -1; }
    }
    if (hi >= 0) return { error: 'Odd number of hex digits: the last byte has only one digit', offset: hiAt, byte: out.length };
    if (out.length > MAX_BODY) return { error: 'Body is larger than 16 MiB', offset: 0, byte: MAX_BODY };
    return { bytes: Uint8Array.from(out) };
  }

  // formatHex: lowercase pairs, 16 bytes per line, so parseHex(formatHex(b)) round-trips.
  function formatHex(bytes) {
    var lines = [];
    for (var o = 0; o < bytes.length; o += 16) {
      var parts = [];
      for (var i = o; i < Math.min(o + 16, bytes.length); i++) parts.push(hex2(bytes[i]));
      lines.push(parts.join(' '));
    }
    return lines.join('\n');
  }

  // dumpRows: offset / hex / ASCII rows of perRow bytes (default 16) (hex padded so columns align).
  function dumpRows(bytes, maxRows, perRow) {
    perRow = perRow > 0 ? perRow : 16;
    var rows = [], limit = maxRows > 0 ? maxRows : Infinity;
    for (var o = 0; o < bytes.length && rows.length < limit; o += perRow) {
      var hx = [], asc = '';
      for (var i = o; i < o + perRow; i++) {
        if (i < bytes.length) {
          var b = bytes[i];
          hx.push(hex2(b));
          asc += b >= 32 && b < 127 ? String.fromCharCode(b) : '.';
        } else hx.push('  ');
      }
      var off = o.toString(16);
      while (off.length < 8) off = '0' + off;
      rows.push({ offset: off, hex: hx.join(' '), ascii: asc });
    }
    return rows;
  }

  // decodeAt: strict UTF-8 sequence at i. Rejects overlong forms, surrogates,
  // code points above U+10FFFF and truncated sequences.
  // Returns {cp, len} or null.
  function decodeAt(b, i) {
    var c = b[i];
    if (c < 0x80) return { cp: c, len: 1 };
    var n, min, cp;
    if (c >= 0xc2 && c <= 0xdf) { n = 1; min = 0x80; cp = c & 0x1f; }
    else if (c >= 0xe0 && c <= 0xef) { n = 2; min = 0x800; cp = c & 0x0f; }
    else if (c >= 0xf0 && c <= 0xf4) { n = 3; min = 0x10000; cp = c & 0x07; }
    else return null; // continuation byte, C0/C1 (always overlong), F5..FF
    if (i + n >= b.length) return null;
    for (var k = 1; k <= n; k++) {
      var cc = b[i + k];
      if (cc === undefined || (cc & 0xc0) !== 0x80) return null;
      cp = (cp << 6) | (cc & 0x3f);
    }
    if (cp < min) return null;
    if (cp >= 0xd800 && cp <= 0xdfff) return null;
    if (cp > 0x10ffff) return null;
    return { cp: cp, len: n + 1 };
  }

  function isControl(cp) { return cp === 0x7f || (cp < 0x20 && cp !== 9 && cp !== 10 && cp !== 13); }

  function isValidUTF8(bytes) {
    for (var i = 0; i < bytes.length;) {
      var d = decodeAt(bytes, i);
      if (!d) return false;
      i += d.len;
    }
    return true;
  }

  // needsHex: not valid UTF-8, or has control bytes other than \t \n \r.
  function needsHex(bytes) {
    for (var i = 0; i < bytes.length;) {
      var d = decodeAt(bytes, i);
      if (!d || isControl(d.cp)) return true;
      i += d.len;
    }
    return false;
  }

  function fromCP(cp) {
    if (cp > 0xffff) { cp -= 0x10000; return String.fromCharCode(0xd800 + (cp >> 10), 0xdc00 + (cp & 0x3ff)); }
    return String.fromCharCode(cp);
  }

  // textView: the Text rendering. Valid, printable characters are shown as text;
  // every other byte (invalid UTF-8, control) is shown as \xNN.
  function textView(bytes) {
    var s = '';
    for (var i = 0; i < bytes.length;) {
      var d = decodeAt(bytes, i);
      if (d && !isControl(d.cp)) { s += fromCP(d.cp); i += d.len; }
      else { s += '\\x' + hex2(bytes[i]); i++; }
    }
    return s;
  }

  // encodeUTF8: string -> bytes. A lone surrogate becomes U+FFFD.
  function encodeUTF8(str) {
    var out = [];
    for (var i = 0; i < str.length; i++) {
      var c = str.charCodeAt(i);
      if (c >= 0xd800 && c <= 0xdbff && i + 1 < str.length) {
        var d = str.charCodeAt(i + 1);
        if (d >= 0xdc00 && d <= 0xdfff) { c = 0x10000 + ((c - 0xd800) << 10) + (d - 0xdc00); i++; }
      }
      if (c >= 0xd800 && c <= 0xdfff) c = 0xfffd;
      if (c < 0x80) out.push(c);
      else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
      else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
      else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
    }
    return Uint8Array.from(out);
  }

  // decodeUTF8: bytes known to be valid -> string.
  function decodeUTF8(bytes) {
    var s = '';
    for (var i = 0; i < bytes.length;) {
      var d = decodeAt(bytes, i);
      if (!d) { s += '�'; i++; continue; }
      s += fromCP(d.cp); i += d.len;
    }
    return s;
  }

  function ascii(str) {
    var out = [];
    for (var i = 0; i < str.length; i++) out.push(str.charCodeAt(i) & 255);
    return out;
  }
  function pkt(str) {
    var n = (str.length + 4).toString(16);
    while (n.length < 4) n = '0' + n;
    return n + str;
  }

  // Example bodies for the two presets. The push is a real receive-pack request:
  // pkt-line commands, a flush packet and a minimal (empty) packfile.
  function presetJSON(repo) {
    var body = '{"sha": "9f3c2be41d7a8e05c6b1f2a9d4e7c03b58a61f2e", "force": true}';
    return {
      method: 'PATCH',
      path: '/repos/' + (repo || 'acme/api') + '/git/refs/heads/main',
      headers: 'Content-Type: application/json',
      bytes: encodeUTF8(body)
    };
  }
  function presetPush(repo) {
    var z = '0000000000000000000000000000000000000000';
    var a = '1f4e9c0a7b3d2e5f6a8b9c0d1e2f3a4b5c6d7e8f';
    var b = '9f3c2be41d7a8e05c6b1f2a9d4e7c03b58a61f2e';
    var c = '3a7d5e9b1c2f4a6e8d0b7c9a1e3f5d7b9c2a4e6f';
    var s = pkt(z + ' ' + b + ' refs/heads/agent/fix-1203\0 report-status side-band-64k agent=git/2.45.0') +
      pkt(a + ' ' + c + ' refs/heads/release/1.4') + '0000';
    var bytes = ascii(s).concat(ascii('PACK'), [0, 0, 0, 2, 0, 0, 0, 0]);
    for (var i = 0; i < 20; i++) bytes.push((i * 37 + 11) & 255);
    return {
      method: 'POST',
      path: '/' + (repo || 'acme/api') + '.git/git-receive-pack',
      headers: 'Content-Type: application/x-git-receive-pack-request',
      bytes: Uint8Array.from(bytes)
    };
  }

  // parseHeaders: "Name: value" lines -> {name: value}. Returns {headers, error}.
  function parseHeaders(text) {
    var h = {}, lines = String(text || '').split(/\r?\n/);
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      if (!line.trim()) continue;
      var at = line.indexOf(':');
      if (at < 1) return { headers: h, error: 'Header line ' + (i + 1) + ' must look like Name: value' };
      var name = line.slice(0, at).trim();
      if (!/^[A-Za-z0-9!#$%&'*+.^_`|~-]+$/.test(name)) return { headers: h, error: 'Header line ' + (i + 1) + ' has an invalid name' };
      h[name] = line.slice(at + 1).trim();
    }
    return { headers: h, error: '' };
  }

  var api = {
    MAX_BODY: MAX_BODY, parseHex: parseHex, formatHex: formatHex, dumpRows: dumpRows,
    isValidUTF8: isValidUTF8, needsHex: needsHex, textView: textView,
    encodeUTF8: encodeUTF8, decodeUTF8: decodeUTF8,
    presetJSON: presetJSON, presetPush: presetPush, parseHeaders: parseHeaders
  };
  if (typeof module === 'object' && module && module.exports) module.exports = api;
  else root.ASHEX = api;
})(typeof window !== 'undefined' ? window : this);
