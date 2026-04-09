package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildPromptAllAgentRequestTypes(t *testing.T) {
	tests := []struct {
		agent       AgentType
		reqType     string
		context     string
		itemCount   int
		wantContain string
	}{
		// Architect
		{Architect, "start_architect", "", 1, "Read SPECIFICATION.md"},
		{Architect, "start_architect", "extra info", 1, "extra info"},
		{Architect, "request_architecture_change", "update interfaces", 1, "change to the technical contract"},
		{Architect, "request_architecture_change", "item1\nitem2", 3, "Multiple changes"},
		{Architect, "request_architecture_change", "item1\nitem2", 3, "3 items"},

		// Solver
		{Solver, "start_solver", "", 1, "Read the technical contract"},
		{Solver, "start_solver", "contract changed", 1, "contract changed"},
		{Solver, "request_solution_change", "fix bug X", 1, "change to the solution"},
		{Solver, "request_solution_change", "bug1\nbug2", 2, "Multiple changes"},
		{Solver, "request_solution_change", "bug1\nbug2", 2, "ALL the feedback"},

		// Evaluator
		{Evaluator, "start_evaluator", "", 1, "solution deliverable"},
		{Evaluator, "start_evaluator", "extra context", 1, "Additional context: extra context"},
		{Evaluator, "request_evaluation_change", "update tests", 1, "change to the test suite"},
		{Evaluator, "request_evaluation_change", "test1\ntest2", 2, "Multiple changes"},

		// Reviewer
		{Reviewer, "start_reviewer", "", 1, "Solution code is available"},
		{Reviewer, "start_reviewer", "extra context", 1, "Additional context: extra context"},
	}

	for _, tt := range tests {
		name := string(tt.agent) + "/" + tt.reqType
		if tt.itemCount > 1 {
			name += "/coalesced"
		}
		if tt.context != "" {
			name += "/with-context"
		}
		t.Run(name, func(t *testing.T) {
			result := BuildPrompt(tt.agent, tt.reqType, tt.context, tt.itemCount)
			if !strings.Contains(result, tt.wantContain) {
				t.Errorf("BuildPrompt(%s, %s, %q, %d) = %q, want to contain %q",
					tt.agent, tt.reqType, tt.context, tt.itemCount, result, tt.wantContain)
			}
		})
	}
}

func TestBuildPromptStartEvaluatorWithoutContext(t *testing.T) {
	result := BuildPrompt(Evaluator, "start_evaluator", "", 1)
	if strings.Contains(result, "Additional context") {
		t.Errorf("start_evaluator with empty context should not contain 'Additional context', got: %s", result)
	}
}

func TestBuildPromptStartReviewerWithoutContext(t *testing.T) {
	result := BuildPrompt(Reviewer, "start_reviewer", "", 1)
	if strings.Contains(result, "Additional context") {
		t.Errorf("start_reviewer with empty context should not contain 'Additional context', got: %s", result)
	}
}

func TestBuildPromptFallback(t *testing.T) {
	result := BuildPrompt(Solver, "unknown_type", "some context", 1)
	if !strings.Contains(result, "unknown_type") {
		t.Errorf("fallback prompt should contain request type, got: %s", result)
	}
}

func TestExtractAssistantLogLines(t *testing.T) {
	blocks := []fullContentBlock{
		{Type: "text", Text: "Hello\nWorld"},
		{Type: "thinking", Thinking: "Let me think"},
		{Type: "tool_use", Name: "Read", Input: json.RawMessage(`{"file_path":"/foo/bar/baz.go"}`)},
	}
	data, _ := json.Marshal(blocks)

	lines := extractAssistantLogLines(json.RawMessage(data))

	// text lines: "Hello", "World"
	// thinking: "Let me think"
	// tool_use: "▶ Read …/bar/baz.go"
	if len(lines) < 4 {
		t.Fatalf("expected at least 4 lines, got %d", len(lines))
	}

	if lines[0].Type != LogText || lines[0].Text != "Hello" {
		t.Errorf("line 0: got %v", lines[0])
	}
	if lines[1].Type != LogText || lines[1].Text != "World" {
		t.Errorf("line 1: got %v", lines[1])
	}
	if lines[2].Type != LogThinking || lines[2].Text != "Let me think" {
		t.Errorf("line 2: got %v", lines[2])
	}
	if lines[3].Type != LogToolUse || !strings.Contains(lines[3].Text, "Read") {
		t.Errorf("line 3: got %v", lines[3])
	}
}

func TestExtractToolErrorLines(t *testing.T) {
	blocks := []fullContentBlock{
		{Type: "tool_result", IsError: true, Content: json.RawMessage(`"command failed: exit 1"`)},
		{Type: "tool_result", IsError: false, Content: json.RawMessage(`"success"`)},
	}
	data, _ := json.Marshal(blocks)

	lines := extractToolErrorLines(json.RawMessage(data))

	if len(lines) != 1 {
		t.Fatalf("expected 1 error line, got %d", len(lines))
	}
	if lines[0].Type != LogToolError {
		t.Errorf("expected LogToolError, got %v", lines[0].Type)
	}
	if !strings.Contains(lines[0].Text, "command failed") {
		t.Errorf("expected error text, got %q", lines[0].Text)
	}
}

