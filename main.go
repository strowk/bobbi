package main

import (
	"fmt"
	"os"

	"bob/internal/cmd"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: bob <init|up|mcp>\n")
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "init":
		err = cmd.Init()
	case "up":
		err = cmd.Up(os.Args[2:])
	case "mcp":
		err = cmd.MCP(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUsage: bob <init|up|mcp>\n", os.Args[1])
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
