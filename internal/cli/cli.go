package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
	"github.com/herdanis/his-mouse-friday/internal/tui"
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	// Bare `hmf` opens the orchestrator TUI; cobra only runs RunE when no
	// subcommand matched, so every subcommand keeps its current behavior.
	root := &cobra.Command{
		Use:  "hmf",
		RunE: func(cmd *cobra.Command, args []string) error { return tui.Run() },
	}

	root.AddCommand(upCmd())
	root.AddCommand(downCmd())
	root.AddCommand(workspaceCmd())
	root.AddCommand(projectCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(initCmd())
	root.AddCommand(configCmd())
	root.AddCommand(doneCmd())
	root.AddCommand(sessionCmd())
	root.AddCommand(pruneCmd())
	root.AddCommand(syncCmd())
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
			// Backgrounded: stderr is already redirected into the same file, so
			// writing there too would double every line.
			out := io.Writer(daemon.LogWriter())
			if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
				out = io.MultiWriter(os.Stderr, out)
			}
			log.SetOutput(out)
			log.SetFlags(log.LstdFlags | log.Lmicroseconds)
			fmt.Fprintln(os.Stderr, "log:", daemon.LogPath())
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
			if _, err := protocol.Call("shutdown", struct{}{}); err != nil {
				return err
			}
			return nil
		},
	}
}

