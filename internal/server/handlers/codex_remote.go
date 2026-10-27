package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

const codexRemoteBase = "/backend-api/wham/remote/control/server"
const codexRemoteCapabilityPrefix = "dw_rc_"

func (h *ProxyHandler) WithCodexRemote(q *queries.CodexRemoteQueries) *ProxyHandler {
	h.remoteControl = q
	return h
}

// Native Codex v0.164 uses OAuth exclusively for enroll/refresh. Host pairing
// and relay use the distinct token returned by those calls. Never replace that
// dedicated token with an OAuth token, or allow an unbound bearer through.
func (h *ProxyHandler) handleCodexRemote(w http.ResponseWriter, r *http.Request, path string) {
	bootstrap := path == codexRemoteBase+"/enroll" || path == codexRemoteBase+"/refresh"
	relay := path == codexRemoteBase
	if (!bootstrap && !relay && path != codexRemoteBase+"/pair" && path != codexRemoteBase+"/pair/status") ||
		(relay && r.Method != http.MethodGet) || (!relay && r.Method != http.MethodPost) || r.URL.RawQuery != "" {
		jsonError(w, "unsupported remote control request", http.StatusBadRequest)
		return
	}
	if h.remoteControl == nil || h.crypto == nil {
		jsonError(w, "remote control unavailable", http.StatusServiceUnavailable)
		return
	}
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client authentication required", http.StatusUnauthorized)
		return
	}
	svc, err := h.services.GetByName("openai")
	if err != nil || !svc.IsActive {
		jsonError(w, "remote control service unavailable", http.StatusForbidden)
		return
	}
	if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		jsonError(w, "one bearer credential required", http.StatusBadRequest)
		return
	}
	installation := strings.TrimSpace(r.Header.Get("X-Codex-Installation-Id"))
	if bootstrap {
		unlock := h.remoteControl.LockHost(client.ID, installation)
		defer unlock()
	}
	var binding *queries.CodexRemoteEnrollment
	resolveReq := r.Clone(r.Context())
	resolveReq.Header = r.Header.Clone()
	if !bootstrap {
		capability := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(capability, codexRemoteCapabilityPrefix) {
			jsonError(w, "remote host capability required", http.StatusForbidden)
			return
		}
		binding, err = h.remoteControl.Capability(services.HashToken(capability), client.ID, time.Now().Unix())
		if err != nil {
			jsonError(w, "remote host capability unavailable", http.StatusForbidden)
			return
		}
		resolveReq.Header.Set("Authorization", "Bearer "+binding.Placeholder)
		if svc.AuthHeader != "" && !strings.EqualFold(svc.AuthHeader, "Authorization") {
			resolveReq.Header.Del(svc.AuthHeader)
		}
	}
	result, err := h.resolveProxyKey(resolveReq, client.ID, svc, true)
	if err != nil || result == nil || result.Error != "" || result.NeedApproval || !result.IsRefreshable {
		jsonError(w, "remote control assignment unavailable or requires approval", http.StatusForbidden)
		return
	}
	account := h.codexOAuthSourceAccountID(result)
	if account == "" || account == "duckway-account" {
		jsonError(w, "remote control account unavailable", http.StatusForbidden)
		return
	}
	accountHash := services.HashToken(account)
	if binding != nil && (binding.Placeholder != result.Placeholder || binding.APIKeyID != result.APIKeyID || binding.AccountHash != accountHash) {
		jsonError(w, "remote control assignment changed", http.StatusForbidden)
		return
	}
	var body []byte
	var payload map[string]json.RawMessage
	if (bootstrap || relay) && (installation == "" || len(installation) > 256) {
		jsonError(w, "remote installation required", http.StatusBadRequest)
		return
	}
	if !relay {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
		if err != nil || json.Unmarshal(body, &payload) != nil {
			jsonError(w, "invalid remote request body", http.StatusBadRequest)
			return
		}
	}
	if bootstrap {
		var bodyInstallation, serverID string
		_ = json.Unmarshal(payload["installation_id"], &bodyInstallation)
		if bodyInstallation != installation {
			jsonError(w, "remote installation mismatch", http.StatusBadRequest)
			return
		}
		if path == codexRemoteBase+"/refresh" {
			_ = json.Unmarshal(payload["server_id"], &serverID)
			binding, err = h.remoteControl.Host(client.ID, result.Placeholder, serverID, installation)
			if err != nil || binding.Placeholder != result.Placeholder || binding.APIKeyID != result.APIKeyID || binding.AccountHash != accountHash {
				jsonError(w, "remote host not bound to assignment", http.StatusForbidden)
				return
			}
		}
	} else if relay && (binding.InstallationID != installation || r.Header.Get("X-Codex-Server-Id") != binding.ServerID) {
		jsonError(w, "remote host mismatch", http.StatusForbidden)
		return
	}
	for _, layer := range []proxyACLLayer{{"service", svc.DefaultACL}, {"api_key", result.APIKeyACL}, {"placeholder", result.PermissionConfig}} {
		if layer.config != "" && !h.permissions.Check(layer.config, result.PlaceholderID, r.Method, path, body).Allowed {
			jsonError(w, "remote control permission denied", http.StatusForbidden)
			return
		}
	}
	if relay {
		validation := r.Clone(r.Context())
		validation.Header = r.Header.Clone()
		if validateCodexWebSocketRequest(validation, "/backend-api/codex/responses") != nil || !isProxyWebSocketUpgrade(r) {
			jsonError(w, "invalid remote websocket", http.StatusBadRequest)
			return
		}
	}
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, "https://chatgpt.com"+path, bytes.NewReader(body))
	if err != nil {
		jsonError(w, "invalid remote request", http.StatusBadRequest)
		return
	}
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if shouldStripHeader(key) || lower == "chatgpt-account-id" || lower == "sec-websocket-extensions" {
			continue
		}
		if lower != "content-type" && lower != "user-agent" && !strings.HasPrefix(lower, "x-codex-") && !strings.HasPrefix(lower, "sec-websocket-") {
			continue
		}
		for _, value := range values {
			upstream.Header.Add(key, value)
		}
	}
	if bootstrap {
		upstream.Header.Set("Authorization", "Bearer "+result.RealKey)
		upstream.Header.Set("ChatGPT-Account-Id", account)
	} else {
		token, decryptErr := h.crypto.Decrypt(binding.TokenEncrypted)
		if decryptErr != nil {
			jsonError(w, "remote host credential unavailable", http.StatusBadGateway)
			return
		}
		upstream.Header.Set("Authorization", "Bearer "+token)
	}
	if relay {
		upstream.Header.Set("Connection", "Upgrade")
		upstream.Header.Set("Upgrade", "websocket")
	}
	upstream.Header.Set("Accept-Encoding", "identity")
	httpClient, err := h.httpClientForUpstream(result.UpstreamProxyURL)
	if err != nil {
		jsonError(w, "remote upstream unavailable", http.StatusBadGateway)
		return
	}
	// Redirects could disclose a remote-host bearer outside the fixed origin.
	boundedClient := *httpClient
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := boundedClient.Do(upstream)
	if err != nil {
		jsonError(w, "remote upstream request failed", http.StatusBadGateway)
		return
	}
	if relay && resp.StatusCode == http.StatusSwitchingProtocols {
		if !isProxyWebSocketUpgradeResponse(resp) {
			resp.Body.Close()
			jsonError(w, "invalid remote websocket response", http.StatusBadGateway)
			return
		}
		if h.requestLog != nil {
			h.requestLog.Log(client.ID, result.PlaceholderID, "openai-chatgpt", r.Method, path, resp.StatusCode)
		}
		stop := watchCodexRemoteRelay(r.Context(), resp.Body, time.Unix(binding.ExpiresAt, 0), time.Second, func(ctx context.Context) bool {
			current, lookupErr := h.remoteControl.RelayAuthorization(ctx, binding, time.Now().Unix())
			if lookupErr != nil {
				return false
			}
			token, decryptErr := h.crypto.Decrypt(current.KeyEncrypted)
			if decryptErr != nil {
				return false
			}
			info, parseErr := parseSubscriptionInfo(current.SubscriptionInfo)
			if parseErr != nil || !isCodexOAuthInfo(info, current.TokenEndpoint) {
				return false
			}
			account := ""
			for _, candidate := range []string{token, stringFromMap(info, "id_token")} {
				if isDuckwayCodexPhantomJWT(candidate) {
					continue
				}
				claims := parseJWTClaims(candidate)
				auth, _ := claims["https://api.openai.com/auth"].(map[string]interface{})
				account, _ = auth["chatgpt_account_id"].(string)
				if account != "" && account != "duckway-account" {
					break
				}
			}
			if account == "" || account == "duckway-account" || services.HashToken(account) != binding.AccountHash {
				return false
			}
			for _, acl := range []string{current.ServiceACL, current.KeyACL, current.PlaceholderACL} {
				if acl != "" && !codexRemoteRelayACLAllowed(h.permissions, acl, current.PlaceholderID, r.Method, path) {
					return false
				}
			}
			return true
		})
		defer stop()
		h.relayProxyWebSocket(w, resp, nil)
		return
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(response) > 1024*1024 {
		jsonError(w, "invalid remote response", http.StatusBadGateway)
		return
	}
	if bootstrap && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var document map[string]json.RawMessage
		var token, serverID, expires string
		if json.Unmarshal(response, &document) != nil {
			jsonError(w, "invalid remote enrollment", http.StatusBadGateway)
			return
		}
		_ = json.Unmarshal(document["remote_control_token"], &token)
		_ = json.Unmarshal(document["server_id"], &serverID)
		_ = json.Unmarshal(document["expires_at"], &expires)
		expiry, parseErr := time.Parse(time.RFC3339, expires)
		if parseErr != nil || !expiry.After(time.Now()) || token == "" || serverID == "" || len(serverID) > 256 || len(token) > 64*1024 || (binding != nil && binding.ServerID != serverID) {
			jsonError(w, "invalid remote enrollment", http.StatusBadGateway)
			return
		}
		capability, generateErr := services.GenerateToken(32)
		encrypted, encryptErr := h.crypto.Encrypt(token)
		if generateErr != nil || encryptErr != nil {
			jsonError(w, "remote enrollment unavailable", http.StatusInternalServerError)
			return
		}
		capability = codexRemoteCapabilityPrefix + capability
		binding = &queries.CodexRemoteEnrollment{CapabilityHash: services.HashToken(capability), ClientID: client.ID, Placeholder: result.Placeholder, APIKeyID: result.APIKeyID, AccountHash: accountHash, ServerID: serverID, InstallationID: installation, TokenEncrypted: encrypted, ExpiresAt: expiry.Unix()}
		if h.remoteControl.Save(binding, time.Now().Unix()) != nil {
			jsonError(w, "remote enrollment storage unavailable", http.StatusServiceUnavailable)
			return
		}
		document["remote_control_token"], _ = json.Marshal(capability)
		response, _ = json.Marshal(document)
	}
	// Only metadata is recorded. Enrollment, pairing codes and relay frames are
	// credentials/control traffic and must never enter request detail or usage.
	if h.requestLog != nil {
		h.requestLog.Log(client.ID, result.PlaceholderID, "openai-chatgpt", r.Method, path, resp.StatusCode)
	}
	if retry := resp.Header.Get("Retry-After"); len(retry) <= 128 {
		w.Header().Set("Retry-After", retry)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(response)
}

