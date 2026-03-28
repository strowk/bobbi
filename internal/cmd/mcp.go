package cmd

import (
	"fmt"
	"os"
	"strings"

	"bobbi/internal/agent"
	mcpserver "bobbi/internal/mcp"
)

func MCP(args []string) error {
	var agentType string
	var queuesDir string
	for i, arg := range args {
		if arg == "--agent" && i+1 < len(args) {
			agentType = args[i+1]
		} else if strings.HasPrefix(arg, "--agent=") {
			agentType = strings.TrimPrefix(arg, "--agent=")
		} else if arg == "--queues-dir" && i+1 < len(args) {
			queuesDir = args[i+1]
		} else if strings.HasPrefix(arg, "--queues-dir=") {
			queuesDir = strings.TrimPrefix(arg, "--queues-dir=")
		}
	}

	if agentType == "" {
		return fmt.Errorf("usage: bobbi mcp --agent <solver|evaluator|architect|reviewer> --queues-dir <path>")
	}

	switch agentType {
	case "solver", "evaluator", "architect", "reviewer":
		// valid
	default:
		return fmt.Errorf("unknown agent type: %s", agentType)
	}

	if queuesDir == "" {
		return fmt.Errorf("--queues-dir is required: bobbi mcp --agent <type> --queues-dir <path>")
	}

	return mcpserver.Serve(agent.AgentType(agentType), queuesDir, os.Stdin, os.Stdout)
}
