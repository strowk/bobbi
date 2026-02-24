package agent

import (
	"context"
	"fmt"
	"io"
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
// If the context is cancelled, the child process is killed.
func StartAgent(ctx context.Context, agentType AgentType, workDir string, prompt string) error {
	bobbBin, err := os.Executable()
	if err != nil {
		bobbBin = "bobbi"
	}
	// Normalize to forward slashes for valid JSON on Windows
	bobbBin = strings.ReplaceAll(bobbBin, `\`, "/")

	// Ensure .mcp.json points to the right bobbi binary
	mcpJSON := fmt.Sprintf(`{
  "mcpServers": {
    "bobbi": {
      "type": "stdio",
      "command": %q,
      "args": ["mcp", "--agent", %q]
    }
  }
}`, bobbBin, string(agentType))
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

	// Pass prompt via stdin instead of -p argument, because on Windows
	// the claude .cmd batch shim truncates arguments at newlines
	cmd := exec.CommandContext(ctx, "claude",
		"-p", "-",
		"--dangerously-skip-permissions",
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)

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

	// Pipe output through Go rather than inheriting handles directly,
	// which can silently lose output on Windows/MINGW
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	fmt.Printf("[bobbi] Starting %s agent in %s\n", agentType, workDir)
	fmt.Printf("[bobbi] Command: %s\n", strings.Join(cmd.Args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("agent %s start: %w", agentType, err)
	}

	// Copy child output to terminal in background
	done := make(chan struct{})
	go func() {
		io.Copy(os.Stdout, stdoutPipe)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(os.Stderr, stderrPipe)
		done <- struct{}{}
	}()

	err = cmd.Wait()
	<-done
	<-done
	if err != nil {
		return fmt.Errorf("agent %s failed: %w", agentType, err)
	}
	fmt.Printf("[bobbi] Agent %s finished\n", agentType)
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
		case "request_evaluation_change":
			return fmt.Sprintf("Changes to your tests have been requested:\n\n%s\n\nRead the architecture/ directory for the current technical contract. Review and update your tests based on the feedback above. Run the updated tests against the solution deliverable in solution-deliverable/. If all tests pass, use the confirm_solution MCP tool. If tests fail, use the request_solution_change MCP tool with details.", additionalContext)
		}
	case Reviewer:
		switch requestType {
		case "start_reviewer":
			return "Review the code in the solution/ directory for code quality issues. Focus on: correctness, readability, maintainability, and potential bugs. If you find issues that should be fixed, use the request_solution_change MCP tool with specific feedback. If the code quality is acceptable, simply commit a review summary note."
		}
	}

	return fmt.Sprintf("Perform the task: %s\nContext: %s", requestType, additionalContext)
}
