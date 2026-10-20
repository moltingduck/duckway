package client

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchCodexCredentialsDistinguishesAbsenceFromFetchFailure(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		body            string
		wantErr         bool
		wantCredentials bool
	}{
		{"native credentials", 200, `{"tokens":{"id_token":"secret-must-not-appear"}}`, false, true},
		{"no assignment", 204, "", false, false},
		{"unauthorized", 401, "secret-must-not-appear", false, false},
		{"revoked", 403, "secret-must-not-appear", false, false},
		{"older server", 404, "secret-must-not-appear", false, false},
		{"request timeout", 408, "secret-must-not-appear", true, false},
		{"rate limited", 429, "secret-must-not-appear", true, false},
		{"server error", 500, "secret-must-not-appear", true, false},
		{"gateway error", 502, "secret-must-not-appear", true, false},
		{"empty response", 200, "", true, false},
		{"malformed response", 200, `{"secret-must-not-appear`, true, false},
		{"trailing response", 200, `{"tokens":{}} secret-must-not-appear`, true, false},
		{"missing tokens", 200, `{}`, false, false},
		{"null response", 200, `null`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/client/codex-credentials" || r.Header.Get("X-Duckway-Token") != "client-token" {
					t.Error("credential request omitted endpoint or client authentication")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			creds, err := NewAPIClient(srv.URL, "client-token").FetchCodexCredentials()
			if (err != nil) != tc.wantErr || (creds != nil) != tc.wantCredentials {
				t.Fatalf("credentials present=%v, error=%v; want present=%v, error=%v", creds != nil, err, tc.wantCredentials, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret-must-not-appear") {
				t.Fatal("credential response leaked into error")
			}
		})
	}
}

type codexFailureTransport struct{}

func (codexFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("secret-must-not-appear")
}

func TestFetchCodexCredentialsTransportFailureIsGeneric(t *testing.T) {
	api := NewAPIClient("http://duckway.invalid", "client-token")
	api.httpClient = &http.Client{Transport: codexFailureTransport{}}
	creds, err := api.FetchCodexCredentials()
	if creds != nil || err == nil {
		t.Fatal("transport failure was reported as authoritative absence")
	}
	if strings.Contains(err.Error(), "secret-must-not-appear") {
		t.Fatal("transport details leaked into credential error")
	}
}
