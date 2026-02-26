package agent

import (
	"bufio"
	"context"
	"encoding/json"
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

// StartOptions configures agent process I/O and callbacks.
type StartOptions struct {
	// OnTokens is called for each JSONL assistant event with token usage.
	// input = input_tokens + cache_creation_input_tokens + cache_read_input_tokens
	// output = output_tokens
	OnTokens func(input, output int64)
	// StdoutWriter receives all stdout lines. If nil, stdout is discarded.
	StdoutWriter io.Writer
	// StderrWriter receives all stderr lines. If nil, stderr is discarded.
	StderrWriter io.Writer
	// LogFunc is called for log messages. If nil, logs are discarded.
	LogFunc func(format string, args ...interface{})
}

// StartAgent launches a claude process for the given agent type.
// It blocks until the agent finishes or the context is cancelled.
func StartAgent(ctx context.Context, agentType AgentType, workDir string, prompt string, opts *StartOptions) error {
	if opts == nil {
		opts = &StartOptions{}
	}

	bobbiBin, err := os.Executable()
	if err != nil {
		bobbiBin = "bobbi"
	}
	bobbiBin = strings.ReplaceAll(bobbiBin, `\`, "/")

	// Regenerate .mcp.json to point to the correct binary
	if err := os.WriteFile(filepath.Join(workDir, ".mcp.json"), []byte(McpJSON(agentType, bobbiBin)), 0644); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}

	// Regenerate .claude/settings.json with correct absolute paths
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	settingsDir := filepath.Join(workDir, ".claude")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(SettingsJSON(absWorkDir)), 0644); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}

	cmd := exec.CommandContext(ctx, "claude",
		"-p", "-",
		"--dangerously-skip-permissions",
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)

	// Strip env vars so child claude doesn't inherit parent session state
	for _, env := range os.Environ() {
		key := strings.SplitN(env, "=", 2)[0]
		switch key {
		case "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT", "ANTHROPIC_API_KEY":
			continue
		default:
			cmd.Env = append(cmd.Env, env)
		}
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("agent %s start: %w", agentType, err)
	}

	done := make(chan struct{})
	errCh := make(chan error, 1)

	// Process stdout: parse JSONL for tokens, forward to writer
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if opts.OnTokens != nil {
				parseTokenUsage(line, opts.OnTokens)
			}
			if opts.StdoutWriter != nil {
				fmt.Fprintln(opts.StdoutWriter, line)
			}
		}
		if err := scanner.Err(); err != nil {
			if opts.LogFunc != nil {
				opts.LogFunc("stdout scanner error for %s: %v", agentType, err)
			}
			errCh <- err
		}
	}()

	// Process stderr
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if opts.StderrWriter != nil {
			io.Copy(opts.StderrWriter, stderrPipe)
		} else {
			io.Copy(io.Discard, stderrPipe)
		}
	}()

	err = cmd.Wait()
	<-done
	<-stderrDone
	if err != nil {
		return fmt.Errorf("agent %s failed: %w", agentType, err)
	}
	select {
	case scanErr := <-errCh:
		return fmt.Errorf("agent %s stdout scanner: %w", agentType, scanErr)
	default:
		return nil
	}
}

// claudeEvent represents the relevant fields of a JSONL event from claude's stream-json output.
type claudeEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// parseTokenUsage extracts token usage from a JSONL line and calls onTokens if found.
func parseTokenUsage(line string, onTokens func(input, output int64)) {
	var event claudeEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return
	}
	if event.Type != "assistant" || event.Message == nil || event.Message.Usage == nil {
		return
	}
	u := event.Message.Usage
	input := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	output := u.OutputTokens
	if input > 0 || output > 0 {
		onTokens(input, output)
	}
}

// BuildPrompt returns the task prompt for an agent given a request type, context, and item count.
// When itemCount > 1, coalesced prompt variants are used for request_*_change types.
func BuildPrompt(agentType AgentType, requestType string, additionalContext string, itemCount int) string {
	switch agentType {
	case Architect:
		switch requestType {
		case "start_architect":
			if additionalContext != "" {
				return fmt.Sprintf("Read SPECIFICATION.md and create/update the technical contract. %s", additionalContext)
			}
			return "Read SPECIFICATION.md and create/update the technical contract."
		case "request_architecture_change":
			if itemCount > 1 {
				return fmt.Sprintf("Multiple changes to the technical contract have been requested (%d items):\n\n%s\n\nAddress ALL the feedback above and update the contract.", itemCount, additionalContext)
			}
			return fmt.Sprintf("A change to the technical contract has been requested: %s. Review and update the contract as needed.", additionalContext)
		}
	case Solver:
		switch requestType {
		case "start_solver":
			if additionalContext != "" {
				return fmt.Sprintf("The technical contract in architecture/ has been updated. Here is what changed:\n\n%s\n\nReview the changes in the architecture, adjust your existing solution accordingly, rebuild the deliverable, place it in solution-deliverable/, and use the handoff_solution tool.", additionalContext)
			}
			return "Read the technical contract in architecture/ and implement the solution. Build the deliverable and place it in solution-deliverable/, then use the handoff_solution tool."
		case "request_solution_change":
			if itemCount > 1 {
				return fmt.Sprintf("Multiple changes to the solution have been requested (%d items):\n\n%s\n\nAddress ALL the feedback above and resubmit.", itemCount, additionalContext)
			}
			return fmt.Sprintf("A change to the solution has been requested: %s. Address the feedback and resubmit.", additionalContext)
		}
	case Evaluator:
		switch requestType {
		case "start_evaluator":
			return "A solution deliverable is available in solution-deliverable/. The technical contract is in architecture/. Write and run tests against the deliverable. Provide feedback or confirm the solution."
		case "request_evaluation_change":
			if itemCount > 1 {
				return fmt.Sprintf("Multiple changes to the test suite have been requested (%d items):\n\n%s\n\nAddress ALL the feedback above and update the tests.", itemCount, additionalContext)
			}
			return fmt.Sprintf("A change to the test suite has been requested: %s. Update the tests accordingly.", additionalContext)
		}
	case Reviewer:
		switch requestType {
		case "start_reviewer":
			return "Solution code is available in solution/. The technical contract is in architecture/. Review the code for quality and contract compliance, and provide feedback."
		}
	}
	return fmt.Sprintf("Perform the task: %s\nContext: %s", requestType, additionalContext)
}