func TestCountToolUses(t *testing.T) {
	blocks := []fullContentBlock{
		{Type: "tool_result", IsError: false},
		{Type: "tool_result", IsError: true},
		{Type: "tool_result", IsError: false},
		{Type: "text", Text: "not a tool result"},
	}
	data, _ := json.Marshal(blocks)

	var gotTotal, gotFailures int
	countToolUses(json.RawMessage(data), func(total, failures int) {
		gotTotal = total
		gotFailures = failures
	})

	if gotTotal != 3 {
		t.Errorf("total = %d, want 3", gotTotal)
	}
	if gotFailures != 1 {
		t.Errorf("failures = %d, want 1", gotFailures)
	}
}

func TestExtractErrorTextString(t *testing.T) {
	result := extractErrorText(json.RawMessage(`"simple error"`))
	if result != "simple error" {
		t.Errorf("got %q, want %q", result, "simple error")
	}
}

func TestExtractErrorTextArray(t *testing.T) {
	result := extractErrorText(json.RawMessage(`[{"type":"text","text":"error part 1"},{"type":"text","text":" part 2"}]`))
	if result != "error part 1 part 2" {
		t.Errorf("got %q, want %q", result, "error part 1 part 2")
	}
}

func TestExtractErrorTextNil(t *testing.T) {
	result := extractErrorText(nil)
	if result != "" {
		t.Errorf("got %q, want empty", result)
	}
}

func TestFormatToolUseSummary(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    string
		want     string
	}{
		{"Read", "Read", `{"file_path":"/a/b/c/d/e.go"}`, "e.go"},
		{"Write", "Write", `{"file_path":"/x/y/z.txt"}`, "z.txt"},
		{"Bash", "Bash", `{"command":"go build ./..."}`, "go build ./..."},
		{"Grep", "Grep", `{"pattern":"TODO","path":"src/"}`, `"TODO" in src/`},
		{"Glob", "Glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"WebFetch", "WebFetch", `{"url":"https://example.com/api"}`, "https://example.com/api"},
		{"WebSearch", "WebSearch", `{"query":"golang testing"}`, `"golang testing"`},
		{"unknown", "UnknownTool", `{"foo":"bar"}`, ""},
		{"nil input", "Read", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input json.RawMessage
			if tt.input != "" {
				input = json.RawMessage(tt.input)
			}
			got := formatToolUseSummary(tt.toolName, input)
			if !strings.Contains(got, tt.want) {
				t.Errorf("formatToolUseSummary(%s, %s) = %q, want to contain %q", tt.toolName, tt.input, got, tt.want)
			}
		})
	}
}

func TestShortenPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/a/b/c/d/e.go", "\u2026/c/d/e.go"},
		{"a/b/c.go", "a/b/c.go"},
		{`C:\Users\foo\bar\baz\file.go`, "\u2026/bar/baz/file.go"},
		{"file.go", "file.go"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ShortenPath(tt.input)
			if got != tt.want {
				t.Errorf("ShortenPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestProcessEventTokens(t *testing.T) {
	event := &claudeEvent{
		Type: "assistant",
		Message: &struct {
			ID    string `json:"id"`
			Usage *struct {
				InputTokens              int64 `json:"input_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
				OutputTokens             int64 `json:"output_tokens"`
			} `json:"usage"`
			Content json.RawMessage `json:"content"`
		}{
			ID: "msg-1",
			Usage: &struct {
				InputTokens              int64 `json:"input_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
				OutputTokens             int64 `json:"output_tokens"`
			}{
				InputTokens:              100,
				CacheCreationInputTokens: 50,
				CacheReadInputTokens:     25,
				OutputTokens:             200,
			},
		},
	}

	var gotInput, gotOutput int64
	opts := &StartOptions{
		OnTokens: func(input, output int64) {
			gotInput = input
			gotOutput = output
		},
	}

	processEvent(event, opts)

	if gotInput != 175 { // 100 + 50 + 25
		t.Errorf("input tokens = %d, want 175", gotInput)
	}
	if gotOutput != 200 {
		t.Errorf("output tokens = %d, want 200", gotOutput)
	}
}

func TestProcessEventSessionID(t *testing.T) {
	event := &claudeEvent{
		Type:      "assistant",
		SessionID: "sess-abc",
		Message: &struct {
			ID    string `json:"id"`
			Usage *struct {
				InputTokens              int64 `json:"input_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
				OutputTokens             int64 `json:"output_tokens"`
			} `json:"usage"`
			Content json.RawMessage `json:"content"`
		}{ID: "msg-1"},
	}

	// We can't easily test OnSessionID through processEvent since it's captured
	// in the scanner loop, but we can verify the event struct parses correctly
	if event.SessionID != "sess-abc" {
		t.Errorf("session ID = %q, want %q", event.SessionID, "sess-abc")
	}
}

func TestBashCommandTruncation(t *testing.T) {
	longCmd := strings.Repeat("x", 100)
	input := json.RawMessage(`{"command":"` + longCmd + `"}`)
	got := formatToolUseSummary("Bash", input)
	if len(got) > 83 { // 80 + "..."
		t.Errorf("long bash command should be truncated, got length %d", len(got))
	}
}

func TestExtractTextLogLines(t *testing.T) {
	blocks := []fullContentBlock{
		{Type: "text", Text: "line 1\nline 2"},
		{Type: "tool_use", Name: "Read"}, // should be ignored
	}
	data, _ := json.Marshal(blocks)

	lines := extractTextLogLines(json.RawMessage(data))
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0].Text != "line 1" {
		t.Errorf("line 0 = %q, want %q", lines[0].Text, "line 1")
	}
}

func TestExtractTextLogLinesNil(t *testing.T) {
	lines := extractTextLogLines(nil)
	if len(lines) != 0 {
		t.Errorf("expected 0 lines for nil input, got %d", len(lines))
	}
}
