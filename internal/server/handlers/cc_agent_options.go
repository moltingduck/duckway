package handlers

import (
	"encoding/json"
	"fmt"
)

const defaultCodexSandbox = "workspace-write"

var allowedCodexSandboxes = map[string]bool{
	"read-only":          true,
	"workspace-write":    true,
	"danger-full-access": true,
	"none":               true,
}

func normalizeCCConfigForAgent(agentType, config string) (string, error) {
	var cfg map[string]interface{}
	if config == "" {
		config = "{}"
	}
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("invalid config json")
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	opts, _ := cfg["agent_options"].(map[string]interface{})
	if opts == nil {
		opts = map[string]interface{}{}
	}
	switch agentType {
	case "codex":
		sandbox, _ := opts["sandbox"].(string)
		if sandbox == "" {
			sandbox = defaultCodexSandbox
		}
		if !allowedCodexSandboxes[sandbox] {
			return "", fmt.Errorf("invalid codex sandbox: %s", sandbox)
		}
		opts = map[string]interface{}{"sandbox": sandbox}
	case "claude_code", "openclaw":
		opts = map[string]interface{}{}
	default:
		opts = map[string]interface{}{}
	}
	if len(opts) == 0 {
		delete(cfg, "agent_options")
	} else {
		cfg["agent_options"] = opts
	}
	out, _ := json.Marshal(cfg)
	return string(out), nil
}

func agentOptionsForClient(agentType, config string) map[string]string {
	normalized, err := normalizeCCConfigForAgent(agentType, config)
	if err != nil {
		return nil
	}
	var cfg struct {
		AgentOptions map[string]string `json:"agent_options"`
	}
	_ = json.Unmarshal([]byte(normalized), &cfg)
	if cfg.AgentOptions == nil {
		return map[string]string{}
	}
	return cfg.AgentOptions
}
