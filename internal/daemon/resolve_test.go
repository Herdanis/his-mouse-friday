package daemon

import (
	"errors"
	"testing"

	"github.com/herdanis/his-mouse-friday/internal/config"
)

// okLookPath pretends every binary except the listed ones is installed.
func okLookPath(missing ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, m := range missing {
		set[m] = true
	}
	return func(b string) (string, error) {
		if set[b] {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + b, nil
	}
}

func TestResolveAgent_NoMouse(t *testing.T) {
	d := &Daemon{LookPath: okLookPath()}
	bin, model := d.resolveAgent(nil)
	if bin != "opencode" || model != "default" {
		t.Errorf("got %s/%s want opencode/default", bin, model)
	}
}

func TestResolveAgent_PrimaryBinaryMissing(t *testing.T) {
	d := &Daemon{LookPath: okLookPath("opencode")}
	mouse := &config.MouseConfig{Agent: config.AgentConfig{
		Primary:   config.AgentTarget{Provider: "opencode", Model: "default"},
		Secondary: config.AgentTarget{Provider: "claude", Model: "default"},
	}}
	bin, model := d.resolveAgent(mouse)
	if bin != "claude" || model != "default" {
		t.Errorf("got %s/%s want claude/default", bin, model)
	}
}

func TestResolveAgent_PrimaryModelMissing(t *testing.T) {
	d := &Daemon{
		LookPath: okLookPath(),
		ModelProbe: func(bin, model string) (bool, bool) {
			return model == "fallback-model", true
		},
	}
	mouse := &config.MouseConfig{Agent: config.AgentConfig{
		Primary:   config.AgentTarget{Provider: "opencode", Model: "primary-model"},
		Secondary: config.AgentTarget{Provider: "opencode", Model: "fallback-model"},
	}}
	bin, model := d.resolveAgent(mouse)
	if bin != "opencode" || model != "fallback-model" {
		t.Errorf("got %s/%s want opencode/fallback-model", bin, model)
	}
}

func TestResolveAgent_SecondaryDuplicateDropped(t *testing.T) {
	// Secondary identical to primary → only one candidate; even with the
	// model probe saying unavailable, result stays primary (no fallback loop).
	d := &Daemon{
		LookPath:   okLookPath(),
		ModelProbe: func(string, string) (bool, bool) { return false, true },
	}
	mouse := &config.MouseConfig{Agent: config.AgentConfig{
		Primary:   config.AgentTarget{Provider: "opencode", Model: "m"},
		Secondary: config.AgentTarget{Provider: "opencode", Model: "m"},
	}}
	bin, model := d.resolveAgent(mouse)
	if bin != "opencode" || model != "m" {
		t.Errorf("got %s/%s want opencode/m", bin, model)
	}
}

func TestResolveAgent_UncheckableModelKept(t *testing.T) {
	d := &Daemon{
		LookPath:   okLookPath(),
		ModelProbe: func(string, string) (bool, bool) { return false, false },
	}
	mouse := &config.MouseConfig{Agent: config.AgentConfig{
		Primary:   config.AgentTarget{Provider: "opencode", Model: "primary-model"},
		Secondary: config.AgentTarget{Provider: "claude", Model: "default"},
	}}
	bin, model := d.resolveAgent(mouse)
	if bin != "opencode" || model != "primary-model" {
		t.Errorf("got %s/%s want opencode/primary-model", bin, model)
	}
}
