package agent

import (
	"bufio"
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

// LogLineType categorizes log lines for display styling.
type LogLineType int

const (
	LogText      LogLineType = iota // Regular assistant text
	LogToolUse                      // Tool call: ▶ ToolName summary
	LogToolError                    // Tool error:   ✗ error message
	LogThinking                     // Thinking block content
)

// LogLine represents a single line of extracted log content.
type LogLine struct {
	Type LogLineType
	Text string
}

// StartOptions configures agent process I/O and callbacks.
type StartOptions struct {
	// BaseDir is the BOBBI project root directory (parent of agent repo dirs).
	// Used to write MCP config files to .bobbi/mcp-config-<agent-type>.json.
	BaseDir string
	// OnTokens is called for each JSONL assistant event with token usage.
	// input = input_tokens + cache_creation_input_tokens + cache_read_input_tokens
	// output = output_tokens
	OnTokens func(input, output int64)
	// OnToolUse is called for each tool_result block found in user events.
	// total is the number of tool_result blocks in the event, failures is the count with is_error=true.
	OnToolUse func(total, failures int)
	// OnLogEntry is called with extracted log content from JSONL events.
	// messageID is non-empty for assistant events (for deduplication); empty for user/result events.
	OnLogEntry func(messageID string, lines []LogLine)
	// OnSessionID is called once with the sessionId from the first JSONL event that contains one.
	OnSessionID func(sessionID string)
	// StdoutWriter receives all stdout lines. If nil, stdout is discarded.
	StdoutWriter io.Writer
	// StderrWriter receives all stderr lines. If nil, stderr is discarded.
	StderrWriter io.Writer
	// LogFunc is called for log messages. If nil, logs are discarded.
	LogFunc func(format string, args ...interface{})
}

// StartAgent launches a claude process for the given agent type.
// It blocks until the agent process exits. The process is never killed;
// it always runs to completion (per contract: agents finish naturally).
func StartAgent(agentType AgentType, workDir string, prompt string, opts *StartOptions) error {
	if opts == nil {
		opts = &StartOptions{}
	}

	bobbiBin, err := os.Executable()
	if err != nil {
		bobbiBin = "bobbi"
	}
	bobbiBin = strings.ReplaceAll(bobbiBin, `\`, "/")

	// Resolve absolute path to queues directory for MCP config
	queuesDir := filepath.Join(opts.BaseDir, ".bobbi", "queues")
	absQueuesDir, err := filepath.Abs(queuesDir)
	if err != nil {
		return fmt.Errorf("resolve queues dir: %w", err)
	}

	// Write MCP config to .bobbi/mcp-config-<agent-type>.json at the project root
	mcpConfigContent := McpJSON(agentType, bobbiBin, absQueuesDir)
	mcpConfigDir := filepath.Join(opts.BaseDir, ".bobbi")
	if err := os.MkdirAll(mcpConfigDir, 0755); err != nil {
		return fmt.Errorf("create .bobbi dir for mcp config: %w", err)
	}
	mcpConfigPath := filepath.Join(mcpConfigDir, fmt.Sprintf("mcp-config-%s.json", agentType))
	if err := os.WriteFile(mcpConfigPath, []byte(mcpConfigContent), 0644); err != nil {
		return fmt.Errorf("write mcp config file: %w", err)
	}
	absMcpConfigPath, err := filepath.Abs(mcpConfigPath)
	if err != nil {
		return fmt.Errorf("resolve mcp config path: %w", err)
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--mcp-config", absMcpConfigPath,
		"--allowedTools", AllowedTools(),
	}

	// Log the command for debugging
	if opts.LogFunc != nil {
		opts.LogFunc("Starting agent %s with command: claude %s", agentType, strings.Join(args, " "))
		opts.LogFunc("Working directory: %s", workDir)
		opts.LogFunc("MCP config path: %s", absMcpConfigPath)
	}

	cmd := exec.Command("claude",
		args...,
	)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)

	// Strip env vars so child claude doesn't inherit parent session state.
	// ANTHROPIC_API_KEY is intentionally excluded: the claude CLI uses its
	// own credential store, and passing the parent key could cause child
	// agents to bypass the CLI's auth flow or hit the wrong account.
	cmd.Env = []string{}
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

	// Process stdout: parse JSONL, forward to writer
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		sessionIDCaptured := false
		for scanner.Scan() {
			line := scanner.Text()

			var event claudeEvent
			if json.Unmarshal([]byte(line), &event) == nil {
				// Session ID capture: check both sessionId (camelCase) and session_id (snake_case)
				sid := event.SessionID
				if sid == "" {
					sid = event.SessionIDSnake
				}
				if opts.OnSessionID != nil && !sessionIDCaptured && sid != "" {
					sessionIDCaptured = true
					opts.OnSessionID(sid)
				}
				// Process event for tokens, tool use, and log content
				processEvent(&event, opts)
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
	Type           string `json:"type"`
	SessionID      string `json:"sessionId"`
	SessionIDSnake string `json:"session_id"`
	Message        *struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Result *struct {
		Content json.RawMessage `json:"content"`
	} `json:"result"`
}

// fullContentBlock represents any content block type found in message.content arrays.
type fullContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`       // text blocks
	Thinking  string          `json:"thinking"`   // thinking blocks
	Name      string          `json:"name"`       // tool_use blocks
	BlockID   string          `json:"id"`         // tool_use blocks
	Input     json.RawMessage `json:"input"`      // tool_use blocks
	IsError   bool            `json:"is_error"`   // tool_result blocks
	Content   json.RawMessage `json:"content"`    // tool_result blocks
	ToolUseID string          `json:"tool_use_id"` // tool_result blocks
}

// processEvent dispatches a parsed JSONL event to appropriate callbacks.
func processEvent(event *claudeEvent, opts *StartOptions) {
	switch event.Type {
	case "assistant":
		if event.Message == nil {
			return
		}
		// Token usage
		if opts.OnTokens != nil && event.Message.Usage != nil {
			u := event.Message.Usage
			input := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
			output := u.OutputTokens
			opts.OnTokens(input, output)
		}
		// Log content extraction (text, tool_use, thinking)
		if opts.OnLogEntry != nil && event.Message.Content != nil {
			lines := extractAssistantLogLines(event.Message.Content)
			if len(lines) > 0 {
				messageID := ""
				if event.Message.ID != "" {
					messageID = event.Message.ID
				}
				opts.OnLogEntry(messageID, lines)
			}
		}

	case "user":
		if event.Message == nil || event.Message.Content == nil {
			return
		}
		// Tool use counting
		if opts.OnToolUse != nil {
			countToolUses(event.Message.Content, opts.OnToolUse)
		}
		// Tool error extraction
		if opts.OnLogEntry != nil {
			errorLines := extractToolErrorLines(event.Message.Content)
			if len(errorLines) > 0 {
				opts.OnLogEntry("", errorLines)
			}
		}

	case "result":
		if opts.OnLogEntry != nil && event.Result != nil && event.Result.Content != nil {
			lines := extractTextLogLines(event.Result.Content)
			if len(lines) > 0 {
				opts.OnLogEntry("", lines)
			}
		}
	}
}

// extractAssistantLogLines extracts log lines from assistant message content blocks.
func extractAssistantLogLines(content json.RawMessage) []LogLine {
	var blocks []fullContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var lines []LogLine
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				for _, l := range strings.Split(b.Text, "\n") {
					lines = append(lines, LogLine{Type: LogText, Text: l})
				}
			}
		case "thinking":
			if b.Thinking != "" {
				for _, l := range strings.Split(b.Thinking, "\n") {
					lines = append(lines, LogLine{Type: LogThinking, Text: l})
				}
			}
		case "tool_use":
			summary := formatToolUseSummary(b.Name, b.Input)
			text := "\u25b6 " + b.Name
			if summary != "" {
				text += " " + summary
			}
			lines = append(lines, LogLine{Type: LogToolUse, Text: text})
		}
	}
	return lines
}

