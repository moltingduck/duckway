package handlers_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func BenchmarkProxyGitHubSmartHTTPReceivePackStreaming(b *testing.B) {
	const payloadSize = 8 * 1024 * 1024
	payload := bytes.Repeat([]byte("x"), payloadSize)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if n != int64(payloadSize) {
			http.Error(w, "unexpected body bytes: "+strconv.FormatInt(n, 10), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(b, upstream.URL)
	seedGitHubService(b, f)
	if err := f.placeholderQ.UpdatePlaceholder(f.placeholderID, "github_pat_dw_fake"); err != nil {
		b.Fatal(err)
	}
	acl := `{"version":"1","provider":"github","rules":[{"name":"git","endpoints":[{"method":"POST","path":"/OWNER/REPO.git/git-receive-pack","allow":true}],"deny_all_other":true}]}`
	ph, err := f.placeholderQ.GetByID(f.placeholderID)
	if err != nil {
		b.Fatal(err)
	}
	ph.PermissionConfig = &acl
	if err := f.placeholderQ.Update(ph); err != nil {
		b.Fatal(err)
	}

	h := newProxyHandler(f)
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:github_pat_dw_fake"))

	b.ReportAllocs()
	b.SetBytes(payloadSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := httptest.NewRequest("POST", "/proxy/github/OWNER/REPO.git/git-receive-pack", bytes.NewReader(payload))
		r.Header.Set("Authorization", auth)
		r = withClient(r, f.client)
		code, body := doProxy(h, r)
		if code != http.StatusOK {
			b.Fatalf("want 200, got %d; body: %s", code, body)
		}
	}
}
