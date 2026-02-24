package cmd

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"bobbi/internal/orchestrator"
	"bobbi/internal/queue"
)

func Up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	promptFlag := fs.String("p", "", "prompt to pass to architect (e.g. bobbi up -p 'change the API to use REST')")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// Verify .bobbi directory exists
	if _, err := os.Stat(cwd + "/.bobbi"); os.IsNotExist(err) {
		return fmt.Errorf("not a bobbi project directory (run 'bobbi init' first)")
	}

	userPrompt := *promptFlag

	// If no -p flag and queue is empty, ask the user interactively
	if userPrompt == "" {
		queuesDir := cwd + "/.bobbi/queues"
		requests, _, err := queue.ReadRequests(queuesDir)
		if err == nil && len(requests) == 0 {
			userPrompt, err = askUserForPrompt()
			if err != nil {
				return fmt.Errorf("reading user input: %w", err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Printf("\n[bobbi] Received signal %v, shutting down...\n", sig)
		cancel()
	}()

	orch := orchestrator.New(cwd, userPrompt)
	fmt.Println("[bobbi] Starting orchestrator...")
	return orch.Run(ctx)
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
