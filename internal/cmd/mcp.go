package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bobbi/internal/agent"
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

	queuesDir, err := findQueuesDir()
	if err != nil {
		return fmt.Errorf("resolve queues directory: %w", err)
	}

	return mcpserver.Serve(agent.AgentType(agentType), queuesDir, os.Stdin, os.Stdout)
}

// findQueuesDir searches upward from the current working directory for a
// .bobbi/ directory and returns the path to its queues/ subdirectory.
func findQueuesDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	dir := cwd
	for {
		candidate := filepath.Join(dir, ".bobbi", "queues")
		if info, err := os.Stat(filepath.Join(dir, ".bobbi")); err == nil && info.IsDir() {
			return candidate, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("could not find .bobbi/ directory in any parent of %s", cwd)
}
