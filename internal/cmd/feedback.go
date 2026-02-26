package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bobbi/internal/queue"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

var feedbackTypes = map[string]string{
	"bug":  "request_solution_change",
	"spec": "request_architecture_change",
	"test": "request_evaluation_change",
}

func Feedback(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: bobbi feedback <bug|spec|test> [description]\nIf description is omitted, opens an interactive editor")
	}

	fbType := args[0]
	reqType, ok := feedbackTypes[fbType]
	if !ok {
		return fmt.Errorf("unknown feedback type %q (expected: bug, spec, test)", fbType)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".bobbi")); os.IsNotExist(err) {
		return fmt.Errorf("not a bobbi project directory (run 'bobbi init' first)")
	}

	var message string
	if len(args) > 1 {
		message = strings.Join(args[1:], " ")
	} else {
		message, err = readFeedbackInteractive(fbType)
		if err != nil {
			return err
		}
	}

	if message == "" {
		return fmt.Errorf("feedback message cannot be empty")
	}

	queuesDir := filepath.Join(cwd, ".bobbi", "queues")
	path, err := queue.WriteRequest(queuesDir, reqType, "user", message)
	if err != nil {
		return fmt.Errorf("write feedback: %w", err)
	}

	fmt.Printf("[bobbi] Feedback queued: %s (%s)\n", fbType, path)
	return nil
}

// readFeedbackInteractive opens a bubbletea textarea for multi-line input.
func readFeedbackInteractive(fbType string) (string, error) {
	m := newFeedbackModel(fbType)
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("editor: %w", err)
	}
	fm, ok := result.(feedbackModel)
	if !ok {
		return "", fmt.Errorf("editor: unexpected model type %T", result)
	}
	if fm.cancelled {
		return "", fmt.Errorf("cancelled")
	}
	return strings.TrimSpace(fm.textarea.Value()), nil
}

type feedbackModel struct {
	textarea  textarea.Model
	cancelled bool
	submitted bool
}

func newFeedbackModel(fbType string) feedbackModel {
	ta := textarea.New()
	ta.Placeholder = "Describe the " + fbType + "..."
	ta.Focus()
	ta.SetWidth(80)
	ta.SetHeight(10)
	ta.ShowLineNumbers = false

	return feedbackModel{textarea: ta}
}

func (m feedbackModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m feedbackModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := msg.(tea.KeyMsg); ok {
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.cancelled = true
			return m, tea.Quit
		case tea.KeyCtrlD:
			m.submitted = true
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m feedbackModel) View() string {
	if m.submitted || m.cancelled {
		return ""
	}
	return fmt.Sprintf(
		"Enter feedback (Ctrl+D to submit, Esc to cancel):\n\n%s\n",
		m.textarea.View(),
	)
}
