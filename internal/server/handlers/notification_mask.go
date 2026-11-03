package handlers

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// Notification channel configs hold secrets (bot tokens, webhook URLs that
// embed a token, shared secrets). The browser never receives them: every GET
// returns the ccSecretMask sentinel in their place, and a PUT that sends the
// sentinel back keeps the stored value (same contract as cc_mask.go).

// notificationSecretKeys lists, per channel type, the config keys that are
// always treated as secret. Any other key that ccIsSecretKey flags is masked
// too, so a future config field fails closed.
var notificationSecretKeys = map[string][]string{
	"telegram":    {"bot_token"},
	"discord":     {"webhook_url"},
	"discord_bot": {"bot_token"},
	"webhook":     {"url", "secret"}, // generic webhook URLs commonly embed a token
}

// notificationRequiredKeys are the fields each channel type needs.
var notificationRequiredKeys = map[string][]string{
	"telegram":    {"bot_token", "chat_id"},
	"discord":     {"webhook_url"},
	"discord_bot": {"bot_token", "channel_id"},
	"webhook":     {"url"},
}

func notificationKeyIsSecret(channelType, key string) bool {
	for _, k := range notificationSecretKeys[channelType] {
		if k == key {
			return true
		}
	}
	return ccIsSecretKey(key)
}

func parseNotificationConfig(config string) map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(config), &m); err != nil || m == nil {
		return map[string]interface{}{}
	}
	return m
}

// maskNotificationConfig returns config JSON with secret values replaced by
// the mask. Empty secrets stay empty so the UI can tell "set" from "not set".
// Unparseable config yields "{}" rather than leaking verbatim.
func maskNotificationConfig(channelType, config string) string {
	m := parseNotificationConfig(config)
	for k, v := range m {
		if !notificationKeyIsSecret(channelType, k) {
			// Nested values are masked recursively (validation also rejects
			// them, but stored configs may predate that rule).
			m[k] = maskCCValue(v)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		if v != nil {
			m[k] = ccSecretMask
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// restoreNotificationConfig replaces mask sentinels in an incoming config with
// the stored values. A sentinel with no stored value is dropped, never stored
// literally.
func restoreNotificationConfig(channelType, incoming, stored string) string {
	in := parseNotificationConfig(incoming)
	st := parseNotificationConfig(stored)
	for k, v := range in {
		if s, ok := v.(string); ok && s == ccSecretMask && notificationKeyIsSecret(channelType, k) {
			if old, has := st[k]; has {
				in[k] = old
			} else {
				delete(in, k)
			}
		}
	}
	out, _ := json.Marshal(in)
	return string(out)
}

// configHasMaskSentinel reports whether any secret field is still the mask.
func configHasMaskSentinel(channelType, config string) bool {
	for k, v := range parseNotificationConfig(config) {
		if s, ok := v.(string); ok && s == ccSecretMask && notificationKeyIsSecret(channelType, k) {
			return true
		}
	}
	return false
}

func configString(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// validateNotificationConfig checks required fields and URL shapes.
func validateNotificationConfig(channelType, config string) string {
	var raw interface{}
	if err := json.Unmarshal([]byte(config), &raw); err != nil {
		return "config must be valid JSON"
	}
	if _, ok := raw.(map[string]interface{}); !ok {
		return "config must be a JSON object"
	}
	m := parseNotificationConfig(config)
	for k, v := range m {
		switch v.(type) {
		case map[string]interface{}, []interface{}:
			return k + " must be a plain value, not an object or list"
		}
	}
	for _, k := range notificationRequiredKeys[channelType] {
		if configString(m, k) == "" {
			return k + " is required"
		}
	}
	urlKey := map[string]string{"discord": "webhook_url", "webhook": "url"}[channelType]
	if urlKey != "" {
		u, err := url.Parse(configString(m, urlKey))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return urlKey + " must be an http(s) URL"
		}
	}
	return ""
}

// notificationSummary is a non-secret, human-readable description of where a
// channel delivers to. It never contains a token or full URL.
func notificationSummary(channelType, config string) string {
	m := parseNotificationConfig(config)
	switch channelType {
	case "telegram":
		return "Chat " + configString(m, "chat_id")
	case "discord":
		if u, err := url.Parse(configString(m, "webhook_url")); err == nil && u.Host != "" {
			return "Webhook on " + u.Host
		}
		return "Discord webhook"
	case "discord_bot":
		return "Channel " + configString(m, "channel_id")
	case "webhook":
		if u, err := url.Parse(configString(m, "url")); err == nil && u.Host != "" {
			return "POST to " + u.Host
		}
		return "Webhook"
	}
	return ""
}

// scrubSecrets removes any stored secret values from free text (for example
// transport errors that echo a request URL containing a bot token).
func scrubSecrets(channelType, config, text string) string {
	for k, v := range parseNotificationConfig(config) {
		if !notificationKeyIsSecret(channelType, k) {
			continue
		}
		s, ok := v.(string)
		if !ok || len(s) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, s, ccSecretMask)
		if u, err := url.Parse(s); err == nil && u.Path != "" && len(u.Path) >= 4 {
			text = strings.ReplaceAll(text, u.Path, ccSecretMask)
		}
		text = strings.ReplaceAll(text, url.PathEscape(s), ccSecretMask)
	}
	return text
}

// NotificationView is the admin-API shape of a channel: config is masked.
type NotificationView struct {
	ID          string `json:"id"`
	ChannelType string `json:"channel_type"`
	Name        string `json:"name"`
	Config      string `json:"config"`
	IsActive    bool   `json:"is_active"`
	CreatedAt   string `json:"created_at"`
	Summary     string `json:"summary"`
}

func notificationView(c queries.NotificationChannel) NotificationView {
	return NotificationView{
		ID: c.ID, ChannelType: c.ChannelType, Name: c.Name,
		Config:    maskNotificationConfig(c.ChannelType, c.Config),
		IsActive:  c.IsActive,
		CreatedAt: c.CreatedAt,
		Summary:   notificationSummary(c.ChannelType, c.Config),
	}
}

// NotificationViews converts stored channels to their masked views.
func NotificationViews(list []queries.NotificationChannel) []NotificationView {
	out := make([]NotificationView, 0, len(list))
	for _, c := range list {
		out = append(out, notificationView(c))
	}
	return out
}