// extractToolErrorLines extracts error lines from user event tool_result blocks.
func extractToolErrorLines(content json.RawMessage) []LogLine {
	if content == nil {
		return nil
	}
	var blocks []fullContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var lines []LogLine
	for _, b := range blocks {
		if b.Type == "tool_result" && b.IsError {
			errorText := extractErrorText(b.Content)
			firstLine := strings.SplitN(errorText, "\n", 2)[0]
			if len(firstLine) > 120 {
				firstLine = firstLine[:117] + "..."
			}
			lines = append(lines, LogLine{Type: LogToolError, Text: "  \u2717 " + firstLine})
		}
	}
	return lines
}

// extractTextLogLines extracts text-only log lines from a content array (for result events).
func extractTextLogLines(content json.RawMessage) []LogLine {
	if content == nil {
		return nil
	}
	var blocks []fullContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var lines []LogLine
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			for _, l := range strings.Split(b.Text, "\n") {
				lines = append(lines, LogLine{Type: LogText, Text: l})
			}
		}
	}
	return lines
}

// countToolUses counts tool_result blocks in a user event content array.
func countToolUses(content json.RawMessage, onToolUse func(total, failures int)) {
	var blocks []fullContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return
	}
	total := 0
	failures := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			total++
			if b.IsError {
				failures++
			}
		}
	}
	if total > 0 {
		onToolUse(total, failures)
	}
}

