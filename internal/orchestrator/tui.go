package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"bobbi/internal/agent"

	"github.com/NimbleMarkets/ntcharts/sparkline"
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

// agentOrder defines the display order of agents.
var agentOrder = []agent.AgentType{agent.Architect, agent.Solver, agent.Evaluator, agent.Reviewer}

// sparklineWidth is the fixed number of bars in each sparkline.
const sparklineWidth = 20

// TUIModel is the BubbleTea model for the BOBBI terminal UI.
type TUIModel struct {
	orch     *Orchestrator
	cancel   func() // cancel the orchestrator context
	width    int
	height   int
	quitting bool

	// Agent selection / navigation
	selectedIdx int  // index into agentOrder
	detailView  bool // whether the detail view is open

	// Detail view scrolling
	detailOffset int  // scroll offset (lines from top)
	followMode   bool // auto-scroll to bottom
}

// NewTUIModel creates a new TUI model.
func NewTUIModel(orch *Orchestrator, cancel func()) TUIModel {
	return TUIModel{
		orch:       orch,
		cancel:     cancel,
		width:      80,
		height:     24,
		followMode: true,
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
		if m.detailView {
			return m.updateDetailView(msg)
		}
		return m.updateMainView(msg)

	case tickMsg:
		select {
		case <-m.orch.Done():
			m.quitting = true
			return m, tea.Quit
		default:
		}
		// In follow mode, snap to bottom
		if m.detailView && m.followMode {
			m.snapToBottom()
		}
		return m, tickCmd()

	case orchestratorDoneMsg:
		m.quitting = true
		return m, tea.Quit
	}

	return m, nil
}

func (m TUIModel) updateMainView(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.quitting = true
		m.cancel()
		return m, tea.Quit
	case "up", "k":
		if m.selectedIdx > 0 {
			m.selectedIdx--
		}
	case "down", "j":
		if m.selectedIdx < len(agentOrder)-1 {
			m.selectedIdx++
		}
	case "enter":
		m.detailView = true
		m.followMode = true
		m.snapToBottom()
	}
	return m, nil
}

func (m TUIModel) updateDetailView(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		m.cancel()
		return m, tea.Quit
	case "esc", "q":
		m.detailView = false
		return m, nil
	case "f":
		m.followMode = !m.followMode
		if m.followMode {
			m.snapToBottom()
		}
	case "up", "k":
		m.followMode = false
		if m.detailOffset > 0 {
			m.detailOffset--
		}
	case "down", "j":
		m.detailOffset++
		m.clampDetailOffset()
	case "pgup":
		m.followMode = false
		viewH := m.detailViewHeight()
		m.detailOffset -= viewH
		if m.detailOffset < 0 {
			m.detailOffset = 0
		}
	case "pgdown":
		viewH := m.detailViewHeight()
		m.detailOffset += viewH
		m.clampDetailOffset()
	}
	return m, nil
}

func (m *TUIModel) snapToBottom() {
	ai := m.orch.GetAgentInfo(agentOrder[m.selectedIdx])
	totalLines := len(ai.LogLines)
	viewH := m.detailViewHeight()
	if totalLines > viewH {
		m.detailOffset = totalLines - viewH
	} else {
		m.detailOffset = 0
	}
}

func (m *TUIModel) clampDetailOffset() {
	ai := m.orch.GetAgentInfo(agentOrder[m.selectedIdx])
	totalLines := len(ai.LogLines)
	viewH := m.detailViewHeight()
	maxOffset := totalLines - viewH
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.detailOffset > maxOffset {
		m.detailOffset = maxOffset
	}
	// Re-enable follow if at bottom
	if m.detailOffset >= maxOffset && maxOffset > 0 {
		m.followMode = true
	}
}

func (m TUIModel) detailViewHeight() int {
	// Total height minus header (3 lines), footer (2 lines), and some padding
	h := m.height - 6
	if h < 1 {
		h = 1
	}
	return h
}

func (m TUIModel) View() string {
	if m.quitting {
		return ""
	}

	if m.detailView {
		return m.viewDetail()
	}
	return m.viewMain()
}

