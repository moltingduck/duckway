package handlers

import "testing"

func TestRedactApprovalQuery(t *testing.T) {
	cases := map[string]string{
		"":                                              "",
		"mode=live&page=2&per_page=50":                  "mode=live&page=2&per_page=50",
		"api_key=sk-123&page=1":                         "api_key=[redacted]&page=1",
		"apiKey=abc":                                    "apiKey=[redacted]",
		"access_token=eyJhbGci&mode=x":                  "access_token=[redacted]&mode=x",
		"sig=deadbeef":                                  "sig=[redacted]",
		"signature=abc123":                              "signature=[redacted]",
		"code=4711&state=ok":                            "code=[redacted]&state=ok",
		"password=hunter2":                              "password=[redacted]",
		"X-Amz-Signature=abcdef&X-Amz-Date=1":           "X-Amz-Signature=[redacted]&X-Amz-Date=1",
		"X-Amz-Credential=AKIA%2F2026":                  "X-Amz-Credential=[redacted]",
		"token=t1&refresh_token=t2":                     "token=[redacted]&refresh_token=[redacted]",
		"client_secret=s&client_id=public":              "client_secret=[redacted]&client_id=public",
		"api%5Fkey=enc&q=a%20b":                         "api%5Fkey=[redacted]&q=a%20b",
		"flag&key=v":                                    "[redacted]&key=[redacted]",
		"token[]=a&tokens%5B0%5D=b&tokens[x][y]=c":      "token[]=[redacted]&tokens%5B0%5D=[redacted]&tokens[x][y]=[redacted]",
		"abc123longvalueabc123long":                     "[redacted]",
		"ref=abcdefghij0123456789abcdefghij0123&page=3": "ref=[redacted]&page=3",
		"ticket=t1&sessid=s1&sid=9":                     "ticket=[redacted]&sessid=[redacted]&sid=[redacted]",
		"id=abc123&q=hello&filter=active&n=1234567890123456789012": "id=abc123&q=hello&filter=active&n=1234567890123456789012",
		"slug=just-a-long-word-without-digits-here":                "slug=just-a-long-word-without-digits-here",
		"status_code=200&code_challenge_method=S256":               "status_code=200&code_challenge_method=S256",
		"user[password]=hunter2&filter[page]=2&sort[mode]=asc":     "user[password]=[redacted]&filter[page]=2&sort[mode]=asc",
		"x[api_key]=k&a[b][token]=t&a[b][c]=ok":                    "x[api_key]=[redacted]&a[b][token]=[redacted]&a[b][c]=ok",
		"user%5Bpassword%5D=p&filter%5Bpage%5D=2":                  "user%5Bpassword%5D=[redacted]&filter%5Bpage%5D=2",
		// Identifier shapes stay visible even though they are long and mixed...
		"id=123e4567-e89b-12d3-a456-426614174000":                                  "id=123e4567-e89b-12d3-a456-426614174000",
		"commit=0123456789abcdef0123456789abcdef01234567":                          "commit=0123456789abcdef0123456789abcdef01234567",
		"digest=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef":  "digest=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"since=2026-11-03&at=2026-11-03T12:30:45.123Z&until=2026-11-03%2012:00:00": "since=2026-11-03&at=2026-11-03T12:30:45.123Z&until=2026-11-03%2012:00:00",
		// ...unless the name is secret-looking.
		"token=123e4567-e89b-12d3-a456-426614174000":   "token=[redacted]",
		"sig=0123456789abcdef0123456789abcdef01234567": "sig=[redacted]",
		// 32+ mixed characters under a plain name are redacted, 31 are not.
		"q=abcdefghij0123456789abcdefghij01": "q=[redacted]",
		"q=abcdefghij0123456789abcdefghij0":  "q=abcdefghij0123456789abcdefghij0",
	}
	for in, want := range cases {
		if got := redactApprovalQuery(in); got != want {
			t.Errorf("redactApprovalQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
