package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/herdanis/his-mouse-friday/internal/daemon"
	"github.com/herdanis/his-mouse-friday/internal/protocol"
	"github.com/spf13/cobra"
)

func defaultStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hmf")
}

func socketPath() string { return filepath.Join(defaultStateDir(), "daemon.sock") }
func dbPath() string     { return filepath.Join(defaultStateDir(), "hmf.db") }

func callDaemon(method string, params any) (protocol.Response, error) {
	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	req := protocol.Request{Method: method, Params: raw, ID: 1}
	conn, err := net.Dial("unix", socketPath())
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
	return root
}

func upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Start the hmf daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			os.MkdirAll(defaultStateDir(), 0755)
			d, err := daemon.NewDaemon(socketPath(), dbPath())
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
			_, err := callDaemon("shutdown", struct{}{})
			return err
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
	c.AddCommand(add)
	return c
}

func projectCmd() *cobra.Command {
	c := &cobra.Command{Use: "project"}
	var ws string
	add := &cobra.Command{
		Use:   "add [name] [path]",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, _ := filepath.Abs(args[1])
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
	c.AddCommand(add)
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
			json.Unmarshal(resp.Result, &s)
			fmt.Printf("running: %v\nworkspaces: %d\nprojects: %d\nsessions: %d\nsock: %s\n",
				s.Running, s.Workspaces, s.Projects, s.Sessions, s.Sock)
			return nil
		},
	}
}
