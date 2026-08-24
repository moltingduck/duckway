package services

import "net/http"

var JWTExpiresAtMillis = jwtExpiresAtMillis

func SetTokenRefresherHTTPClientForTest(r *TokenRefresher, client *http.Client) {
	r.client = client
}
