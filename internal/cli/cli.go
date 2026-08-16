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
			if err := json.Unmarshal(resp.Result, &s); err != nil {
				return fmt.Errorf("parse status: %w", err)
			}
			fmt.Printf("running: %v\nworkspaces: %d\nprojects: %d\nsessions: %d\nsock: %s\n",
				s.Running, s.Workspaces, s.Projects, s.Sessions, s.Sock)
			return nil
		},
	}
}
