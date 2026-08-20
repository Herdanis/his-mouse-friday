package config

import (
	"os"
	"path/filepath"

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
	FS       FSPermissions       `yaml:"fs"`
	Commands CommandPermissions  `yaml:"commands"`
}

// FSPermissions: gitignore-style patterns. deny=blocked, ask=approval
// required, default=allowed. Deny wins over ask. Empty = allow all.
type FSPermissions struct {
	Deny []string `yaml:"deny"`
	Ask  []string `yaml:"ask"`
}

// CommandPermissions: same three modes as FS, prefix-matched.
type CommandPermissions struct {
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

// GlobalMousePath returns the path to the global default mouse.yaml.
func GlobalMousePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hmf/mouse.yaml"
	}
	return filepath.Join(home, ".hmf", "mouse.yaml")
}

// LoadGlobalMouse reads ~/.hmf/mouse.yaml. Missing → (nil, nil). Fallback for
// unregistered dirs.
func LoadGlobalMouse() (*MouseConfig, error) {
	return LoadMouse(GlobalMousePath())
}

// ResolveMouse loads repoPath/mouse.yaml, falling back to the global default.
// (nil, nil) if neither exists.
func ResolveMouse(repoPath string) (*MouseConfig, error) {
	cfg, err := LoadMouse(filepath.Join(repoPath, "mouse.yaml"))
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		return cfg, nil
	}
	return LoadGlobalMouse()
}
