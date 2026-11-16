package services

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/models"
)

type codexProbeTransport func(*http.Request) (*http.Response, error)

func (f codexProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func codexProbeJWT(account string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`, account))) + ".signature"
}

func TestCodexAccessProbeRequest(t *testing.T) {
	for _, tc := range []struct{ name, access, metadata, want string }{
		{"access claim", codexProbeJWT("source-access"), `{"account_id":"duckway-account"}`, "source-access"},
		{"ID token fallback", "opaque", fmt.Sprintf(`{"id_token":%q,"account_id":"duckway-account"}`, codexProbeJWT("source-id")), "source-id"},
		{"synthetic claim ignored", codexProbeJWT("duckway-account"), `{"account_id":"duckway-account"}`, ""},
		{"missing claims", "opaque", `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := buildAccessTokenTestRequest(&models.APIKey{TokenEndpoint: "https://auth.openai.com/oauth/token", SubscriptionInfo: tc.metadata}, tc.access)
			if err != nil {
				t.Fatal(err)
			}
			if req.URL.String() != "https://chatgpt.com/backend-api/wham/accounts/check" || req.Method != http.MethodGet || req.Body != nil {
				t.Fatalf("unexpected probe: %v", req)
			}
			if req.Header.Get("Authorization") != "Bearer "+tc.access || req.Header.Get("Accept") != "application/json" || req.Header.Get("ChatGPT-Account-Id") != tc.want {
				t.Fatalf("incorrect probe headers")
			}
		})
	}
	req, err := buildAccessTokenTestRequest(&models.APIKey{ServiceName: "openai"}, "api-key")
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "https://api.openai.com/v1/models" || req.Header.Get("Authorization") != "Bearer api-key" {
		t.Fatal("generic OpenAI API probe changed")
	}
}

func TestCodexAccessProbeClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		network bool
		want    accessTokenTestResult
	}{
		{"valid session", 200, false, accessTokenTestPassed},
		{"rejected credential", 401, false, accessTokenTestFailed},
		{"workspace or edge permission", 403, false, accessTokenTestInconclusive},
		{"rate limit", 429, false, accessTokenTestInconclusive},
		{"upstream failure", 500, false, accessTokenTestInconclusive},
		{"unavailable", 503, false, accessTokenTestInconclusive},
		{"network", 0, true, accessTokenTestInconclusive},
		{"login redirect", 302, false, accessTokenTestInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crypto := NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
			encrypted, err := crypto.Encrypt("probe-bearer-sentinel")
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			client := &http.Client{Transport: codexProbeTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				if requests != 1 || req.URL.String() != "https://chatgpt.com/backend-api/wham/accounts/check" {
					t.Fatal("probe followed redirect or used wrong endpoint")
				}
				if tc.network {
					return nil, fmt.Errorf("offline")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{"https://chatgpt.com/login"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_expired"}}`)), Request: req}, nil
			})}
			refresher := &TokenRefresher{crypto: crypto, client: client}
			got, _ := refresher.testStoredAccessToken(&models.APIKey{KeyEncrypted: encrypted, TokenEndpoint: "https://auth.openai.com/oauth/token"})
			if got != tc.want {
				t.Fatalf("result=%v want=%v", got, tc.want)
			}
			if client.CheckRedirect != nil {
				t.Fatal("shared client mutated")
			}
		})
	}
}

func TestCodexAccessProbeDoesNotBypassInvalidProxy(t *testing.T) {
	crypto := NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	access, err := crypto.Encrypt("probe-access")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := crypto.Encrypt("ftp://invalid-proxy.example")
	if err != nil {
		t.Fatal(err)
	}
	refresher := &TokenRefresher{crypto: crypto, client: &http.Client{Transport: codexProbeTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("probe bypassed configured proxy")
		return nil, fmt.Errorf("unexpected direct request")
	})}}
	result, err := refresher.testStoredAccessToken(&models.APIKey{KeyEncrypted: access, UpstreamProxyURL: proxy, TokenEndpoint: "https://auth.openai.com/oauth/token"})
	if result != accessTokenTestInconclusive || err == nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestGenericOpenAIAccessProbeForbiddenStillFails(t *testing.T) {
	crypto := NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	access, err := crypto.Encrypt("api-access")
	if err != nil {
		t.Fatal(err)
	}
	refresher := &TokenRefresher{crypto: crypto, client: &http.Client{Transport: codexProbeTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.openai.com/v1/models" {
			t.Fatal("wrong generic probe endpoint")
		}
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}}
	result, _ := refresher.testStoredAccessToken(&models.APIKey{KeyEncrypted: access, ServiceName: "openai"})
	if result != accessTokenTestFailed {
		t.Fatalf("result=%v", result)
	}
}
