// Command hmf-mcp is the per-session MCP shim (stdio) forwarding to the daemon.
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
