package cmd

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"bobbi/internal/orchestrator"
	"bobbi/internal/queue"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

func Up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	promptFlag := fs.String("p", "", "prompt to pass to architect")
	rawFlag := fs.Bool("raw", false, "disable Terminal UI and use raw streamed output mode")
	noLogFlag := fs.Bool("no-log", false, "disable writing logs to file")
	noSparklinesFlag := fs.Bool("no-sparklines", false, "disable per-agent activity sparkline charts in the Terminal UI")
	timeoutFlag := fs.Duration("timeout", 30*time.Minute, "maximum time limit for the entire orchestrator run (0 to disable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	if _, err := os.Stat(filepath.Join(cwd, ".bobbi")); os.IsNotExist(err) {
		return fmt.Errorf("not a bobbi project directory (run 'bobbi init' first)")
	}

	explicitRaw := *rawFlag
	isTTY := isatty.IsTerminal(os.Stdout.Fd())
	// Use raw mode if explicitly requested or if stdout is not a TTY
	rawMode := explicitRaw || !isTTY

	// File logging is enabled when:
	// - NOT in explicit raw mode (--raw flag)
	// - AND --no-log is not passed
	// This means: TUI mode logs, non-TTY fallback logs, explicit raw does NOT log
	enableFileLog := !explicitRaw && !*noLogFlag

	userPrompt := *promptFlag

	// If no -p flag and queue is empty, ask the user interactively
	if userPrompt == "" {
		queuesDir := filepath.Join(cwd, ".bobbi", "queues")
		requests, _, err := queue.ReadRequests(queuesDir, nil)
		if err == nil && len(requests) == 0 {
			userPrompt, err = askUserForPrompt()
			if err != nil {
				return fmt.Errorf("reading user input: %w", err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch := orchestrator.New(cwd, userPrompt, rawMode, *timeoutFlag, *noSparklinesFlag)

	// Enable file logging if applicable
	if enableFileLog {
		if err := orch.EnableFileLogging(); err != nil {
			fmt.Fprintf(os.Stderr, "[bobbi] Warning: could not open log file: %v\n", err)
		} else {
			defer orch.CloseLogFile()
		}
	}

	if rawMode {
		// Raw mode: signal handling + direct orchestrator run
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintf(os.Stderr, "\n[bobbi] Received signal, shutting down...\n")
			cancel()
		}()

		fmt.Fprintln(os.Stderr, "[bobbi] Starting orchestrator (raw mode)...")
		return orch.Run(ctx)
	}

	// TUI mode
	model := orchestrator.NewTUIModel(orch, cancel)
	program := tea.NewProgram(model, tea.WithAltScreen())

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.Run(ctx)
	}()

	if _, err := program.Run(); err != nil {
		cancel()
		orchErr := <-errCh
		if orchErr != nil {
			return fmt.Errorf("TUI error: %w (orchestrator also errored: %v)", err, orchErr)
		}
		return fmt.Errorf("TUI error: %w", err)
	}

	cancel()
	orchErr := <-errCh
	return orchErr
}

func askUserForPrompt() (string, error) {
	fmt.Println("[bobbi] No pending requests in the queue.")
	fmt.Println()
	fmt.Println("  1) Start architect with initial specification (default)")
	fmt.Println("  2) Provide instructions for the architect")
	fmt.Println()
	fmt.Print("Choose [1/2]: ")

	reader := bufio.NewReader(os.Stdin)
	choice, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	choice = strings.TrimSpace(choice)

	switch choice {
	case "", "1":
		return "", nil
	case "2":
		fmt.Print("\nEnter your instructions: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(input), nil
	default:
		return "", fmt.Errorf("invalid choice: %s", choice)
	}
}
