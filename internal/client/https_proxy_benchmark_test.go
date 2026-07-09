package client

import (
	"io"
	"net/http"
	"testing"
)

func BenchmarkWriteHTTPResponseGitPack(b *testing.B) {
	const payloadSize = 8 * 1024 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = 'p'
	}

	b.Run("stdlib_resp_write", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(payloadSize)
		for i := 0; i < b.N; i++ {
			resp := benchmarkGitPackResponse(newSliceReadCloser(payload))
			if err := resp.Write(countingWriter{}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("stream_copy_32k", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(payloadSize)
		for i := 0; i < b.N; i++ {
			resp := benchmarkGitPackResponse(newSliceReadCloser(payload))
			if err := writeHTTPResponseStream(countingWriter{}, resp); err != nil {
				b.Fatal(err)
			}
		}
	})
}

type countingWriter struct{}

func (countingWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

type sliceReadCloser struct {
	data []byte
	off  int
}

func newSliceReadCloser(data []byte) *sliceReadCloser {
	return &sliceReadCloser{data: data}
}

func (r *sliceReadCloser) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func (r *sliceReadCloser) Close() error { return nil }

func benchmarkGitPackResponse(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode:       http.StatusOK,
		Proto:            "HTTP/1.1",
		ProtoMajor:       1,
		ProtoMinor:       1,
		Header:           make(http.Header),
		Body:             body,
		ContentLength:    -1,
		TransferEncoding: []string{"chunked"},
	}
}