func workspaceCmd() *cobra.Command {
	c := &cobra.Command{Use: "workspace"}
	add := &cobra.Command{
		Use:  "add [name]",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := protocol.Call("workspace_add", map[string]any{"name": args[0]}); err != nil {
				return err
			}
			fmt.Println("workspace added:", args[0])
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List all workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := protocol.Call("workspace_list", struct{}{})
			if err != nil {
				return err
			}
			var names []string
			if err := json.Unmarshal(result, &names); err != nil {
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
			if _, err := protocol.Call("workspace_delete", map[string]any{"name": args[0]}); err != nil {
				return err
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
		Use:  "add [name] [path]",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[1])
			if err != nil {
				return fmt.Errorf("resolve path: %w", err)
			}
			if _, err := protocol.Call("project_add", map[string]any{"workspace": ws, "name": args[0], "path": abs}); err != nil {
				return err
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
			result, err := protocol.Call("project_list", params)
			if err != nil {
				return err
			}
			var items []struct {
				Workspace string `json:"workspace"`
				Name      string `json:"name"`
				Path      string `json:"path"`
			}
			if err := json.Unmarshal(result, &items); err != nil {
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
			if _, err := protocol.Call("project_delete", map[string]any{"workspace": delWs, "name": args[0]}); err != nil {
				return err
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
			result, err := protocol.Call("status", struct{}{})
			if err != nil {
				return err
			}
			var s daemon.StatusResult
			if err := json.Unmarshal(result, &s); err != nil {
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
  # secondary:              # fallback when primary unavailable
  #   provider: claude      # NOTE: the permissions below are enforced by the
  #   model: default        # opencode plugin. A claude spawn loads no plugin
  #                         # and enforces none of them.
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
      - "rm -rf /"
      - "rm -rf ~"
      - "git push --force"
      - "git push -f"
      - "git reset --hard"
      - "git clean -fd"
      - "git clean -xfd"
      - "sudo"
      - "chmod -R 777"
      - "dd if="
      - "mkfs"
      - "shutdown"
      - "reboot"
    ask:
      - "kubectl scale"
      - "rm -rf"
      - "git push"
      - "npm publish"
      - "docker system prune"
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

// doneCmd posts a "done" reply. Spawned agents run it as a one-liner instead
// of python/heredocs (which the bash tool wrapper mangles). Env from launcher.
func doneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "done [summary]",
		Short: "Post a done reply to the engaging agent (spawned agents signal completion)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channelStr := os.Getenv("HMF_CHANNEL_ID")
			if channelStr == "" {
				return fmt.Errorf("HMF_CHANNEL_ID not set; 'hmf done' is for agents spawned by an hmf wake (post_message with a `to`)")
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
			if _, err := protocol.Call("post_message", params); err != nil {
				return err
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

func pruneCmd() *cobra.Command {
	var olderThan string
	var yes bool
	c := &cobra.Command{
		Use:   "prune",
		Short: "Delete task history (messages, sessions, todos). Registry is never touched",
		RunE: func(cmd *cobra.Command, args []string) error {
			var hours float64
			scope := "ALL task history"
			if olderThan != "" {
				d, err := time.ParseDuration(olderThan)
				if err != nil {
					return fmt.Errorf("--older-than: %w", err)
				}
				hours = d.Hours()
				scope = "task history older than " + olderThan
			}
			if !yes {
				fmt.Printf("This permanently deletes %s.\n", scope)
				fmt.Println("Workspaces and projects are kept; running tasks are skipped.")
				fmt.Print("Type 'yes' to continue: ")
				var answer string
				fmt.Scanln(&answer)
				if answer != "yes" {
					fmt.Println("aborted")
					return nil
				}
			}
			result, err := protocol.Call("prune", map[string]any{"older_than_hours": hours})
			if err != nil {
				return err
			}
			var res daemon.PruneResult
			if err := json.Unmarshal(result, &res); err != nil {
				return fmt.Errorf("parse: %w", err)
			}
			fmt.Printf("pruned %d messages, %d sessions, %d todos\n", res.Messages, res.Sessions, res.Todos)
			if res.Skipped > 0 {
				fmt.Printf("kept %d thread(s) still running or newer than the cutoff\n", res.Skipped)
			}
			return nil
		},
	}
	c.Flags().StringVar(&olderThan, "older-than", "", "only prune history older than this (e.g. 24h, 168h). Default: everything")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func sessionCmd() *cobra.Command {
	c := &cobra.Command{Use: "session", Short: "Manage hmf sessions"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List all hmf sessions (name, project, status, opencode session id, root)",
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := protocol.Call("session_list", struct{}{})
			if err != nil {
				return err
			}
			var items []daemon.SessionListItem
			if err := json.Unmarshal(result, &items); err != nil {
				return fmt.Errorf("parse: %w", err)
			}
			if len(items) == 0 {
				fmt.Println("(no sessions)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPROJECT\tSTATUS\tSESSION\tPARENT\tCREATED")
			for _, it := range items {
				oc := it.AgentSessionID
				if oc == "" {
					oc = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", it.Name, it.Project, it.Status, oc, it.ParentID, it.CreatedAt)
			}
			w.Flush()
			return nil
		},
	}
	var printOnly bool
	attach := &cobra.Command{
		Use:   "attach <id>",
		Short: "Reopen a session interactively (id = session name or opencode session id, prefix ok)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := protocol.Call("session_list", struct{}{})
			if err != nil {
				return err
			}
			var items []daemon.SessionListItem
			if err := json.Unmarshal(result, &items); err != nil {
				return fmt.Errorf("parse: %w", err)
			}
			it, err := resolveSessionForAttach(items, args[0])
			if err != nil {
				return err
			}
			if printOnly {
				fmt.Printf("cd %s && opencode -s %s\n", it.Dir, it.AgentSessionID)
				return nil
			}
			oc := exec.Command("opencode", "-s", it.AgentSessionID)
			oc.Dir = it.Dir
			oc.Stdin, oc.Stdout, oc.Stderr = os.Stdin, os.Stdout, os.Stderr
			return oc.Run()
		},
	}
	attach.Flags().BoolVar(&printOnly, "print", false, "print the attach command instead of running it")
	c.AddCommand(list, attach)
	return c
}

// resolveSessionForAttach picks the session <id> names: exact match on the hmf
// name or the opencode session id first, then a unique prefix of either.
func resolveSessionForAttach(items []daemon.SessionListItem, id string) (daemon.SessionListItem, error) {
	var pre []daemon.SessionListItem
	for _, it := range items {
		if it.Name == id || (it.AgentSessionID != "" && it.AgentSessionID == id) {
			return attachable(it)
		}
		if strings.HasPrefix(it.Name, id) || (it.AgentSessionID != "" && strings.HasPrefix(it.AgentSessionID, id)) {
			pre = append(pre, it)
		}
	}
	// A resumed task leaves one row per run, all sharing the opencode session
	// id — same conversation, not an ambiguous match.
	var cands []string
	seen := map[string]bool{}
	uniq := pre[:0]
	for _, it := range pre {
		key := it.Name + "\x00" + it.AgentSessionID + "\x00" + it.Dir
		if seen[key] {
			continue
		}
		seen[key] = true
		uniq = append(uniq, it)
	}
	switch len(uniq) {
	case 1:
		return attachable(uniq[0])
	case 0:
		return daemon.SessionListItem{}, fmt.Errorf("no session matches %q; run 'hmf session list'", id)
	}
	for _, it := range uniq {
		cands = append(cands, fmt.Sprintf("%s (%s)", it.Name, it.AgentSessionID))
	}
	return daemon.SessionListItem{}, fmt.Errorf("%q is ambiguous, matches: %s", id, strings.Join(cands, ", "))
}

func attachable(it daemon.SessionListItem) (daemon.SessionListItem, error) {
	if it.AgentSessionID == "" {
		return daemon.SessionListItem{}, fmt.Errorf("session %q never captured an opencode session id, cannot attach; run 'hmf session list'", it.Name)
	}
	return it, nil
}
