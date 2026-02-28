package cmd

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// ReadInteractiveText opens a BubbleTea textarea for multi-line input.
// Returns the trimmed text or an error if the user cancels.
func ReadInteractiveText(placeholder string) (string, error) {
	m := newEditorModel(placeholder)
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("editor: %w", err)
	}
	em, ok := result.(editorModel)
	if !ok {
		return "", fmt.Errorf("editor: unexpected model type %T", result)
	}
	if em.cancelled {
		return "", fmt.Errorf("cancelled")
	}
	return strings.TrimSpace(em.textarea.Value()), nil
}

type editorModel struct {
	textarea  textarea.Model
	cancelled bool
	submitted bool
}

func newEditorModel(placeholder string) editorModel {
	ta := textarea.New()
	ta.Placeholder = placeholder
	ta.Focus()
	ta.SetWidth(80)
	ta.SetHeight(10)
	ta.ShowLineNumbers = false

	return editorModel{textarea: ta}
}

func (m editorModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m editorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m editorModel) View() string {
	if m.submitted || m.cancelled {
		return ""
	}
	return fmt.Sprintf(
		"Enter text (Ctrl+D to submit, Esc to cancel):\n\n%s\n",
		m.textarea.View(),
	)
}
