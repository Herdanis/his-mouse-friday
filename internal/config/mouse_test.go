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
	if cfg.Agent.Primary.Provider != "opencode" {
		t.Errorf("primary.provider: got %q want opencode", cfg.Agent.Primary.Provider)
	}
	if cfg.Agent.Primary.Model != "default" {
		t.Errorf("primary.model: got %q want default", cfg.Agent.Primary.Model)
	}
	if cfg.Agent.Secondary.Provider != "" {
		t.Errorf("secondary.provider: got %q want empty", cfg.Agent.Secondary.Provider)
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
	asked := cfg.Permissions.FS.Ask
	if len(asked) != 2 {
		t.Fatalf("ask list: got %d entries want 2", len(asked))
	}
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
	if cfg.Agent.Primary.Provider != "opencode" {
		t.Errorf("primary.provider: got %q", cfg.Agent.Primary.Provider)
	}
	if cfg.A2A.AllowInbound {
		t.Errorf("default allow_inbound should be false, got true")
	}
}
