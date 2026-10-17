package client

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewLocalProxyHTTPClientDoesNotApplyTotalTimeout(t *testing.T) {
	client := newLocalProxyHTTPClient()
	if client.Timeout != 0 {
		t.Fatalf("client timeout = %s, want no total request timeout", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout <= 0 {
		t.Fatal("response header timeout must bound waiting for the first response byte")
	}
	if transport.TLSHandshakeTimeout <= 0 {
		t.Fatal("TLS handshake timeout must be configured")
	}
	if transport.DialContext == nil {
		t.Fatal("dial timeout must be configured")
	}
}

func TestProxyHandlerFlushesSSEBeforeUpstreamCompletes(t *testing.T) {
	upstreamChunkSent := make(chan struct{})
	allowUpstreamFinish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("data: first\n\n")); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		close(upstreamChunkSent)
		<-allowUpstreamFinish
		_, _ = w.Write([]byte("data: second\n\n"))
	}))
	proxy := httptest.NewServer(&proxyHandler{
		serverURL: upstream.URL,
		client:    newLocalProxyHTTPClient(),
	})
	t.Cleanup(proxy.Close)
	t.Cleanup(func() {
		close(allowUpstreamFinish)
		upstream.Close()
	})

	resp, err := http.Get(proxy.URL + "/events")
	if err != nil {
		t.Fatalf("GET proxied SSE stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q, want event stream", got)
	}

	select {
	case <-upstreamChunkSent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not send the first event")
	}
	lineResult := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		lineResult <- struct {
			line string
			err  error
		}{line: line, err: err}
	}()
	var line string
	select {
	case result := <-lineResult:
		line = result.line
		if result.err != nil {
			t.Fatalf("read first event before upstream completed: %v", result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not flush first event while upstream remained open")
	}
	if got, want := line, "data: first\n"; got != want {
		t.Fatalf("first event line = %q, want %q", got, want)
	}
}

func TestProxyHandlerAppliesBodyTimeoutToNonSSE(t *testing.T) {
	const bodyTimeout = 50 * time.Millisecond
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	proxy := httptest.NewServer(&proxyHandler{
		serverURL:   upstream.URL,
		client:      newLocalProxyHTTPClient(),
		bodyTimeout: bodyTimeout,
	})
	t.Cleanup(proxy.Close)
	t.Cleanup(upstream.Close)

	resp, err := http.Get(proxy.URL + "/response")
	if err != nil {
		t.Fatalf("GET proxied response: %v", err)
	}
	defer resp.Body.Close()
	firstByte := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, firstByte); err != nil {
		t.Fatalf("read first response byte: %v", err)
	}
	if string(firstByte) != "{" {
		t.Fatalf("first response byte = %q, want '{'", firstByte)
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		readDone <- err
	}()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("non-SSE upstream request was not canceled by body timeout")
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy response body did not finish after timeout cancellation")
	}
}

func TestProxyHandlerExemptsEventStreamFromBodyTimeout(t *testing.T) {
	const bodyTimeout = 50 * time.Millisecond
	upstreamCanceled := make(chan struct{})
	firstChunkSent := make(chan struct{})
	allowUpstreamFinish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		close(firstChunkSent)
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-allowUpstreamFinish:
			_, _ = w.Write([]byte("data: second\n\n"))
		}
	}))
	proxy := httptest.NewServer(&proxyHandler{
		serverURL:   upstream.URL,
		client:      newLocalProxyHTTPClient(),
		bodyTimeout: bodyTimeout,
	})
	t.Cleanup(proxy.Close)
	t.Cleanup(func() {
		select {
		case <-allowUpstreamFinish:
		default:
			close(allowUpstreamFinish)
		}
		upstream.Close()
	})

	resp, err := http.Get(proxy.URL + "/events")
	if err != nil {
		t.Fatalf("GET proxied event stream: %v", err)
	}
	defer resp.Body.Close()
	select {
	case <-firstChunkSent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not send first event")
	}
	// Wait beyond the configured body timeout while the upstream stays open.
	select {
	case <-upstreamCanceled:
		t.Fatal("event-stream request was canceled by the body timeout")
	case <-time.After(3 * bodyTimeout):
	}
	close(allowUpstreamFinish)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read completed event stream: %v", err)
	}
	if want := "data: first\n\ndata: second\n\n"; string(body) != want {
		t.Fatalf("event stream body = %q, want %q", body, want)
	}
}