func (m TUIModel) viewMain() string {
	w := m.width
	if w < 60 {
		w = 60
	}
	inner := w - 4

	elapsed := time.Since(m.orch.GetStartTime())
	queueDepth := m.orch.GetQueueDepth()
	completedCount := m.orch.GetCompletedCount()
	totalIn, totalOut := m.orch.GetTotalTokens()

	boxStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorPurple).
		Padding(0, 1).
		Width(w)

	// ── Header ───────────────────────────────────────────
	title := lipgloss.NewStyle().Bold(true).Foreground(colorPurple).
		Render("BOBBI Orchestrator")
	meta := lipgloss.NewStyle().Foreground(colorDimGray).
		Render(fmt.Sprintf("%s   Queue: %d", formatDuration(elapsed), queueDepth))
	header := boxStyle.Render(
		spaced(title, meta, inner),
	)

	// ── Agent table ──────────────────────────────────────
	showSparklines := !m.orch.NoSparklines
	var colHdrText string
	if showSparklines {
		colHdrText = fmt.Sprintf("  %-12s %-12s %-22s %s", "Agent", "Status", "Activity", "Tokens (in / out)")
	} else {
		colHdrText = fmt.Sprintf("  %-12s %-12s %s", "Agent", "Status", "Tokens (in / out)")
	}
	colHdr := lipgloss.NewStyle().Bold(true).Foreground(colorLightGray).Render(colHdrText)
	sep := lipgloss.NewStyle().Foreground(colorGray).
		Render("  " + strings.Repeat("─", inner))

	rows := make([]string, 0, len(agentOrder)*4)

	sharedMax := m.orch.GetSharedMaxTokens()

	for i, at := range agentOrder {
		ai := m.orch.GetAgentInfo(at)

		// Selection indicator
		indicator := "  "
		if i == m.selectedIdx {
			indicator = "> "
		}

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

		var row string
		if showSparklines {
			sparkStr := renderSparkline(ai.SparklineData, sharedMax)
			row = fmt.Sprintf("%s%s %s %-22s %s", indicator, name, status, sparkStr, tok)
		} else {
			row = fmt.Sprintf("%s%s %s %s", indicator, name, status, tok)
		}

		// Highlight selected row
		if i == m.selectedIdx {
			row = lipgloss.NewStyle().Background(lipgloss.Color("#1E1E2E")).Render(row)
		}

		rows = append(rows, row)

		// Prompt summary and log preview for running agents
		if ai.Status == "running" && ai.Prompt != "" {
			badge := requestBadge(ai.RequestType)
			displayText := ai.AdditionalContext
			if displayText == "" {
				displayText = ai.Prompt
			}
			displayText = strings.ReplaceAll(displayText, "\n", " ")

			summaryPrefix := fmt.Sprintf("%s %s: ", at, badge)
			maxDisplayLen := 120 - len(summaryPrefix)
			if maxDisplayLen < 10 {
				maxDisplayLen = 10
			}
			if len(displayText) > maxDisplayLen {
				displayText = displayText[:maxDisplayLen-3] + "..."
			}
			summary := fmt.Sprintf("    %s%s", summaryPrefix, displayText)
			if len(summary) > inner {
				summary = summary[:inner-3] + "..."
			}
			rows = append(rows, lipgloss.NewStyle().Foreground(colorDimGray).Italic(true).Render(summary))

			// Log preview: last 2 non-empty lines
			previewLines := lastNonEmptyLines(ai.LogLines, 2)
			for _, line := range previewLines {
				if len(line) > inner-6 {
					line = line[:inner-9] + "..."
				}
				rows = append(rows, lipgloss.NewStyle().Foreground(colorGray).Render("      "+line))
			}
		}
	}

	table := lipgloss.JoinVertical(lipgloss.Left,
		append([]string{colHdr, sep}, rows...)...)

	// ── Footer ───────────────────────────────────────────
	fLeft := lipgloss.NewStyle().Foreground(colorGreen).
		Render(fmt.Sprintf("Completed: %d", completedCount))
	fMid := lipgloss.NewStyle().Foreground(colorDimGray).
		Render(fmt.Sprintf("Tokens: %s in / %s out",
			formatNumber(totalIn), formatNumber(totalOut)))
	fRight := lipgloss.NewStyle().Foreground(colorGray).
		Render("[↑↓ select, ⏎ detail]")
	footer := boxStyle.Render(spaced(fLeft, fMid+" "+fRight, inner))

	// ── Help ─────────────────────────────────────────────
	help := lipgloss.NewStyle().Foreground(colorGray).Padding(0, 2).
		Render("Press q or ctrl+c to quit")

	// ── Compose ──────────────────────────────────────────
	parts := []string{header, "", table, "", footer, help, ""}

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m TUIModel) viewDetail() string {
	w := m.width
	if w < 60 {
		w = 60
	}
	inner := w - 4

	at := agentOrder[m.selectedIdx]
	ai := m.orch.GetAgentInfo(at)

	boxStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorPurple).
		Padding(0, 1).
		Width(w)

	// ── Header ───────────────────────────────────────────
	nameStatus := lipgloss.NewStyle().Bold(true).Foreground(colorCyan).
		Render(fmt.Sprintf("%s [%s]", at, ai.Status))
	tokStr := lipgloss.NewStyle().Foreground(colorDimGray).
		Render(fmt.Sprintf("%s in / %s out",
			formatNumber(ai.InputTokens), formatNumber(ai.OutputTokens)))

	var modeIndicator string
	if m.followMode {
		modeIndicator = lipgloss.NewStyle().Bold(true).Foreground(colorGreen).Render("[FOLLOW]")
	} else {
		modeIndicator = lipgloss.NewStyle().Bold(true).Foreground(colorAmber).Render("[PAUSED]")
	}

	headerLeft := nameStatus
	headerRight := tokStr + "  " + modeIndicator
	header := boxStyle.Render(spaced(headerLeft, headerRight, inner))

	// ── Log content ──────────────────────────────────────
	viewH := m.detailViewHeight()
	lines := ai.LogLines
	totalLines := len(lines)

	// Determine visible range
	startLine := m.detailOffset
	if startLine > totalLines {
		startLine = totalLines
	}
	endLine := startLine + viewH
	if endLine > totalLines {
		endLine = totalLines
	}

	var visibleLines []string
	for i := startLine; i < endLine; i++ {
		line := lines[i]
		if len(line) > inner {
			line = line[:inner-3] + "..."
		}
		visibleLines = append(visibleLines, line)
	}

	// Pad to fill view height
	for len(visibleLines) < viewH {
		visibleLines = append(visibleLines, "")
	}

	logContent := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorGray).
		Padding(0, 1).
		Width(w).
		Render(strings.Join(visibleLines, "\n"))

	// ── Footer ───────────────────────────────────────────
	footerText := lipgloss.NewStyle().Foreground(colorGray).
		Render("[Esc] back  [f] toggle follow  [↑↓/PgUp/PgDn] scroll")
	footer := boxStyle.Render(footerText)

	return lipgloss.JoinVertical(lipgloss.Left, header, logContent, footer)
}

