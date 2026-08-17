package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
	"github.com/spf13/cobra"
)

func callDaemon(method string, params any) (protocol.Response, error) {
	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	req := protocol.Request{Method: method, Params: raw, ID: 1}
	conn, err := net.Dial("unix", protocol.SocketPath())
	if err != nil {
		return protocol.Response{}, fmt.Errorf("daemon not running? run 'hmf up': %w", err)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(&req); err != nil {
		return protocol.Response{}, err
	}
	var resp protocol.Response
	if err := dec.Decode(&resp); err != nil {
		return protocol.Response{}, err
	}
	return resp, nil
}

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{Use: "hmf"}

	root.AddCommand(upCmd())
	root.AddCommand(downCmd())
	root.AddCommand(workspaceCmd())
	root.AddCommand(projectCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(initCmd())
	root.AddCommand(configCmd())
	root.AddCommand(guardCmd())
	return root
}

func upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Start the hmf daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := os.MkdirAll(protocol.StateDir(), 0755); err != nil {
				return fmt.Errorf("create state dir: %w", err)
			}
			d, err := daemon.NewDaemon(protocol.SocketPath(), protocol.DBPath())
			if err != nil {
				return err
			}
			return d.Serve(context.Background())
		},
	}
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the hmf daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := callDaemon("shutdown", struct{}{})
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			return nil
		},
	}
}

func workspaceCmd() *cobra.Command {
	c := &cobra.Command{Use: "workspace"}
	add := &cobra.Command{
		Use:   "add [name]",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := callDaemon("workspace_add", map[string]any{"name": args[0]})
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			fmt.Println("workspace added:", args[0])
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List all workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := callDaemon("workspace_list", struct{}{})
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			var names []string
			if err := json.Unmarshal(resp.Result, &names); err != nil {
				return fmt.Errorf("parse: %w", err)
			}
			if len(names) == 0 {
				fmt.Println("(no workspaces)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME")
			for _, n := range names {
				fmt.Fprintf(w, "%s\n", n)
			}
			w.Flush()
			return nil
		},
	}
	del := &cobra.Command{
		Use:   "delete [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a workspace (and its projects)",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := callDaemon("workspace_delete", map[string]any{"name": args[0]})
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			fmt.Println("workspace deleted:", args[0])
			return nil
		},
	}
	c.AddCommand(add)
	c.AddCommand(list)
	c.AddCommand(del)
	return c
}

func projectCmd() *cobra.Command {
	c := &cobra.Command{Use: "project"}
	var ws string
	add := &cobra.Command{
		Use:   "add [name] [path]",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[1])
			if err != nil {
				return fmt.Errorf("resolve path: %w", err)
			}
			resp, err := callDaemon("project_add", map[string]any{"workspace": ws, "name": args[0], "path": abs})
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			fmt.Printf("project added: %s/%s -> %s\n", ws, args[0], abs)
			// Auto-guard: add this project's path to global opencode config.
			if err := guardAdd(abs); err != nil {
				fmt.Fprintf(os.Stderr, "warning: guard failed: %v\n", err)
			} else {
				fmt.Println("guarded: direct edits blocked (use engage_project_agent)")
			}
			return nil
		},
	}
	add.Flags().StringVar(&ws, "workspace", "", "workspace name")
	add.MarkFlagRequired("workspace")

	var listWs string
	list := &cobra.Command{
		Use:   "list",
		Short: "List projects (optionally filtered by workspace)",
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{}
			if listWs != "" {
				params["workspace"] = listWs
			}
			resp, err := callDaemon("project_list", params)
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			var items []struct {
				Workspace string `json:"workspace"`
				Name      string `json:"name"`
				Path      string `json:"path"`
			}
			if err := json.Unmarshal(resp.Result, &items); err != nil {
				return fmt.Errorf("parse: %w", err)
			}
			if len(items) == 0 {
				fmt.Println("(no projects)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "WORKSPACE\tNAME\tPATH")
			for _, it := range items {
				fmt.Fprintf(w, "%s\t%s\t%s\n", it.Workspace, it.Name, it.Path)
			}
			w.Flush()
			return nil
		},
	}
	list.Flags().StringVar(&listWs, "workspace", "", "filter by workspace")

	var delWs string
	del := &cobra.Command{
		Use:   "delete [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a project from a workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get path before deleting (for guard cleanup).
			listResp, _ := callDaemon("project_list", map[string]any{"workspace": delWs})
			var projects []struct {
				Workspace string `json:"workspace"`
				Name      string `json:"name"`
				Path      string `json:"path"`
			}
			json.Unmarshal(listResp.Result, &projects)
			var projPath string
			for _, p := range projects {
				if p.Name == args[0] {
					projPath = p.Path
					break
				}
			}
			resp, err := callDaemon("project_delete", map[string]any{"workspace": delWs, "name": args[0]})
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			fmt.Printf("project deleted: %s/%s\n", delWs, args[0])
			// Remove from guard.
			if projPath != "" {
				guardRemove(projPath)
			}
			return nil
		},
	}
	del.Flags().StringVar(&delWs, "workspace", "", "workspace name")
	del.MarkFlagRequired("workspace")

	c.AddCommand(add)
	c.AddCommand(list)
	c.AddCommand(del)
	return c
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := callDaemon("status", struct{}{})
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			var s daemon.StatusResult
			if err := json.Unmarshal(resp.Result, &s); err != nil {
				return fmt.Errorf("parse status: %w", err)
			}
			fmt.Printf("running: %v\nworkspaces: %d\nprojects: %d\nsessions: %d\nsock: %s\n",
				s.Running, s.Workspaces, s.Projects, s.Sessions, s.Sock)
			return nil
		},
	}
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create global default mouse.yaml (~/.hmf/mouse.yaml)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := config.GlobalMousePath()
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil {
				fmt.Println("global config already exists:", path)
				return nil
			}
			content := `agent:
  primary:
    provider: opencode
    model: default
  secondary:
    provider: ""
    model: ""
permissions:
  fs:
    deny:
      - ".env"
      - "*.key"
  commands:
    deny:
      - "kubectl delete"
      - "kubectl apply"
      - "gcloud * delete"
      - "aws * delete"
    ask:
      - "kubectl scale"
a2a:
  allow_inbound: false
  allow_outbound: true
`
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				return err
			}
			fmt.Println("global config created:", path)
			fmt.Println("\nThis applies to unregistered dirs. Edit it to customize.")
			return nil
		},
	}
}

func configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Manage global default configuration"}
	show := &cobra.Command{
		Use:   "show",
		Short: "Show global default mouse.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := config.GlobalMousePath()
			b, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("no global config. Run 'hmf init' to create one.")
					return nil
				}
				return err
			}
			fmt.Printf("# %s\n%s", path, string(b))
			return nil
		},
	}
	c.AddCommand(show)
	return c
}

func guardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guard",
		Args:  cobra.NoArgs,
		Short: "Update global opencode config to deny direct edits to registered projects",
		Long: `Updates ~/.config/opencode/opencode.json to deny direct edits to all
registered project directories. One command, applies to every opencode session
everywhere. Re-run after registering new projects.

Agents opening opencode in any directory will be blocked from editing files
inside registered projects — they must use engage_project_agent instead.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Query all registered projects from daemon.
			resp, err := callDaemon("project_list", struct{}{})
			if err != nil {
				return fmt.Errorf("daemon not running? run 'hmf up': %w", err)
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			var items []struct {
				Workspace string `json:"workspace"`
				Name      string `json:"name"`
				Path      string `json:"path"`
			}
			if err := json.Unmarshal(resp.Result, &items); err != nil {
				return fmt.Errorf("parse: %w", err)
			}
			if len(items) == 0 {
				fmt.Println("no registered projects — nothing to guard")
				return nil
			}
			// Read existing global opencode.json.
			home, _ := os.UserHomeDir()
			ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
			existing, _ := os.ReadFile(ocPath)
			var cfg map[string]any
			if json.Unmarshal(existing, &cfg) != nil {
				cfg = map[string]any{}
			}
			perm, _ := cfg["permission"].(map[string]any)
			if perm == nil {
				perm = map[string]any{}
			}
			editMap, _ := perm["edit"].(map[string]any)
			if editMap == nil {
				editMap = map[string]any{}
			}
			// Remove old hmf-managed patterns (any key matching a registered project path).
			knownPaths := map[string]bool{}
			for _, it := range items {
				knownPaths[it.Path+"/**"] = true
			}
			for k := range editMap {
				if knownPaths[k] {
					delete(editMap, k)
				}
			}
			// Add current registered project paths.
			for _, it := range items {
				editMap[it.Path+"/**"] = "deny"
			}
			perm["edit"] = editMap
			cfg["permission"] = perm
			b, _ := json.MarshalIndent(cfg, "", "  ")
			if err := os.WriteFile(ocPath, b, 0644); err != nil {
				return err
			}
			fmt.Printf("Updated: %s\n", ocPath)
			fmt.Printf("Guarding %d registered projects:\n", len(items))
			for _, it := range items {
				fmt.Printf("  %s/%s → %s\n", it.Workspace, it.Name, it.Path)
			}
			fmt.Println("\nAgents in any directory cannot edit these paths directly.")
			fmt.Println("They must use engage_project_agent to delegate changes.")
			fmt.Println("Re-run 'hmf guard' after registering new projects.")
			return nil
		},
	}
}

// guardAdd adds a single project path to the global opencode edit deny list.
func guardAdd(projectPath string) error {
	home, _ := os.UserHomeDir()
	ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	cfg := readJSONFile(ocPath)
	perm := getOrCreateMap(cfg, "permission")
	editMap := getOrCreateMap(perm, "edit")
	editMap[projectPath+"/**"] = "deny"
	return writeJSONFile(ocPath, cfg)
}

// guardRemove removes a single project path from the global opencode edit deny list.
func guardRemove(projectPath string) {
	home, _ := os.UserHomeDir()
	ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	cfg := readJSONFile(ocPath)
	perm, _ := cfg["permission"].(map[string]any)
	if perm == nil {
		return
	}
	editMap, _ := perm["edit"].(map[string]any)
	if editMap == nil {
		return
	}
	delete(editMap, projectPath+"/**")
	writeJSONFile(ocPath, cfg)
}

func readJSONFile(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return map[string]any{}
	}
	return cfg
}

func getOrCreateMap(parent map[string]any, key string) map[string]any {
	m, _ := parent[key].(map[string]any)
	if m == nil {
		m = map[string]any{}
		parent[key] = m
	}
	return m
}

func writeJSONFile(path string, cfg map[string]any) error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, b, 0644)
}
