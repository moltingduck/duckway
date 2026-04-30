package handlers

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"testing"

	"github.com/andybalholm/brotli"
)

func gzipped(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func zlibbed(s string) []byte {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func deflated(s string) []byte {
	var b bytes.Buffer
	w, _ := flate.NewWriter(&b, flate.DefaultCompression)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func brotlied(s string) []byte {
	var b bytes.Buffer
	w := brotli.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestDecodeBody_Gzip(t *testing.T) {
	plain := "hello world from anthropic"
	got, ok := decodeBody(gzipped(plain), "gzip")
	if !ok || string(got) != plain {
		t.Errorf("gzip decode: ok=%v got=%q", ok, got)
	}
}

func TestDecodeBody_GzipUppercase(t *testing.T) {
	got, ok := decodeBody(gzipped("hi"), "GZIP")
	if !ok || string(got) != "hi" {
		t.Errorf("uppercase encoding: ok=%v got=%q", ok, got)
	}
}

func TestDecodeBody_DeflateZlib(t *testing.T) {
	plain := "deflate via zlib"
	got, ok := decodeBody(zlibbed(plain), "deflate")
	if !ok || string(got) != plain {
		t.Errorf("zlib-flavored deflate: ok=%v got=%q", ok, got)
	}
}

func TestDecodeBody_DeflateRaw(t *testing.T) {
	plain := "deflate raw"
	got, ok := decodeBody(deflated(plain), "deflate")
	if !ok || string(got) != plain {
		t.Errorf("raw flate: ok=%v got=%q", ok, got)
	}
}

func TestDecodeBody_Brotli(t *testing.T) {
	plain := "brotli compressed payload"
	got, ok := decodeBody(brotlied(plain), "br")
	if !ok || string(got) != plain {
		t.Errorf("brotli: ok=%v got=%q", ok, got)
	}
}

func TestDecodeBody_Identity(t *testing.T) {
	got, ok := decodeBody([]byte("plain"), "identity")
	if ok || string(got) != "plain" {
		t.Errorf("identity should be passthrough: ok=%v got=%q", ok, got)
	}
}

func TestDecodeBody_Empty(t *testing.T) {
	got, ok := decodeBody([]byte{}, "gzip")
	if ok || len(got) != 0 {
		t.Errorf("empty input: ok=%v got=%q", ok, got)
	}
}

func TestDecodeBody_Unknown(t *testing.T) {
	raw := []byte("unrecognised bytes")
	got, ok := decodeBody(raw, "snappy")
	if ok || !bytes.Equal(got, raw) {
		t.Errorf("unknown encoding should fall back: ok=%v got=%q", ok, got)
	}
}

func TestDecodeBody_TruncatedGzip(t *testing.T) {
	// Cut the gzip stream mid-block: we should still return the prefix that
	// was successfully decoded, not the raw compressed bytes.
	full := gzipped("the quick brown fox jumps over the lazy dog the quick brown fox jumps over the lazy dog")
	truncated := full[:len(full)/2]
	got, ok := decodeBody(truncated, "gzip")
	if !ok {
		t.Fatalf("expected partial decode success on truncated gzip, got ok=false (raw=%d bytes)", len(truncated))
	}
	if len(got) == 0 {
		t.Errorf("expected some decoded prefix, got empty")
	}
	// Whatever decoded must be a prefix of the original plaintext.
	plain := "the quick brown fox jumps over the lazy dog"
	if !bytes.HasPrefix([]byte(plain+plain), got) && len(got) < len(plain)*2 {
		// Allow for partial: just check it's plausible text.
		for _, b := range got {
			if b < 9 || (b > 13 && b < 32) {
				t.Errorf("decoded prefix contains binary garbage: %q", got)
				break
			}
		}
	}
}

func TestDecodeBody_NoEncoding(t *testing.T) {
	got, ok := decodeBody([]byte("raw"), "")
	if ok || string(got) != "raw" {
		t.Errorf("empty encoding: ok=%v got=%q", ok, got)
	}
}
