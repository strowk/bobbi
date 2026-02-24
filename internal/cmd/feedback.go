package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"bobbcode/internal/queue"
)

var feedbackTypes = map[string]string{
	"bug":  "request_solution_change",
	"spec": "request_architecture_change",
	"test": "request_evaluation_change",
}

func Feedback(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: bobbcode feedback <bug|spec|test> [description]\nIf description is omitted, reads from stdin")
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
	if _, err := os.Stat(cwd + "/.bobb"); os.IsNotExist(err) {
		return fmt.Errorf("not a bobb project directory (run 'bobbcode init' first)")
	}

	var message string
	if len(args) > 1 {
		message = strings.Join(args[1:], " ")
	} else {
		fmt.Fprintln(os.Stderr, "[bobbcode] Enter feedback (Ctrl+D to submit, Ctrl+C to cancel):")
		data, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		message = strings.TrimSpace(string(data))
	}

	if message == "" {
		return fmt.Errorf("feedback message cannot be empty")
	}

	queuesDir := cwd + "/.bobb/queues"
	path, err := queue.WriteRequest(queuesDir, reqType, "user", message)
	if err != nil {
		return fmt.Errorf("write feedback: %w", err)
	}

	fmt.Printf("[bobbcode] Feedback queued: %s (%s)\n", fbType, path)
	return nil
}
