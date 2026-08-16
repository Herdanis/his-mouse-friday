// Command hmf-mcp is the per-session MCP server shim. It is spawned by opencode
// over stdio and forwards tool calls to the hmf daemon.
package main

import (
	"context"
	"log"

	"github.com/herdanis/his-mouse-friday/internal/mcp"
)

func main() {
	if err := mcp.RunServer(context.Background()); err != nil {
		log.Fatal(err)
	}
}
