// Package mcp runs the hmf-mcp shim: a stateless stdio MCP server that exposes
// 5 orchestration tools. Each tool call is forwarded to the hmf daemon over a
// unix socket and the daemon's response is relayed back to the caller.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/herdanis/his-mouse-friday/internal/protocol"
	mcpserver "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ============================================
// Tool input / output types
// ============================================

// jsonschema tags carry ONLY a bare description string: jsonschema-go v0.4.3
// (transitively pulled by go-sdk v1.7.0) treats the whole tag value as the
// property description and rejects any value matching ^[^ \t\n]*= (it would
// make AddTool panic at startup). "required,description=..." matches that
// forbidden prefix, so it cannot be used. Required-ness is instead derived
// from the json tag: a field is required unless it carries omitempty.

type EngageInput struct {
	Project string `json:"project" jsonschema:"workspace/project to engage"`
	Task    string `json:"task" jsonschema:"task description"`
}
type EngageOutput struct {
	SessionID int64 `json:"session_id"`
	ChannelID int64 `json:"channel_id"`
}
type PostInput struct {
	Channel  int64  `json:"channel" jsonschema:"channel id"`
	ThreadID int64  `json:"thread_id,omitempty" jsonschema:"thread id for replies"`
	Content  string `json:"content" jsonschema:"message content"`
}
type ReadChanInput struct {
	Channel int64 `json:"channel" jsonschema:"channel id"`
}
type ReadThreadInput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"thread id"`
}
type ListInput struct {
	Workspace string `json:"workspace,omitempty" jsonschema:"workspace name filter"`
}

// ============================================
// Daemon client (unix socket)
// ============================================

// callDaemon dials the daemon, sends a single JSON-RPC request, and returns the
// Result field of the response. It is one-request/one-response per connection:
// the daemon's serveConn loops on a shared decoder, so a fresh connection per
// call keeps framing simple and avoids carrying buffered state across calls.
func callDaemon(ctx context.Context, method string, params any) (json.RawMessage, error) {
	conn, err := net.Dial("unix", protocol.SocketPath())
	if err != nil {
		return nil, fmt.Errorf("daemon not running (run 'hmf up'): %w", err)
	}
	defer conn.Close()

	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	req := protocol.Request{Method: method, Params: raw, ID: 1}

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(&req); err != nil {
		return nil, err
	}
	var resp protocol.Response
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}
	return resp.Result, nil
}

// ============================================
// MCP server
// ============================================

// newServer builds the MCP server and registers the 5 orchestration tools.
// Separated from RunServer so tests can verify registration without blocking
// on stdio. Each tool is backed by a callDaemon forward to the hmf daemon.
func newServer() *mcpserver.Server {
	srv := mcpserver.NewServer(&mcpserver.Implementation{Name: "hmf-mcp", Version: "v0.1.0"}, nil)

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "engage_project_agent",
		Description: "Spawn/resume target project agent and return session + channel id",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in EngageInput) (*mcpserver.CallToolResult, EngageOutput, error) {
		result, err := callDaemon(ctx, "engage_project_agent", in)
		if err != nil {
			return nil, EngageOutput{}, err
		}
		var out EngageOutput
		if err := json.Unmarshal(result, &out); err != nil {
			return nil, EngageOutput{}, fmt.Errorf("decode engage result: %w", err)
		}
		return nil, out, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "post_message",
		Description: "Post a message to a channel or thread",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in PostInput) (*mcpserver.CallToolResult, struct{}, error) {
		if _, err := callDaemon(ctx, "post_message", in); err != nil {
			return nil, struct{}{}, err
		}
		return nil, struct{}{}, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "read_channel",
		Description: "Read messages in a channel",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ReadChanInput) (*mcpserver.CallToolResult, json.RawMessage, error) {
		result, err := callDaemon(ctx, "read_channel", in)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "read_thread",
		Description: "Read a message thread",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ReadThreadInput) (*mcpserver.CallToolResult, json.RawMessage, error) {
		result, err := callDaemon(ctx, "read_thread", in)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	mcpserver.AddTool(srv, &mcpserver.Tool{
		Name:        "list_project_agents",
		Description: "List registered project agents",
	}, func(ctx context.Context, req *mcpserver.CallToolRequest, in ListInput) (*mcpserver.CallToolResult, json.RawMessage, error) {
		result, err := callDaemon(ctx, "list_project_agents", in)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	return srv
}

// RunServer starts the hmf-mcp server over stdio and blocks until the context
// is cancelled or the transport closes.
func RunServer(ctx context.Context) error {
	return newServer().Run(ctx, &mcpserver.StdioTransport{})
}
