package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncOpencodeJSON(t *testing.T) {
	dir := t.TempDir()
	mouse := `permissions:
  commands:
    deny:
      - "rm -rf /"
      - "sudo"
    ask:
      - "git push"
`
	if err := os.WriteFile(filepath.Join(dir, "mouse.yaml"), []byte(mouse), 0644); err != nil {
		t.Fatal(err)
	}
	// Existing config carries an unrelated key, a stale deny, and a non-default
	// catch-all — the first two must survive/vanish, the third must be kept.
	existing := `{
  "mcp": {"hmf": {"type": "local"}},
  "permission": {"bash": {"*": "allow", "kubectl delete": "deny"}, "edit": "allow"}
}`
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	n, got, err := syncOpencodeJSON(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || got != path {
		t.Fatalf("got (%d, %q), want (2, %q)", n, got, path)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["mcp"]; !ok {
		t.Error("mcp block dropped")
	}
	bash := root["permission"].(map[string]any)["bash"].(map[string]any)
	if bash["rm -rf /"] != "deny" || bash["sudo"] != "deny" {
		t.Errorf("deny patterns not mirrored: %v", bash)
	}
	if _, ok := bash["kubectl delete"]; ok {
		t.Errorf("stale deny kept: %v", bash)
	}
	if bash["*"] != "allow" {
		t.Errorf("catch-all not preserved: %v", bash["*"])
	}
	if _, ok := bash["git push"]; ok {
		t.Errorf("ask pattern mirrored, should be plugin-only: %v", bash)
	}
}

func TestSyncOpencodeJSON_NoMouseYaml(t *testing.T) {
	if _, _, err := syncOpencodeJSON(t.TempDir()); err == nil {
		t.Fatal("want error when mouse.yaml missing")
	}
}