// renderSparkline creates a sparkline string from data using ntcharts.
func renderSparkline(data []float64, sharedMax float64) string {
	if len(data) == 0 {
		return strings.Repeat(" ", sparklineWidth)
	}

	// Use only the last sparklineWidth data points
	displayData := data
	if len(displayData) > sparklineWidth {
		displayData = displayData[len(displayData)-sparklineWidth:]
	}

	if sharedMax <= 0 {
		sharedMax = 1
	}

	sl := sparkline.New(sparklineWidth, 1,
		sparkline.WithNoAutoMaxValue(),
		sparkline.WithMaxValue(sharedMax),
		sparkline.WithStyle(lipgloss.NewStyle().Foreground(colorCyan)),
	)
	sl.PushAll(displayData)
	sl.DrawColumnsOnly()
	return sl.View()
}

// requestBadge returns the badge string for a request type.
func requestBadge(requestType string) string {
	if strings.HasPrefix(requestType, "start_") {
		return "[start]"
	}
	if strings.HasPrefix(requestType, "request_") {
		return "[change]"
	}
	return "[start]"
}

// lastNonEmptyLines returns the last n non-empty lines from a slice.
func lastNonEmptyLines(lines []string, n int) []string {
	var result []string
	for i := len(lines) - 1; i >= 0 && len(result) < n; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" {
			result = append([]string{trimmed}, result...)
		}
	}
	return result
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