// The upstream close unblocks both stream copiers; stopping waits for the watcher.
func watchCodexRemoteRelay(ctx context.Context, upstream io.Closer, expiry time.Time, interval time.Duration, valid func(context.Context) bool) func() {
	ctx, cancel := context.WithDeadline(ctx, expiry)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var result chan bool
		var checkerDone chan struct{}
		defer func() {
			cancel()
			upstream.Close()
			if checkerDone != nil {
				<-checkerDone
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if result != nil {
					continue
				}
				result = make(chan bool, 1)
				checkerDone = make(chan struct{})
				go func(out chan bool, done chan struct{}) {
					defer close(done)
					checkCtx, checkCancel := context.WithTimeout(ctx, interval)
					defer checkCancel()
					out <- valid(checkCtx)
				}(result, checkerDone)
			case ok := <-result:
				<-checkerDone
				checkerDone = nil
				result = nil
				if !ok {
					return
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

// Established relays recheck policy without charging another request per tick.
func codexRemoteRelayACLAllowed(checker *services.PermissionChecker, acl, placeholder, method, path string) bool {
	config, err := services.ParsePermissionConfig(acl)
	if err != nil {
		return false
	}
	for i := range config.Rules {
		config.Rules[i].RateLimit = nil
	}
	policy, err := json.Marshal(config)
	return err == nil && checker.Check(string(policy), placeholder, method, path, nil).Allowed
}
