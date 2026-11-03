package handlers

import "testing"

func TestRedactApprovalQuery(t *testing.T) {
	cases := map[string]string{
		"":                                           "",
		"mode=live&page=2&per_page=50":               "mode=live&page=2&per_page=50",
		"api_key=sk-123&page=1":                      "api_key=[redacted]&page=1",
		"apiKey=abc":                                 "apiKey=[redacted]",
		"access_token=eyJhbGci&mode=x":               "access_token=[redacted]&mode=x",
		"sig=deadbeef":                               "sig=[redacted]",
		"signature=abc123":                           "signature=[redacted]",
		"code=4711&state=ok":                         "code=[redacted]&state=ok",
		"password=hunter2":                           "password=[redacted]",
		"X-Amz-Signature=abcdef&X-Amz-Date=1":        "X-Amz-Signature=[redacted]&X-Amz-Date=1",
		"X-Amz-Credential=AKIA%2F2026":               "X-Amz-Credential=[redacted]",
		"token=t1&refresh_token=t2":                  "token=[redacted]&refresh_token=[redacted]",
		"client_secret=s&client_id=public":           "client_secret=[redacted]&client_id=public",
		"api%5Fkey=enc&q=a%20b":                      "api%5Fkey=[redacted]&q=a%20b",
		"flag&key=v":                                 "flag&key=[redacted]",
		"status_code=200&code_challenge_method=S256": "status_code=200&code_challenge_method=S256",
	}
	for in, want := range cases {
		if got := redactApprovalQuery(in); got != want {
			t.Errorf("redactApprovalQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
