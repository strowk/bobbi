package cmd

import (
	"fmt"
	"os"
	"strings"

	mcpserver "bobbi/internal/mcp"
)

func MCP(args []string) error {
	var agentType string
	for i, arg := range args {
		if arg == "--agent" && i+1 < len(args) {
			agentType = args[i+1]
			break
		}
		if strings.HasPrefix(arg, "--agent=") {
			agentType = strings.TrimPrefix(arg, "--agent=")
			break
		}
	}

	if agentType == "" {
		return fmt.Errorf("usage: bobbi mcp --agent <solver|evaluator|architect|reviewer>")
	}

	switch agentType {
	case "solver", "evaluator", "architect", "reviewer":
		// valid
	default:
		return fmt.Errorf("unknown agent type: %s", agentType)
	}

	return mcpserver.Serve(agentType, os.Stdin, os.Stdout)
}
