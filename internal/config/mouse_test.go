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
	if cfg.Permissions.FS["paths"]["src/**"] != "allow" {
		t.Errorf("fs.paths src/**: got %q want allow", cfg.Permissions.FS["paths"]["src/**"])
	}
	if cfg.Permissions.Azure["instances"]["write"] != "deny" {
		t.Errorf("azure.instances.write: got %q want deny", cfg.Permissions.Azure["instances"]["write"])
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
	if cfg.Agent.Primary != "opencode" {
		t.Errorf("primary: got %q", cfg.Agent.Primary)
	}
	if cfg.A2A.AllowInbound {
		t.Errorf("default allow_inbound should be false, got true")
	}
}
