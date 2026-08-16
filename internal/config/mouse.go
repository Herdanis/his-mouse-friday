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

// FSPermissions uses gitignore-style patterns with three modes:
//   - deny: blocked outright
//   - ask:  requires human approval before access
//   - (default): allowed
// Deny wins over ask. Empty lists = allow all.
type FSPermissions struct {
	Deny []string `yaml:"deny"`
	Ask  []string `yaml:"ask"`
}

// CommandPermissions controls CLI commands the agent may run.
// Patterns match command prefixes (e.g. "kubectl delete" matches
// "kubectl delete pods foo"). Same three modes as FS; deny wins over ask.
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

// LoadGlobalMouse reads the global default mouse.yaml (~/.hmf/mouse.yaml).
// Missing file → (nil, nil). This is the fallback for unregistered dirs.
func LoadGlobalMouse() (*MouseConfig, error) {
	return LoadMouse(GlobalMousePath())
}

// ResolveMouse loads project-specific mouse.yaml from repoPath, falling back
// to the global default (~/.hmf/mouse.yaml) if not found.
// Returns (nil, nil) if neither exists (truly open mode).
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
