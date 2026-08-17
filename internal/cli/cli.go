package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	root.AddCommand(doneCmd())
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
			resp, err := callDaemon("project_delete", map[string]any{"workspace": delWs, "name": args[0]})
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			fmt.Printf("project deleted: %s/%s\n", delWs, args[0])
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

// doneCmd posts a "done" reply to the engaging agent. Spawned agents run this
// as a one-line shell command instead of writing python/heredocs to hit the
// daemon socket (which the bash tool wrapper mangles). Env (set by the
// launcher): HMF_CHANNEL_ID, HMF_TASK_MSG_ID, HMF_PROJECT, HMF_FROM.
func doneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "done [summary]",
		Short: "Post a done reply to the engaging agent (spawned agents signal completion)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channelStr := os.Getenv("HMF_CHANNEL_ID")
			if channelStr == "" {
				return fmt.Errorf("HMF_CHANNEL_ID not set; 'hmf done' is for agents spawned by 'hmf engage'")
			}
			params := map[string]any{
				"channel": atoi64(channelStr),
				"from":    os.Getenv("HMF_PROJECT"),
				"to":      os.Getenv("HMF_FROM"),
				"content": strings.Join(args, " "),
				"status":  "done",
			}
			if tid := os.Getenv("HMF_TASK_MSG_ID"); tid != "" {
				params["thread_id"] = atoi64(tid)
			}
			resp, err := callDaemon("post_message", params)
			if err != nil {
				return err
			}
			if resp.Error != nil {
				return fmt.Errorf("%s", resp.Error.Message)
			}
			fmt.Println("done")
			return nil
		},
	}
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// Protection model: global opencode.json has "edit": "ask" (prompts for
// every edit outside a registered project). Each registered project's own
// opencode.json sets "edit": "allow" (free editing inside own repo).
// No hmf-specific guard code needed — opencode's native config layering
// enforces it. /hmf-setup generates the per-repo opencode.json with
// "edit": "allow".
