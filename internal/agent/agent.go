package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type AgentType string

const (
	Solver    AgentType = "solver"
	Evaluator AgentType = "evaluator"
	Architect AgentType = "architect"
	Reviewer  AgentType = "reviewer"
)

func AllTypes() []AgentType {
	return []AgentType{Solver, Evaluator, Architect, Reviewer}
}

func RepoDir(agentType AgentType) string {
	switch agentType {
	case Solver:
		return "solution"
	case Evaluator:
		return "evaluation"
	case Architect:
		return "architecture"
	case Reviewer:
		return "review"
	}
	return string(agentType)
}

// StartAgent launches a claude process for the given agent type in the given working directory.
// The prompt describes the task to perform. It blocks until the agent finishes.
func StartAgent(agentType AgentType, workDir string, prompt string) error {
	bobBin, err := os.Executable()
	if err != nil {
		bobBin = "bob"
	}

	// Ensure .mcp.json points to the right bob binary
	mcpJSON := fmt.Sprintf(`{
  "mcpServers": {
    "bob": {
      "type": "stdio",
      "command": %q,
      "args": ["mcp", "--agent", %q]
    }
  }
}`, bobBin, string(agentType))
	mcpPath := filepath.Join(workDir, ".mcp.json")
	if err := os.WriteFile(mcpPath, []byte(mcpJSON), 0644); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}

	// Write scoped settings.json with permissions restricted to agent's workDir
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	settingsDir := filepath.Join(workDir, ".claude")
	os.MkdirAll(settingsDir, 0755)
	settingsPath := filepath.Join(settingsDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(SettingsJSON(absWorkDir)), 0644); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}

	cmd := exec.Command("claude",
		"-p", prompt,
		"--dangerously-skip-permissions",
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Dir = workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Strip env vars so child claude uses subscription auth (not API key)
	// and doesn't think it's inside an existing session
	for _, env := range os.Environ() {
		key := strings.SplitN(env, "=", 2)[0]
		switch key {
		case "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT",
			"ANTHROPIC_API_KEY":
			continue
		default:
			cmd.Env = append(cmd.Env, env)
		}
	}

	fmt.Printf("[bob] Starting %s agent in %s\n", agentType, workDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("agent %s failed: %w", agentType, err)
	}
	fmt.Printf("[bob] Agent %s finished\n", agentType)
	return nil
}

// BuildPrompt returns the task prompt for an agent given a request type and context.
func BuildPrompt(agentType AgentType, requestType string, additionalContext string) string {
	switch agentType {
	case Architect:
		switch requestType {
		case "start_architect":
			if additionalContext != "" {
				return fmt.Sprintf("The user has provided the following instructions:\n\n%s\n\nRead the SPECIFICATION.md file in your repository. Based on the specification and the user's instructions above, update the technical contract that describes what the solution should implement and how the evaluator should test it. Update any necessary contract files. When done, commit your changes with a descriptive message.", additionalContext)
			}
			return "Read the SPECIFICATION.md file in your repository. Based on it, create a detailed technical contract that describes what the solution should implement and how the evaluator should test it. Create any necessary contract files. When done, commit your changes with a descriptive message."
		case "request_architecture_change":
			return fmt.Sprintf("A change to the architecture has been requested:\n\n%s\n\nReview and update the technical contract and any related files accordingly. When done, commit your changes with a descriptive message.", additionalContext)
		}
	case Solver:
		switch requestType {
		case "start_solver":
			return "Read the architecture/ directory to understand the technical contract. Implement the solution according to the specification. Build/compile your solution and place the deliverable output in the solution-deliverable/ directory. When done, commit your source changes with a descriptive message, then use the handoff_solution MCP tool to submit your work for evaluation."
		case "request_solution_change":
			return fmt.Sprintf("Changes to your solution have been requested:\n\n%s\n\nRead the architecture/ directory for the current technical contract. Make the requested changes, rebuild, and place the updated deliverable in solution-deliverable/. Commit your changes, then use the handoff_solution MCP tool to submit your work for evaluation.", additionalContext)
		}
	case Evaluator:
		switch requestType {
		case "start_evaluator":
			return "Read the architecture/ directory to understand the technical contract. The solution deliverable is available in solution-deliverable/. Write tests that verify the solution meets the specification, then run them. If all tests pass, use the confirm_solution MCP tool. If tests fail, use the request_solution_change MCP tool with a description of what needs to be fixed."
		}
	case Reviewer:
		switch requestType {
		case "start_reviewer":
			return "Review the code in the solution/ directory for code quality issues. Focus on: correctness, readability, maintainability, and potential bugs. If you find issues that should be fixed, use the request_solution_change MCP tool with specific feedback. If the code quality is acceptable, simply commit a review summary note."
		}
	}

	return fmt.Sprintf("Perform the task: %s\nContext: %s", requestType, additionalContext)
}
