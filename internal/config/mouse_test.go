package config

import (
	"path/filepath"
	"testing"
)

func TestLoadMouse_Valid(t *testing.T) {
	cfg, err := LoadMouse(filepath.Join("..", "..", "testdata", "mouse_valid.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.Primary != "opencode" {
		t.Errorf("primary: got %q want opencode", cfg.Agent.Primary)
	}
	if cfg.Agent.Model != "default" {
		t.Errorf("model: got %q want default", cfg.Agent.Model)
	}
	if !cfg.A2A.AllowInbound {
		t.Errorf("allow_inbound: got false want true")
	}
	denied := cfg.Permissions.FS.Deny
	if len(denied) != 4 {
		t.Fatalf("deny list: got %d entries want 4", len(denied))
	}
	foundEnv := false
	for _, p := range denied {
		if p == ".env" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Errorf("deny list missing .env: %v", denied)
	}
	// Default: empty deny = allow all. Minimal config has no permissions block.
}

func TestLoadMouse_MissingFile(t *testing.T) {
	cfg, err := LoadMouse("/nonexistent/mouse.yaml")
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("missing file should return nil cfg, got %+v", cfg)
	}
}

func TestLoadMouse_Minimal(t *testing.T) {
	cfg, err := LoadMouse(filepath.Join("..", "..", "testdata", "mouse_minimal.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.Primary != "opencode" {
		t.Errorf("primary: got %q", cfg.Agent.Primary)
	}
	if cfg.A2A.AllowInbound {
		t.Errorf("default allow_inbound should be false, got true")
	}
}
