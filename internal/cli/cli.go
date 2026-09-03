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
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/herdanis/his-mouse-friday/internal/config"
	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
	"github.com/spf13/cobra"
)

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
	root.AddCommand(sessionCmd())
	root.AddCommand(taskCmd())
	root.AddCommand(watchCmd())
	root.AddCommand(monitorCmd())
	root.AddCommand(pruneCmd())
	root.AddCommand(progressCmd())
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
			// Background starts discard stderr — mirror stdlib log into the file.
			log.SetOutput(io.MultiWriter(os.Stderr, daemon.LogWriter()))
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

// watchCmd blocks in a terminal (human-run, zero LLM cost) until a task's
// done reply lands, then fires a desktop notification. Alternative to an
// orchestrator polling task_status itself.
func watchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch <message_id>",
		Short: "Block until a delegated task finishes, then fire a desktop notification",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			msgID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid message_id %q: %w", args[0], err)
			}
			fmt.Printf("watching message %d — Ctrl-C to stop\n", msgID)
			for {
				result, err := protocol.CallWithTimeout("task_status",
					map[string]any{"message_id": msgID}, 5*time.Minute+10*time.Second)
				if err != nil {
					return fmt.Errorf("task_status: %w", err)
				}
				var ts struct {
					HasDone     bool   `json:"has_done"`
					AgentStatus string `json:"agent_status"`
				}
				if err := json.Unmarshal(result, &ts); err != nil {
					return fmt.Errorf("decode task_status: %w", err)
				}
				now := time.Now().Format("15:04:05")
				switch {
				case ts.HasDone:
					fmt.Printf("[%s] done (agent_status=%s)\n", now, ts.AgentStatus)
					notify("hmf: task done", fmt.Sprintf("message %d finished (%s)", msgID, ts.AgentStatus))
					return nil
				case ts.AgentStatus == "exited" || ts.AgentStatus == "failed" || ts.AgentStatus == "no_agent":
					fmt.Printf("[%s] ended without a done reply (agent_status=%s)\n", now, ts.AgentStatus)
					notify("hmf: task ended without reply", fmt.Sprintf("message %d — agent_status=%s", msgID, ts.AgentStatus))
					return nil
				default:
					fmt.Printf("[%s] still working...\n", now)
				}
			}
		},
	}
}

// notify fires a best-effort OS desktop notification. No-op if the platform
// isn't supported — this is a convenience, never load-bearing.
func notify(title, message string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := fmt.Sprintf("display notification %q with title %q sound name \"Glass\"", message, title)
	_ = exec.Command("osascript", "-e", script).Run()
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// sessionCmd groups session subcommands (currently just `list`).
// progressCmd is the shell equivalent of the report_progress MCP tool, for
// spawned agents that reach for bash before tools (same rationale as doneCmd).
func progressCmd() *cobra.Command {
	var eta int
	c := &cobra.Command{
		Use:   "progress <note>",
		Short: "Report what you are working on and how much longer you need",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tid := os.Getenv("HMF_TASK_MSG_ID")
			if tid == "" {
				return fmt.Errorf("HMF_TASK_MSG_ID not set; 'hmf progress' is for agents spawned by an hmf wake")
			}
			_, err := protocol.Call("report_progress", map[string]any{
				"thread_id":   atoi64(tid),
				"from":        os.Getenv("HMF_PROJECT"),
				"note":        strings.Join(args, " "),
				"eta_minutes": eta,
			})
			return err
		},
	}
	c.Flags().IntVar(&eta, "eta", 0, "minutes you expect still to need")
	return c
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
	c.AddCommand(list)
	return c
}

// taskCmd groups task subcommands for viewing shared todos bound to threads.
func taskCmd() *cobra.Command {
	c := &cobra.Command{Use: "task", Short: "View shared todos bound to task threads"}
	list := &cobra.Command{
		Use:   "list [thread_id]",
		Short: "List todos (all threads with todos, or one thread)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				result, err := protocol.Call("todo_threads", struct{}{})
				if err != nil {
					return err
				}
				var rows []struct {
					ThreadID int64  `json:"thread_id"`
					Preview  string `json:"preview"`
					Done     int    `json:"done"`
					Total    int    `json:"total"`
				}
				if err := json.Unmarshal(result, &rows); err != nil {
					return fmt.Errorf("parse: %w", err)
				}
				if len(rows) == 0 {
					fmt.Println("(no threads with todos)")
					return nil
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "THREAD\tDONE/TOTAL\tPREVIEW")
				for _, r := range rows {
					fmt.Fprintf(w, "%d\t%d/%d\t%s\n", r.ThreadID, r.Done, r.Total, r.Preview)
				}
				w.Flush()
				return nil
			}
			return printThreadTodos(atoi64(args[0]))
		},
	}
	show := &cobra.Command{
		Use:   "show <thread_id>",
		Short: "Show todos for a thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return printThreadTodos(atoi64(args[0]))
		},
	}
	del := &cobra.Command{
		Use:     "delete <todo_id>...",
		Aliases: []string{"rm"},
		Short:   "Delete work items by id (see `hmf task show <thread_id>`)",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if _, err := protocol.Call("todo_delete", map[string]any{"id": atoi64(a)}); err != nil {
					return fmt.Errorf("todo %s: %w", a, err)
				}
				fmt.Printf("deleted todo %s\n", a)
			}
			return nil
		},
	}
	c.AddCommand(list)
	c.AddCommand(show)
	c.AddCommand(del)
	return c
}

func printThreadTodos(threadID int64) error {
	result, err := protocol.Call("todo_list", map[string]any{"thread_id": threadID})
	if err != nil {
		return err
	}
	var todos []struct {
		ID      int64  `json:"id"`
		Content string `json:"content"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal(result, &todos); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if len(todos) == 0 {
		fmt.Printf("(no todos for thread %d)\n", threadID)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tCONTENT")
	for _, t := range todos {
		fmt.Fprintf(w, "%d\t%s\t%s\n", t.ID, t.State, t.Content)
	}
	w.Flush()
	return nil
}
