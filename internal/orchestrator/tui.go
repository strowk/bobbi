package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"bobbi/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
)

// tickMsg triggers periodic TUI refresh.
type tickMsg time.Time

// orchestratorDoneMsg signals the TUI that the orchestrator has finished.
type orchestratorDoneMsg struct{}

// TUIModel is the BubbleTea model for the BOBBI terminal UI.
type TUIModel struct {
	orch     *Orchestrator
	cancel   func() // cancel the orchestrator context
	width    int
	height   int
	quitting bool
}

// NewTUIModel creates a new TUI model.
func NewTUIModel(orch *Orchestrator, cancel func()) TUIModel {
	return TUIModel{
		orch:   orch,
		cancel: cancel,
		width:  80,
		height: 24,
	}
}

func (m TUIModel) Init() tea.Cmd {
	return tickCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m TUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			m.cancel()
			return m, tea.Quit
		}

	case tickMsg:
		// Check if orchestrator is done
		select {
		case <-m.orch.Done():
			m.quitting = true
			return m, tea.Quit
		default:
		}
		return m, tickCmd()

	case orchestratorDoneMsg:
		m.quitting = true
		return m, tea.Quit
	}

	return m, nil
}

func (m TUIModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder
	w := m.width
	if w < 60 {
		w = 60
	}

	elapsed := time.Since(m.orch.GetStartTime())
	queueDepth := m.orch.GetQueueDepth()
	completedCount := m.orch.GetCompletedCount()
	totalIn, totalOut := m.orch.GetTotalTokens()

	// Header
	header := fmt.Sprintf(" BOBBI Orchestrator")
	headerRight := fmt.Sprintf("Elapsed: %s  Queue: %d ", formatDuration(elapsed), queueDepth)
	padding := w - len(header) - len(headerRight)
	if padding < 1 {
		padding = 1
	}
	b.WriteString("┌" + strings.Repeat("─", w-2) + "┐\n")
	b.WriteString("│" + header + strings.Repeat(" ", padding) + headerRight + "│\n")
	b.WriteString("├" + strings.Repeat("─", 11) + "┬" + strings.Repeat("─", 10) + "┬" + strings.Repeat("─", w-25) + "┤\n")

	// Column headers
	b.WriteString(fmt.Sprintf("│ %-9s │ %-8s │ %-*s │\n", "Agent", "Status", w-27, "Tokens (in / out)"))
	b.WriteString("├" + strings.Repeat("─", 11) + "┼" + strings.Repeat("─", 10) + "┼" + strings.Repeat("─", w-25) + "┤\n")

	// Agent rows
	agentOrder := []agent.AgentType{agent.Architect, agent.Solver, agent.Evaluator, agent.Reviewer}
	var runningPrompts []string

	for _, at := range agentOrder {
		info := m.orch.GetAgentInfo(at)
		status := info.Status
		tokens := "— / —"
		if info.HasRun || info.InputTokens > 0 || info.OutputTokens > 0 {
			tokens = fmt.Sprintf("%s / %s", formatNumber(info.InputTokens), formatNumber(info.OutputTokens))
		}
		b.WriteString(fmt.Sprintf("│ %-9s │ %-8s │ %-*s │\n", at, status, w-27, tokens))

		if info.Status == "running" && info.Prompt != "" {
			summary := truncatePrompt(info.Prompt, w-6)
			runningPrompts = append(runningPrompts, fmt.Sprintf("%s: %s", at, summary))
		}
	}

	b.WriteString("├" + strings.Repeat("─", 11) + "┴" + strings.Repeat("─", 10) + "┴" + strings.Repeat("─", w-25) + "┤\n")

	// Prompt summary section
	if len(runningPrompts) > 0 {
		for _, p := range runningPrompts {
			line := " " + p
			if len(line) > w-4 {
				line = line[:w-5] + "..."
			}
			b.WriteString(fmt.Sprintf("│ %-*s │\n", w-4, line))
		}
	} else {
		b.WriteString(fmt.Sprintf("│ %-*s │\n", w-4, " (no agents running)"))
	}
	b.WriteString("├" + strings.Repeat("─", w-2) + "┤\n")

	// Footer
	footer := fmt.Sprintf(" Completed: %d │ Total tokens: %s in / %s out ",
		completedCount, formatNumber(totalIn), formatNumber(totalOut))
	if len(footer) > w-4 {
		footer = footer[:w-5] + "..."
	}
	b.WriteString(fmt.Sprintf("│ %-*s │\n", w-4, footer))
	b.WriteString("└" + strings.Repeat("─", w-2) + "┘\n")

	return b.String()
}

// formatDuration formats a duration as MM:SS.
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

// formatNumber formats an int64 with comma separators.
func formatNumber(n int64) string {
	if n == 0 {
		return "0"
	}
	s := fmt.Sprintf("%d", n)
	// Insert commas
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// truncatePrompt truncates a prompt to maxLen characters.
func truncatePrompt(prompt string, maxLen int) string {
	// Replace newlines with spaces for display
	prompt = strings.ReplaceAll(prompt, "\n", " ")
	if len(prompt) > maxLen {
		if maxLen > 3 {
			return prompt[:maxLen-3] + "..."
		}
		return prompt[:maxLen]
	}
	return prompt
}
