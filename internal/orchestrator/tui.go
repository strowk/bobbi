package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"bobbi/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// tickMsg triggers periodic TUI refresh.
type tickMsg time.Time

// orchestratorDoneMsg signals the TUI that the orchestrator has finished.
type orchestratorDoneMsg struct{}

// Color palette.
var (
	colorPurple    = lipgloss.Color("#7C3AED")
	colorCyan      = lipgloss.Color("#06B6D4")
	colorGreen     = lipgloss.Color("#10B981")
	colorAmber     = lipgloss.Color("#F59E0B")
	colorGray      = lipgloss.Color("#6B7280")
	colorDimGray   = lipgloss.Color("#9CA3AF")
	colorLightGray = lipgloss.Color("#D1D5DB")
)

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

	w := m.width
	if w < 60 {
		w = 60
	}
	// Content width inside a bordered+padded box: 2 for border, 2 for padding(0,1).
	inner := w - 4

	elapsed := time.Since(m.orch.GetStartTime())
	queueDepth := m.orch.GetQueueDepth()
	completedCount := m.orch.GetCompletedCount()
	totalIn, totalOut := m.orch.GetTotalTokens()

	boxStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorPurple).
		Padding(0, 1).
		Width(inner)

	// ── Header ───────────────────────────────────────────
	title := lipgloss.NewStyle().Bold(true).Foreground(colorPurple).
		Render("BOBBI Orchestrator")
	meta := lipgloss.NewStyle().Foreground(colorDimGray).
		Render(fmt.Sprintf("%s   Queue: %d", formatDuration(elapsed), queueDepth))
	header := boxStyle.Render(
		spaced(title, meta, inner),
	)

	// ── Agent table ──────────────────────────────────────
	colHdr := lipgloss.NewStyle().Bold(true).Foreground(colorLightGray).
		Render(fmt.Sprintf("  %-12s %-12s %s", "Agent", "Status", "Tokens (in / out)"))
	sep := lipgloss.NewStyle().Foreground(colorGray).
		Render("  " + strings.Repeat("─", inner))

	agentOrder := []agent.AgentType{agent.Architect, agent.Solver, agent.Evaluator, agent.Reviewer}
	rows := make([]string, 0, len(agentOrder))
	var prompts []string

	for _, at := range agentOrder {
		ai := m.orch.GetAgentInfo(at)

		name := lipgloss.NewStyle().Bold(true).Foreground(colorCyan).
			Render(fmt.Sprintf("%-12s", at))

		var status string
		switch ai.Status {
		case "running":
			status = lipgloss.NewStyle().Bold(true).Foreground(colorGreen).
				Render(fmt.Sprintf("%-12s", "● running"))
		case "queued":
			status = lipgloss.NewStyle().Foreground(colorAmber).
				Render(fmt.Sprintf("%-12s", "◌ queued"))
		default:
			status = lipgloss.NewStyle().Foreground(colorGray).
				Render(fmt.Sprintf("%-12s", "○ idle"))
		}

		tok := lipgloss.NewStyle().Foreground(colorGray).Render("— / —")
		if ai.HasRun || ai.InputTokens > 0 || ai.OutputTokens > 0 {
			tok = lipgloss.NewStyle().Foreground(colorDimGray).
				Render(fmt.Sprintf("%s / %s",
					formatNumber(ai.InputTokens), formatNumber(ai.OutputTokens)))
		}

		rows = append(rows, fmt.Sprintf("  %s %s %s", name, status, tok))

		if ai.Status == "running" && ai.Prompt != "" {
			summary := truncatePrompt(ai.Prompt, inner-len(string(at))-4)
			prompts = append(prompts, fmt.Sprintf("%s: %s", at, summary))
		}
	}

	table := lipgloss.JoinVertical(lipgloss.Left,
		append([]string{colHdr, sep}, rows...)...)

	// ── Running prompts ──────────────────────────────────
	var promptBox string
	if len(prompts) > 0 {
		lines := make([]string, len(prompts))
		for i, p := range prompts {
			if len(p) > inner-2 {
				p = p[:inner-5] + "..."
			}
			lines[i] = lipgloss.NewStyle().Foreground(colorDimGray).Italic(true).Render(p)
		}
		promptBox = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(colorGray).
			Padding(0, 1).
			Width(inner).
			Render(strings.Join(lines, "\n"))
	}

	// ── Footer ───────────────────────────────────────────
	fLeft := lipgloss.NewStyle().Foreground(colorGreen).
		Render(fmt.Sprintf("Completed: %d", completedCount))
	fRight := lipgloss.NewStyle().Foreground(colorDimGray).
		Render(fmt.Sprintf("Tokens: %s in / %s out",
			formatNumber(totalIn), formatNumber(totalOut)))
	footer := boxStyle.Render(spaced(fLeft, fRight, inner))

	// ── Help ─────────────────────────────────────────────
	help := lipgloss.NewStyle().Foreground(colorGray).Padding(0, 2).
		Render("Press q or ctrl+c to quit")

	// ── Compose ──────────────────────────────────────────
	parts := []string{header, "", table}
	if promptBox != "" {
		parts = append(parts, "", promptBox)
	}
	parts = append(parts, "", footer, help, "")

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// spaced joins left and right strings with spaces to fill width.
func spaced(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
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
	prefix := ""
	if s[0] == '-' {
		prefix = "-"
		s = s[1:]
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return prefix + string(result)
}

// truncatePrompt truncates a prompt to maxLen characters.
func truncatePrompt(prompt string, maxLen int) string {
	prompt = strings.ReplaceAll(prompt, "\n", " ")
	if len(prompt) > maxLen {
		if maxLen > 3 {
			return prompt[:maxLen-3] + "..."
		}
		return prompt[:maxLen]
	}
	return prompt
}
