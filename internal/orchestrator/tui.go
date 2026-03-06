package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"bobbi/internal/agent"

	"github.com/NimbleMarkets/ntcharts/sparkline"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// tickMsg triggers periodic TUI refresh.
type tickMsg time.Time

// OrchestratorDoneMsg signals the TUI that the orchestrator has fully stopped
// (all agents finished, Run() returned). Exported so up.go can send it.
type OrchestratorDoneMsg struct{}

// Color palette.
var (
	colorPurple    = lipgloss.Color("#7C3AED")
	colorCyan      = lipgloss.Color("#06B6D4")
	colorGreen     = lipgloss.Color("#10B981")
	colorAmber     = lipgloss.Color("#F59E0B")
	colorRed       = lipgloss.Color("#EF4444")
	colorGray      = lipgloss.Color("#6B7280")
	colorDimGray   = lipgloss.Color("#9CA3AF")
	colorLightGray = lipgloss.Color("#D1D5DB")
)

// agentOrder defines the display order of agents.
var agentOrder = []agent.AgentType{agent.Architect, agent.Solver, agent.Evaluator, agent.Reviewer}

// sparklineWidth is the fixed number of bars in each sparkline.
const sparklineWidth = 20

// detailLine holds a line of content and its type for styling in the detail view.
type detailLine struct {
	text     string
	lineType agent.LogLineType // agent.LogText for normal/prompt text
}

// TUIModel is the BubbleTea model for the BOBBI terminal UI.
type TUIModel struct {
	orch     *Orchestrator
	cancel   func() // cancel the orchestrator context
	width    int
	height   int
	quitting bool

	// shuttingDown is true after confirm_solution (o.done closed) but before
	// the orchestrator has fully stopped. The TUI stays visible during this
	// phase so the user can see running agents finish.
	shuttingDown bool

	// Agent selection / navigation
	selectedIdx int  // index into agentOrder
	detailView  bool // whether the detail view is open

	// Detail view scrolling
	detailOffset  int  // scroll offset (lines from top)
	followMode    bool // auto-scroll to bottom
	wrapMode      bool // word wrap in detail view
	thinkingMode  bool // show thinking blocks in detail view
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

	case tea.MouseMsg:
		if m.detailView {
			switch msg.Type {
			case tea.MouseWheelUp:
				m.followMode = false
				if m.detailOffset > 0 {
					m.detailOffset -= 3
					if m.detailOffset < 0 {
						m.detailOffset = 0
					}
				}
			case tea.MouseWheelDown:
				m.detailOffset += 3
				m.clampDetailOffset()
			}
		}
		return m, nil

	case tickMsg:
		// Check if the orchestrator has signalled confirm_solution (done).
		// We do NOT quit here — the TUI stays visible so the user can see
		// running agents finish. We quit only on OrchestratorDoneMsg which
		// is sent after Run() fully returns.
		if !m.shuttingDown {
			select {
			case <-m.orch.Done():
				m.shuttingDown = true
			default:
			}
		}
		// In follow mode, snap to bottom
		if m.detailView && m.followMode {
			m.snapToBottom()
		}
		return m, tickCmd()

	case OrchestratorDoneMsg:
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
	case "w":
		m.wrapMode = !m.wrapMode
		m.clampDetailOffset()
	case "t":
		m.thinkingMode = !m.thinkingMode
		m.clampDetailOffset()
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
	totalLines := m.detailTotalLines()
	viewH := m.detailViewHeight()
	if totalLines > viewH {
		m.detailOffset = totalLines - viewH
	} else {
		m.detailOffset = 0
	}
}

