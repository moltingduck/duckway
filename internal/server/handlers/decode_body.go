package handlers

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
)

// decodeBody decompresses raw bytes using the given Content-Encoding. Returns
// the decoded bytes plus true on success, or the original bytes plus false
// when the encoding is unknown / empty / decompression fails outright.
//
// Designed to tolerate truncation: capture buffers cap response bodies at
// maxCapturedBytes, so a streaming response is almost always cut mid-frame.
// gzip / zlib / brotli all fail near the cut, but we keep whatever was
// successfully decoded before the failure (useful for showing the prefix of
// a long SSE stream).
func decodeBody(raw []byte, contentEncoding string) ([]byte, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	if enc == "" || enc == "identity" {
		return raw, false
	}

	// Content-Encoding can chain multiple ("gzip, deflate"). Apply right-to-left.
	encodings := strings.Split(enc, ",")
	current := raw
	any := false
	for i := len(encodings) - 1; i >= 0; i-- {
		e := strings.TrimSpace(encodings[i])
		decoded, ok := decodeOnce(current, e)
		if !ok {
			// If we already decoded at least one layer, return that progress;
			// otherwise return raw with false.
			if any {
				return current, true
			}
			return raw, false
		}
		current = decoded
		any = true
	}
	return current, any
}

func decodeOnce(raw []byte, encoding string) ([]byte, bool) {
	var r io.ReadCloser
	switch encoding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, false
		}
		r = zr
	case "deflate":
		// Some servers send raw flate, others zlib-wrapped. Try zlib first
		// (RFC 7230 says "deflate" is zlib), fall back to raw flate.
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			r = zr
		} else {
			r = flate.NewReader(bytes.NewReader(raw))
		}
	case "br":
		return readAllTolerant(brotli.NewReader(bytes.NewReader(raw)), raw)
	default:
		return raw, false
	}
	return readAllTolerant(r, raw)
}

// readAllTolerant reads everything it can from r. Returns the bytes it managed
// to read plus true if any progress was made — even if the stream ended with
// io.ErrUnexpectedEOF (typical for a truncated capture). On a hard error
// before any byte is decoded, returns the original raw bytes plus false.
func readAllTolerant(r io.Reader, fallback []byte) ([]byte, bool) {
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			// Hard error mid-stream: keep what we have if it's useful.
			if out.Len() > 0 {
				return out.Bytes(), true
			}
			return fallback, false
		}
	}
	if rc, ok := r.(io.Closer); ok {
		_ = rc.Close()
	}
	if out.Len() == 0 {
		return fallback, false
	}
	return out.Bytes(), true
}
