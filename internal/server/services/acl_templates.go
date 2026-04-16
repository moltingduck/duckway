package services

// ACLTemplate is a pre-built permission configuration for a specific service.
// Admins can pick a template to apply as the service's default_acl, which
// is used when a placeholder key has no permission_config of its own.
type ACLTemplate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Config      string `json:"config"` // JSON PermissionConfig
}

// ACLTemplatesByService maps service name → list of available templates.
// Each service has an "allow-all" template first (the default), followed by
// progressively restrictive templates derived from the service's official
// API documentation.
var ACLTemplatesByService = map[string][]ACLTemplate{
	"openai": {
		{
			ID:          "allow-all",
			Name:        "Allow All (default)",
			Description: "No restrictions — all OpenAI endpoints permitted",
			Config:      "",
		},
		{
			ID:          "chat-only",
			Name:        "Chat Only",
			Description: "Only /v1/chat/completions and /v1/models (read)",
			Config: `{
  "version": "1", "provider": "openai",
  "rules": [{
    "name": "chat-only",
    "endpoints": [
      {"method": "POST", "path": "/v1/chat/completions", "allow": true},
      {"method": "GET", "path": "/v1/models", "allow": true},
      {"method": "GET", "path": "/v1/models/*", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "chat-embeddings",
			Name:        "Chat + Embeddings",
			Description: "Chat completions, embeddings, moderations (most agent workloads)",
			Config: `{
  "version": "1", "provider": "openai",
  "rules": [{
    "name": "chat-embeddings",
    "endpoints": [
      {"method": "POST", "path": "/v1/chat/completions", "allow": true},
      {"method": "POST", "path": "/v1/embeddings", "allow": true},
      {"method": "POST", "path": "/v1/moderations", "allow": true},
      {"method": "GET", "path": "/v1/models", "allow": true},
      {"method": "GET", "path": "/v1/models/*", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "inference-all",
			Name:        "Full Inference",
			Description: "All inference endpoints: chat, embeddings, images, audio, moderations",
			Config: `{
  "version": "1", "provider": "openai",
  "rules": [{
    "name": "inference-all",
    "endpoints": [
      {"method": "POST", "path": "/v1/chat/completions", "allow": true},
      {"method": "POST", "path": "/v1/completions", "allow": true},
      {"method": "POST", "path": "/v1/embeddings", "allow": true},
      {"method": "POST", "path": "/v1/moderations", "allow": true},
      {"method": "POST", "path": "/v1/images/generations", "allow": true},
      {"method": "POST", "path": "/v1/images/edits", "allow": true},
      {"method": "POST", "path": "/v1/images/variations", "allow": true},
      {"method": "POST", "path": "/v1/audio/transcriptions", "allow": true},
      {"method": "POST", "path": "/v1/audio/translations", "allow": true},
      {"method": "POST", "path": "/v1/audio/speech", "allow": true},
      {"method": "GET", "path": "/v1/models", "allow": true},
      {"method": "GET", "path": "/v1/models/*", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "no-admin",
			Name:        "No Admin (block org/fine-tuning)",
			Description: "Block organization, fine-tuning, file management, and batches",
			Config: `{
  "version": "1", "provider": "openai",
  "rules": [{
    "name": "no-admin",
    "endpoints": [
      {"method": "*", "path": "/v1/organization/*", "allow": false},
      {"method": "*", "path": "/v1/fine_tuning/*", "allow": false},
      {"method": "*", "path": "/v1/files", "allow": false},
      {"method": "*", "path": "/v1/files/*", "allow": false},
      {"method": "*", "path": "/v1/batches", "allow": false},
      {"method": "*", "path": "/v1/batches/*", "allow": false}
    ]
  }]
}`,
		},
	},

	"anthropic": {
		{
			ID:          "allow-all",
			Name:        "Allow All (default)",
			Description: "No restrictions — all Anthropic endpoints permitted",
			Config:      "",
		},
		{
			ID:          "messages-only",
			Name:        "Messages Only",
			Description: "Only /v1/messages (standard chat completion)",
			Config: `{
  "version": "1", "provider": "anthropic",
  "rules": [{
    "name": "messages-only",
    "endpoints": [
      {"method": "POST", "path": "/v1/messages", "allow": true},
      {"method": "GET", "path": "/v1/models", "allow": true},
      {"method": "GET", "path": "/v1/models/*", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "no-batches",
			Name:        "No Batches / No Files",
			Description: "Block message batches, files API, admin endpoints",
			Config: `{
  "version": "1", "provider": "anthropic",
  "rules": [{
    "name": "no-batches",
    "endpoints": [
      {"method": "*", "path": "/v1/messages/batches", "allow": false},
      {"method": "*", "path": "/v1/messages/batches/*", "allow": false},
      {"method": "*", "path": "/v1/files", "allow": false},
      {"method": "*", "path": "/v1/files/*", "allow": false},
      {"method": "*", "path": "/v1/organizations/*", "allow": false}
    ]
  }]
}`,
		},
	},

	"github": {
		{
			ID:          "allow-all",
			Name:        "Allow All (default)",
			Description: "No restrictions — all GitHub API endpoints permitted",
			Config:      "",
		},
		{
			ID:          "read-only",
			Name:        "Read-Only",
			Description: "Only GET requests across all endpoints",
			Config: `{
  "version": "1", "provider": "github",
  "rules": [{
    "name": "read-only",
    "endpoints": [
      {"method": "GET", "path": "/*", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "repo-read",
			Name:        "Repository Read",
			Description: "Read access to repos, users, orgs, search",
			Config: `{
  "version": "1", "provider": "github",
  "rules": [{
    "name": "repo-read",
    "endpoints": [
      {"method": "GET", "path": "/user", "allow": true},
      {"method": "GET", "path": "/users/*", "allow": true},
      {"method": "GET", "path": "/repos/*", "allow": true},
      {"method": "GET", "path": "/orgs/*", "allow": true},
      {"method": "GET", "path": "/search/*", "allow": true},
      {"method": "GET", "path": "/rate_limit", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "issues-prs",
			Name:        "Issues + PRs",
			Description: "Read/write issues and pull requests, read repos",
			Config: `{
  "version": "1", "provider": "github",
  "rules": [{
    "name": "issues-prs",
    "endpoints": [
      {"method": "GET", "path": "/user", "allow": true},
      {"method": "GET", "path": "/repos/*", "allow": true},
      {"method": "GET", "path": "/search/*", "allow": true},
      {"method": "POST", "path": "/repos/*/issues", "allow": true},
      {"method": "PATCH", "path": "/repos/*/issues/*", "allow": true},
      {"method": "POST", "path": "/repos/*/issues/*/comments", "allow": true},
      {"method": "POST", "path": "/repos/*/pulls", "allow": true},
      {"method": "PATCH", "path": "/repos/*/pulls/*", "allow": true},
      {"method": "POST", "path": "/repos/*/pulls/*/reviews", "allow": true},
      {"method": "GET", "path": "/rate_limit", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "no-destructive",
			Name:        "No Destructive Actions",
			Description: "Block DELETE on repos, orgs, users, and force-push / branch delete",
			Config: `{
  "version": "1", "provider": "github",
  "rules": [{
    "name": "no-destructive",
    "endpoints": [
      {"method": "DELETE", "path": "/repos/*", "allow": false},
      {"method": "DELETE", "path": "/orgs/*", "allow": false},
      {"method": "DELETE", "path": "/user/*", "allow": false},
      {"method": "DELETE", "path": "/repos/*/git/refs/*", "allow": false}
    ]
  }]
}`,
		},
		{
			ID:          "gists-only",
			Name:        "Gists Only",
			Description: "Only /gists/* endpoints",
			Config: `{
  "version": "1", "provider": "github",
  "rules": [{
    "name": "gists-only",
    "endpoints": [
      {"method": "*", "path": "/gists", "allow": true},
      {"method": "*", "path": "/gists/*", "allow": true},
      {"method": "GET", "path": "/user", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
	},

	"discord": {
		{
			ID:          "allow-all",
			Name:        "Allow All (default)",
			Description: "No restrictions — all Discord API endpoints permitted",
			Config:      "",
		},
		{
			ID:          "webhook-only",
			Name:        "Webhooks Only",
			Description: "Only POST to webhook URLs (no bot API access)",
			Config: `{
  "version": "1", "provider": "discord",
  "rules": [{
    "name": "webhook-only",
    "endpoints": [
      {"method": "POST", "path": "/webhooks/*", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "messages-only",
			Name:        "Messages Only",
			Description: "Send/read channel messages, no guild/role admin",
			Config: `{
  "version": "1", "provider": "discord",
  "rules": [{
    "name": "messages-only",
    "endpoints": [
      {"method": "GET", "path": "/channels/*", "allow": true},
      {"method": "POST", "path": "/channels/*/messages", "allow": true},
      {"method": "PATCH", "path": "/channels/*/messages/*", "allow": true},
      {"method": "POST", "path": "/webhooks/*", "allow": true},
      {"method": "GET", "path": "/users/@me", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "read-only",
			Name:        "Read-Only",
			Description: "GET requests only (no sending or modification)",
			Config: `{
  "version": "1", "provider": "discord",
  "rules": [{
    "name": "read-only",
    "endpoints": [
      {"method": "GET", "path": "/*", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
	},

	"telegram": {
		{
			ID:          "allow-all",
			Name:        "Allow All (default)",
			Description: "No restrictions — all Telegram Bot API methods permitted",
			Config:      "",
		},
		{
			ID:          "send-only",
			Name:        "Send Only",
			Description: "Only sendMessage/sendPhoto/sendDocument/etc.",
			Config: `{
  "version": "1", "provider": "telegram",
  "rules": [{
    "name": "send-only",
    "endpoints": [
      {"method": "*", "path": "/bot*/sendMessage", "allow": true},
      {"method": "*", "path": "/bot*/sendPhoto", "allow": true},
      {"method": "*", "path": "/bot*/sendDocument", "allow": true},
      {"method": "*", "path": "/bot*/sendVideo", "allow": true},
      {"method": "*", "path": "/bot*/sendAudio", "allow": true},
      {"method": "*", "path": "/bot*/sendAnimation", "allow": true},
      {"method": "*", "path": "/bot*/sendVoice", "allow": true},
      {"method": "*", "path": "/bot*/sendLocation", "allow": true},
      {"method": "*", "path": "/bot*/sendContact", "allow": true},
      {"method": "*", "path": "/bot*/getMe", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "read-only",
			Name:        "Read-Only",
			Description: "Only getMe, getUpdates, getChat — no sending",
			Config: `{
  "version": "1", "provider": "telegram",
  "rules": [{
    "name": "read-only",
    "endpoints": [
      {"method": "*", "path": "/bot*/getMe", "allow": true},
      {"method": "*", "path": "/bot*/getUpdates", "allow": true},
      {"method": "*", "path": "/bot*/getChat", "allow": true},
      {"method": "*", "path": "/bot*/getChatMember", "allow": true},
      {"method": "*", "path": "/bot*/getChatMemberCount", "allow": true}
    ],
    "deny_all_other": true
  }]
}`,
		},
		{
			ID:          "no-admin",
			Name:        "No Admin Actions",
			Description: "Block ban/kick/delete operations on chat members and messages",
			Config: `{
  "version": "1", "provider": "telegram",
  "rules": [{
    "name": "no-admin",
    "endpoints": [
      {"method": "*", "path": "/bot*/banChatMember", "allow": false},
      {"method": "*", "path": "/bot*/unbanChatMember", "allow": false},
      {"method": "*", "path": "/bot*/restrictChatMember", "allow": false},
      {"method": "*", "path": "/bot*/promoteChatMember", "allow": false},
      {"method": "*", "path": "/bot*/deleteMessage", "allow": false},
      {"method": "*", "path": "/bot*/deleteChatPhoto", "allow": false},
      {"method": "*", "path": "/bot*/setChatPhoto", "allow": false},
      {"method": "*", "path": "/bot*/setChatTitle", "allow": false}
    ]
  }]
}`,
		},
	},
}

// GetACLTemplates returns the templates for a given service name.
func GetACLTemplates(serviceName string) []ACLTemplate {
	return ACLTemplatesByService[serviceName]
}

// GetACLTemplate returns a specific template by service + ID.
func GetACLTemplate(serviceName, templateID string) *ACLTemplate {
	for _, t := range ACLTemplatesByService[serviceName] {
		if t.ID == templateID {
			return &t
		}
	}
	return nil
}