func (m *TUIModel) clampDetailOffset() {
	totalLines := m.detailTotalLines()
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

// detailTotalLines returns the total number of display lines for the current
// detail view, accounting for prompt section, log section, and word wrapping.
func (m *TUIModel) detailTotalLines() int {
	return len(m.detailContentLines())
}

// detailContentLines builds the combined prompt + log content for the detail view.
// Returns detailLine entries with type info for styling. When wrap mode is on,
// long lines are soft-wrapped at the inner width.
func (m TUIModel) detailContentLines() []detailLine {
	w := m.width
	if w < 60 {
		w = 60
	}
	inner := w - 4

	ai := m.orch.GetAgentInfo(agentOrder[m.selectedIdx])

	var lines []detailLine

	// ── PROMPT section ──
	if ai.Prompt != "" {
		lines = append(lines, detailLine{promptSeparator("PROMPT", inner), agent.LogText})
		promptLines := strings.Split(ai.Prompt, "\n")
		if m.wrapMode {
			for _, pl := range promptLines {
				for _, wl := range wrapLine(pl, inner) {
					lines = append(lines, detailLine{wl, agent.LogText})
				}
			}
		} else {
			for _, pl := range promptLines {
				lines = append(lines, detailLine{pl, agent.LogText})
			}
		}
	}

	// ── LOG section ──
	lines = append(lines, detailLine{promptSeparator("LOG", inner), agent.LogText})
	for _, ll := range ai.LogLines {
		// Filter thinking lines based on toggle
		if ll.Type == agent.LogThinking && !m.thinkingMode {
			continue
		}
		// Split embedded newlines so each visual line is a separate entry.
		// This ensures the height calculation matches the actual rendered output.
		subLines := strings.Split(ll.Text, "\n")
		for _, sl := range subLines {
			if m.wrapMode {
				for _, wl := range wrapLine(sl, inner) {
					lines = append(lines, detailLine{wl, ll.Type})
				}
			} else {
				lines = append(lines, detailLine{sl, ll.Type})
			}
		}
	}

	return lines
}

// promptSeparator returns a visual separator line like "── PROMPT ──────..."
func promptSeparator(label string, width int) string {
	prefix := "── " + label + " "
	remaining := width - len(prefix)
	if remaining < 0 {
		remaining = 0
	}
	return prefix + strings.Repeat("─", remaining)
}

// truncateToWidth truncates a string to fit within the given display width,
// appending "..." if truncated. Uses runewidth for accurate display-width
// calculation with multi-byte and double-width Unicode characters.
func truncateToWidth(line string, width int) string {
	if runewidth.StringWidth(line) <= width {
		return line
	}
	suffix := "..."
	targetWidth := width - runewidth.StringWidth(suffix)
	if targetWidth <= 0 {
		return suffix
	}
	w := 0
	for i, r := range line {
		rw := runewidth.RuneWidth(r)
		if w+rw > targetWidth {
			return line[:i] + suffix
		}
		w += rw
	}
	return line + suffix
}

// wrapLine soft-wraps a single line at the given display width.
// Uses runewidth for accurate display-width calculation with multi-byte
// and double-width Unicode characters.
func wrapLine(line string, width int) []string {
	if width <= 0 || runewidth.StringWidth(line) <= width {
		return []string{line}
	}
	var result []string
	for runewidth.StringWidth(line) > width {
		w := 0
		pos := 0
		for i, r := range line {
			rw := runewidth.RuneWidth(r)
			if w+rw > width {
				pos = i
				break
			}
			w += rw
			pos = i + len(string(r))
		}
		if pos == 0 {
			break
		}
		result = append(result, line[:pos])
		line = line[pos:]
	}
	result = append(result, line)
	return result
}

func (m TUIModel) detailViewHeight() int {
	// Header box (3) + log content box borders (2) + footer box (3) = 8 lines of chrome
	h := m.height - 8
	if h < 1 {
		h = 1
	}
	return h
}

func (m TUIModel) View() string {
	if m.quitting {
		return ""
	}

	var output string
	if m.detailView {
		output = m.viewDetail()
	} else {
		output = m.viewMain()
	}
	return padToHeight(output, m.height)
}

// padToHeight ensures the output has exactly `height` lines, padding with
// empty lines or truncating as needed. This prevents rendering artifacts when
// switching between views or when content doesn't fill the terminal.
func padToHeight(s string, height int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
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
	failedCount := m.orch.GetFailedCount()
	totalIn, totalOut := m.orch.GetTotalTokens()

	boxStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorPurple).
		Padding(0, 1).
		Width(w)

	// ── Header ───────────────────────────────────────────
	titleText := "BOBBI Orchestrator"
	if m.shuttingDown {
		titleText = "BOBBI Orchestrator — Shutting down..."
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(colorPurple).
		Render(titleText)
	meta := lipgloss.NewStyle().Foreground(colorDimGray).
		Render(fmt.Sprintf("%s   Queue: %d", formatDuration(elapsed), queueDepth))
	header := boxStyle.Render(
		spaced(title, meta, inner),
	)

	totalToolUses, totalToolFailures := m.orch.GetTotalToolUses()

	// ── Agent table ──────────────────────────────────────
	showSparklines := !m.orch.NoSparklines
	var colHdrText string
	if showSparklines {
		colHdrText = fmt.Sprintf("  %-12s %-12s %-10s %-22s %-18s %s", "Agent", "Status", "Session", "Activity", "Tools", "Tokens (in / out)")
	} else {
		colHdrText = fmt.Sprintf("  %-12s %-12s %-10s %-18s %s", "Agent", "Status", "Session", "Tools", "Tokens (in / out)")
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

		// Session ID: show truncated UUID when running, "—" otherwise
		sessionStr := "—"
		if ai.Status == "running" && ai.SessionID != "" {
			sid := ai.SessionID
			if len(sid) > 8 {
				sid = sid[:8]
			}
			sessionStr = sid
		}
		session := lipgloss.NewStyle().Foreground(colorDimGray).
			Render(fmt.Sprintf("%-10s", sessionStr))

		// Tool use counter
		toolStr := lipgloss.NewStyle().Foreground(colorGray).Render(fmt.Sprintf("%-18s", "tools: —"))
		if ai.HasRun || ai.ToolUses > 0 {
			if ai.ToolFailures > 0 {
				toolStr = lipgloss.NewStyle().Foreground(colorDimGray).
					Render(fmt.Sprintf("%-18s", fmt.Sprintf("tools: %d (%d err)", ai.ToolUses, ai.ToolFailures)))
			} else {
				toolStr = lipgloss.NewStyle().Foreground(colorDimGray).
					Render(fmt.Sprintf("%-18s", fmt.Sprintf("tools: %d", ai.ToolUses)))
			}
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
			row = fmt.Sprintf("%s%s %s %s %-22s %s %s", indicator, name, status, session, sparkStr, toolStr, tok)
		} else {
			row = fmt.Sprintf("%s%s %s %s %s %s", indicator, name, status, session, toolStr, tok)
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
			maxDisplayLen := 120 - runewidth.StringWidth(summaryPrefix)
			if maxDisplayLen < 10 {
				maxDisplayLen = 10
			}
			if runewidth.StringWidth(displayText) > maxDisplayLen {
				displayText = truncateToWidth(displayText, maxDisplayLen)
			}
			summary := fmt.Sprintf("    %s%s", summaryPrefix, displayText)
			if runewidth.StringWidth(summary) > inner {
				summary = truncateToWidth(summary, inner)
			}
			rows = append(rows, lipgloss.NewStyle().Foreground(colorDimGray).Italic(true).Render(summary))

			// Log preview: last 2 non-empty lines (excluding thinking)
			previewLines := lastNonEmptyLogLines(ai.LogLines, 2)
			for _, ll := range previewLines {
				line := ll.Text
				if runewidth.StringWidth(line) > inner-6 {
					line = truncateToWidth(line, inner-6)
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
	fFailed := lipgloss.NewStyle().Foreground(colorAmber).
		Render(fmt.Sprintf("Failed: %d", failedCount))
	var fTools string
	if totalToolFailures > 0 {
		fTools = lipgloss.NewStyle().Foreground(colorDimGray).
			Render(fmt.Sprintf("Tools: %d (%d err)", totalToolUses, totalToolFailures))
	} else {
		fTools = lipgloss.NewStyle().Foreground(colorDimGray).
			Render(fmt.Sprintf("Tools: %d", totalToolUses))
	}
	fMid := lipgloss.NewStyle().Foreground(colorDimGray).
		Render(fmt.Sprintf("Tokens: %s in / %s out",
			formatNumber(totalIn), formatNumber(totalOut)))
	fRight := lipgloss.NewStyle().Foreground(colorGray).
		Render("[↑↓, ⏎]")
	footer := boxStyle.Render(spaced(fLeft+" "+fFailed+" "+fTools, fMid+" "+fRight, inner))

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
	// Session ID: truncated UUID or "—"
	sessionDisplay := "—"
	if ai.SessionID != "" {
		sid := ai.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		sessionDisplay = sid
	}
	nameStatus := lipgloss.NewStyle().Bold(true).Foreground(colorCyan).
		Render(fmt.Sprintf("%s [%s] %s", at, ai.Status, sessionDisplay))
	var detailToolStr string
	if ai.ToolFailures > 0 {
		detailToolStr = fmt.Sprintf("tools: %d (%d err)", ai.ToolUses, ai.ToolFailures)
	} else {
		detailToolStr = fmt.Sprintf("tools: %d", ai.ToolUses)
	}
	tokStr := lipgloss.NewStyle().Foreground(colorDimGray).
		Render(fmt.Sprintf("%s  %s in / %s out",
			detailToolStr, formatNumber(ai.InputTokens), formatNumber(ai.OutputTokens)))

	var wrapIndicator string
	if m.wrapMode {
		wrapIndicator = lipgloss.NewStyle().Bold(true).Foreground(colorGreen).Render("[WRAP]")
	} else {
		wrapIndicator = lipgloss.NewStyle().Bold(true).Foreground(colorDimGray).Render("[NOWRAP]")
	}

	var followIndicator string
	if m.followMode {
		followIndicator = lipgloss.NewStyle().Bold(true).Foreground(colorGreen).Render("[FOLLOW]")
	} else {
		followIndicator = lipgloss.NewStyle().Bold(true).Foreground(colorAmber).Render("[PAUSED]")
	}

	indicators := wrapIndicator + " " + followIndicator
	if m.thinkingMode {
		thinkIndicator := lipgloss.NewStyle().Bold(true).Foreground(colorAmber).Render("[THINK]")
		indicators = thinkIndicator + " " + indicators
	}

	headerLeft := nameStatus
	headerRight := tokStr + "  " + indicators

	// Ensure header content fits within inner width to prevent lipgloss wrapping
	// which would add extra lines and corrupt BubbleTea's rendering.
	leftW := lipgloss.Width(headerLeft)
	rightW := lipgloss.Width(headerRight)
	if leftW+1+rightW > inner {
		// Truncate the right side to fit
		maxRight := inner - leftW - 1
		if maxRight < 10 {
			maxRight = 10
		}
		headerRight = truncateToWidth(headerRight, maxRight)
	}
	header := boxStyle.Render(spaced(headerLeft, headerRight, inner))

	// ── Content (prompt + log) ───────────────────────────
	viewH := m.detailViewHeight()
	allLines := m.detailContentLines()
	totalLines := len(allLines)

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
		dl := allLines[i]
		line := dl.text
		// In no-wrap mode, truncate long lines using display width
		if !m.wrapMode && runewidth.StringWidth(line) > inner {
			line = truncateToWidth(line, inner)
		}
		// Apply styling based on line type
		switch dl.lineType {
		case agent.LogToolUse:
			line = lipgloss.NewStyle().Foreground(colorCyan).Render(line)
		case agent.LogToolError:
			line = lipgloss.NewStyle().Foreground(colorRed).Render(line)
		case agent.LogThinking:
			line = lipgloss.NewStyle().Foreground(colorGray).Italic(true).Render(line)
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
		Render("[Esc] back  [f] follow  [w] wrap  [t] thinking  [↑↓/PgUp/PgDn] scroll")
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

// lastNonEmptyLogLines returns the last n non-empty log lines, excluding thinking lines.
func lastNonEmptyLogLines(lines []agent.LogLine, n int) []agent.LogLine {
	var result []agent.LogLine
	for i := len(lines) - 1; i >= 0 && len(result) < n; i-- {
		// Skip thinking lines (never shown in preview)
		if lines[i].Type == agent.LogThinking {
			continue
		}
		trimmed := strings.TrimSpace(lines[i].Text)
		if trimmed != "" {
			result = append([]agent.LogLine{{Type: lines[i].Type, Text: trimmed}}, result...)
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
