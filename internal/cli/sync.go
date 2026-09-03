package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/spf13/cobra"
)

// ============================================
// Sync
// ============================================

func syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync [dir]",
		Short: "Mirror mouse.yaml's command deny list into opencode.json",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			n, path, err := syncOpencodeJSON(dir)
			if err != nil {
				return err
			}
			fmt.Printf("synced %d deny patterns -> %s\n", n, path)
			fmt.Println("ask patterns stay plugin-only: native ask is auto-approved under the daemon's --auto spawn")
			return nil
		},
	}
}

// syncOpencodeJSON rewrites opencode.json's permission.bash map from
// mouse.yaml's commands.deny, so the two files cannot drift. The bash map is
// fully managed (stale denies are dropped); every other key is preserved.
// commands.ask is deliberately not mirrored — the plugin enforces it, and a
// native ask would prompt only to have the plugin throw right after.
func syncOpencodeJSON(dir string) (int, string, error) {
	mouse, err := config.LoadMouse(filepath.Join(dir, "mouse.yaml"))
	if err != nil {
		return 0, "", err
	}
	if mouse == nil {
		return 0, "", fmt.Errorf("no mouse.yaml in %s", dir)
	}

	path := filepath.Join(dir, "opencode.json")
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &root); err != nil {
			return 0, "", fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return 0, "", err
	}

	if _, ok := root["$schema"]; !ok {
		root["$schema"] = "https://opencode.ai/config.json"
	}
	perm, _ := root["permission"].(map[string]any)
	if perm == nil {
		perm = map[string]any{}
	}

	// Keep whatever catch-all is already there: "ask" is answerable in a TTY
	// and auto-approved headless, so it is the right default either way.
	fallback := "ask"
	if old, ok := perm["bash"].(map[string]any); ok {
		if v, ok := old["*"].(string); ok {
			fallback = v
		}
	}
	bash := map[string]any{"*": fallback}
	for _, p := range mouse.Permissions.Commands.Deny {
		bash[p] = "deny" // a literal "*" in deny intentionally wins over the fallback
	}
	perm["bash"] = bash
	if _, ok := perm["edit"]; !ok {
		perm["edit"] = "allow"
	}
	root["permission"] = perm

	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return 0, "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0644); err != nil {
		return 0, "", err
	}
	return len(mouse.Permissions.Commands.Deny), path, nil
}
