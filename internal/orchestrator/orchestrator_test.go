package orchestrator

import (
	"bobbi/internal/agent"
	"testing"
)

func TestMergeLogLines(t *testing.T) {
	existing := []agent.LogLine{
		{Type: agent.LogText, Text: "old text line 1"},
		{Type: agent.LogText, Text: "old text line 2"},
		{Type: agent.LogToolUse, Text: "▶ Read file.go"},
		{Type: agent.LogThinking, Text: "old thinking"},
	}

	incoming := []agent.LogLine{
		{Type: agent.LogText, Text: "new text line"},
	}

	merged := mergeLogLines(existing, incoming)

	// Should keep tool use, remove old text, add new text
	// old thinking should remain (only text is being replaced here)
	hasOldText := false
	hasNewText := false
	hasToolUse := false
	hasThinking := false
	for _, l := range merged {
		if l.Type == agent.LogText && l.Text == "old text line 1" {
			hasOldText = true
		}
		if l.Type == agent.LogText && l.Text == "new text line" {
			hasNewText = true
		}
		if l.Type == agent.LogToolUse {
			hasToolUse = true
		}
		if l.Type == agent.LogThinking {
			hasThinking = true
		}
	}

	if hasOldText {
		t.Error("old text lines should be replaced")
	}
	if !hasNewText {
		t.Error("new text line should be present")
	}
	if !hasToolUse {
		t.Error("tool use should be preserved")
	}
	if !hasThinking {
		t.Error("thinking should be preserved when not replaced")
	}
}

func TestMergeLogLinesThinkingReplacement(t *testing.T) {
	existing := []agent.LogLine{
		{Type: agent.LogThinking, Text: "old thinking 1"},
		{Type: agent.LogThinking, Text: "old thinking 2"},
		{Type: agent.LogToolUse, Text: "▶ Bash ls"},
	}

	incoming := []agent.LogLine{
		{Type: agent.LogThinking, Text: "new thinking"},
	}

	merged := mergeLogLines(existing, incoming)

	thinkingCount := 0
	for _, l := range merged {
		if l.Type == agent.LogThinking {
			thinkingCount++
			if l.Text != "new thinking" {
				t.Errorf("expected new thinking text, got %q", l.Text)
			}
		}
	}
	if thinkingCount != 1 {
		t.Errorf("expected 1 thinking line, got %d", thinkingCount)
	}
}

func TestMergeLogLinesToolUseAccumulates(t *testing.T) {
	existing := []agent.LogLine{
		{Type: agent.LogToolUse, Text: "▶ Read a.go"},
	}

	incoming := []agent.LogLine{
		{Type: agent.LogToolUse, Text: "▶ Write b.go"},
	}

	merged := mergeLogLines(existing, incoming)

	toolCount := 0
	for _, l := range merged {
		if l.Type == agent.LogToolUse {
			toolCount++
		}
	}
	if toolCount != 2 {
		t.Errorf("expected 2 tool use lines, got %d", toolCount)
	}
}

func TestFlattenLogGroups(t *testing.T) {
	groups := []logGroup{
		{messageID: "m1", lines: []agent.LogLine{
			{Type: agent.LogText, Text: "line1"},
		}},
		{messageID: "m2", lines: []agent.LogLine{
			{Type: agent.LogToolUse, Text: "▶ Read"},
			{Type: agent.LogText, Text: "line2"},
		}},
	}

	result := flattenLogGroups(groups)
	if len(result) != 3 {
		t.Errorf("expected 3 lines, got %d", len(result))
	}
}
