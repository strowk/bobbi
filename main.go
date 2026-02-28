package main

import (
	"fmt"
	"os"

	"bobbi/internal/cmd"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: bobbi <init|up|mcp|feedback|backlog>\n")
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
	case "feedback":
		err = cmd.Feedback(os.Args[2:])
	case "backlog":
		err = cmd.Backlog(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUsage: bobbi <init|up|mcp|feedback|backlog>\n", os.Args[1])
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
