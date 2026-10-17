package client

import (
	"bufio"
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
