package main

import (
	"os"

	"github.com/herdanis/his-mouse-friday/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
