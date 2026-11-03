package handlers

import "testing"

func TestRedactApprovalQuery(t *testing.T) {
	cases := map[string]string{
		"":                                         "",
		"mode=live&page=2&per_page=50":             "mode=live&page=2&per_page=50",
		"api_key=sk-123&page=1":                    "api_key=[redacted]&page=1",
		"apiKey=abc":                               "apiKey=[redacted]",
		"access_token=eyJhbGci&mode=x":             "access_token=[redacted]&mode=x",
		"sig=deadbeef":                             "sig=[redacted]",
		"signature=abc123":                         "signature=[redacted]",
		"code=4711&state=ok":                       "code=[redacted]&state=ok",
		"password=hunter2":                         "password=[redacted]",
		"X-Amz-Signature=abcdef&X-Amz-Date=1":      "X-Amz-Signature=[redacted]&X-Amz-Date=1",
		"X-Amz-Credential=AKIA%2F2026":             "X-Amz-Credential=[redacted]",
		"token=t1&refresh_token=t2":                "token=[redacted]&refresh_token=[redacted]",
		"client_secret=s&client_id=public":         "client_secret=[redacted]&client_id=public",
		"api%5Fkey=enc&q=a%20b":                    "api%5Fkey=[redacted]&q=a%20b",
		"flag&key=v":                               "[redacted]&key=[redacted]",
		"token[]=a&tokens%5B0%5D=b&tokens[x][y]=c": "token[]=[redacted]&tokens%5B0%5D=[redacted]&tokens[x][y]=[redacted]",
		"abc123longvalueabc123long":                "[redacted]",
		"ref=abc123longvalueabc123long&page=3":     "ref=[redacted]&page=3",
		"ticket=t1&sessid=s1&sid=9":                "ticket=[redacted]&sessid=[redacted]&sid=[redacted]",
		"id=abc123&q=hello&filter=active&n=1234567890123456789012": "id=abc123&q=hello&filter=active&n=1234567890123456789012",
		"slug=just-a-long-word-without-digits-here":                "slug=just-a-long-word-without-digits-here",
		"status_code=200&code_challenge_method=S256":               "status_code=200&code_challenge_method=S256",
	}
	for in, want := range cases {
		if got := redactApprovalQuery(in); got != want {
			t.Errorf("redactApprovalQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