// extractErrorText extracts text from a tool_result content field (string or array of text blocks).
func extractErrorText(content json.RawMessage) string {
	if content == nil {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	// Try array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		var texts []string
		for _, b := range blocks {
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "")
	}
	return ""
}

// formatToolUseSummary creates a compact input summary for a tool_use block.
func formatToolUseSummary(name string, input json.RawMessage) string {
	if input == nil {
		return ""
	}
	var inputMap map[string]json.RawMessage
	if err := json.Unmarshal(input, &inputMap); err != nil {
		return ""
	}

	switch name {
	case "Read", "Write", "Edit":
		if fp, ok := inputMap["file_path"]; ok {
			var path string
			if json.Unmarshal(fp, &path) == nil {
				return ShortenPath(path)
			}
		}
	case "Bash":
		if cmd, ok := inputMap["command"]; ok {
			var command string
			if json.Unmarshal(cmd, &command) == nil {
				// Replace newlines with spaces to keep tool use summary single-line
				command = strings.ReplaceAll(command, "\n", " ")
				if len(command) > 80 {
					command = command[:77] + "..."
				}
				return command
			}
		}
	case "Grep":
		var result string
		if pat, ok := inputMap["pattern"]; ok {
			var pattern string
			if json.Unmarshal(pat, &pattern) == nil {
				result = fmt.Sprintf(`"%s"`, pattern)
			}
		}
		if p, ok := inputMap["path"]; ok {
			var path string
			if json.Unmarshal(p, &path) == nil {
				result += " in " + path
			}
		}
		return result
	case "Glob":
		if pat, ok := inputMap["pattern"]; ok {
			var pattern string
			if json.Unmarshal(pat, &pattern) == nil {
				return pattern
			}
		}
	case "Task":
		var result string
		if desc, ok := inputMap["description"]; ok {
			var description string
			if json.Unmarshal(desc, &description) == nil {
				result = fmt.Sprintf(`"%s"`, description)
			}
		}
		if st, ok := inputMap["subagent_type"]; ok {
			var subagentType string
			if json.Unmarshal(st, &subagentType) == nil {
				result += fmt.Sprintf(" (%s)", subagentType)
			}
		}
		return result
	case "WebFetch":
		if u, ok := inputMap["url"]; ok {
			var url string
			if json.Unmarshal(u, &url) == nil {
				if len(url) > 60 {
					url = url[:57] + "..."
				}
				return url
			}
		}
	case "WebSearch":
		if q, ok := inputMap["query"]; ok {
			var query string
			if json.Unmarshal(q, &query) == nil {
				if len(query) > 60 {
					query = query[:57] + "..."
				}
				return fmt.Sprintf(`"%s"`, query)
			}
		}
	}
	return ""
}

// ShortenPath shortens a file path to show only the last 3 segments with …/ prefix.
func ShortenPath(path string) string {
	// Normalize to forward slashes
	path = strings.ReplaceAll(path, `\`, "/")
	segments := strings.Split(path, "/")
	// Remove empty segments
	var clean []string
	for _, s := range segments {
		if s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) > 3 {
		return "\u2026/" + strings.Join(clean[len(clean)-3:], "/")
	}
	return path
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
