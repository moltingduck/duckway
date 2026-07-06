package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestFormatHeadersRedactsCredentials(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-secret")
	headers.Set("X-Duckway-Token", "dw-client-token")
	headers.Set("Proxy-Authorization", "Basic secret")
	headers.Set("X-Api-Key", "secret-api-key")
	headers.Set("Cookie", "session=secret")
	headers.Set("Content-Type", "application/json")

	var got map[string][]string
	if err := json.Unmarshal([]byte(formatHeaders(headers)), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Authorization", "X-Duckway-Token", "Proxy-Authorization", "X-Api-Key", "Cookie"} {
		if values := got[key]; len(values) != 1 || values[0] != "[redacted]" {
			t.Fatalf("%s was not redacted: %#v", key, values)
		}
	}
	if got["Content-Type"][0] != "application/json" {
		t.Fatalf("non-sensitive header changed: %#v", got["Content-Type"])
	}
}
