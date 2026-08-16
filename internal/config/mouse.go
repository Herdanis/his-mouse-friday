package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type MouseConfig struct {
	Agent       AgentConfig       `yaml:"agent"`
	Permissions PermissionsConfig `yaml:"permissions"`
	A2A         A2AConfig         `yaml:"a2a"`
}

type AgentConfig struct {
	Primary   AgentTarget `yaml:"primary"`
	Secondary AgentTarget `yaml:"secondary"`
}

// AgentTarget identifies a coding agent runtime + model.
// Empty provider = unset (secondary optional).
type AgentTarget struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type PermissionsConfig struct {
	FS FSPermissions `yaml:"fs"`
}

// FSPermissions uses gitignore-style patterns with three modes:
//   - deny: blocked outright
//   - ask:  requires human approval before access
//   - (default): allowed
// Deny wins over ask. Empty lists = allow all.
type FSPermissions struct {
	Deny []string `yaml:"deny"`
	Ask  []string `yaml:"ask"`
}

type A2AConfig struct {
	AllowInbound  bool `yaml:"allow_inbound"`
	AllowOutbound bool `yaml:"allow_outbound"`
}

// LoadMouse reads mouse.yaml at path. Missing file → (nil, nil) = open mode.
func LoadMouse(path string) (*MouseConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg MouseConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
