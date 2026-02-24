package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"bobbi/internal/agent"
)

func Init() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// Create .bobbi directories
	for _, dir := range []string{".bobbi", ".bobbi/queues", ".bobbi/completed"} {
		if err := os.MkdirAll(filepath.Join(cwd, dir), 0755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	bobbBin, err := os.Executable()
	if err != nil {
		bobbBin = "bobbi"
	}

	for _, agentType := range agent.AllTypes() {
		repoDir := filepath.Join(cwd, agent.RepoDir(agentType))
		if err := initAgentRepo(repoDir, agentType, bobbBin); err != nil {
			return fmt.Errorf("init %s repo: %w", agentType, err)
		}
	}

	// Create output directory for final deliverables
	if err := os.MkdirAll(filepath.Join(cwd, "output"), 0755); err != nil {
		return fmt.Errorf("create output/: %w", err)
	}

	fmt.Println("[bobbi] Initialized successfully")
	return nil
}

func initAgentRepo(repoDir string, agentType agent.AgentType, bobBin string) error {
	// Create directories
	claudeDir := filepath.Join(repoDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return err
	}

	// Write .claude/CLAUDE.md
	if err := os.WriteFile(
		filepath.Join(claudeDir, "CLAUDE.md"),
		[]byte(agent.ClaudeMD(agentType)),
		0644,
	); err != nil {
		return err
	}

	// Write .mcp.json
	if err := os.WriteFile(
		filepath.Join(repoDir, ".mcp.json"),
		[]byte(agent.McpJSON(agentType, bobBin)),
		0644,
	); err != nil {
		return err
	}

	// Write .gitignore
	if err := os.WriteFile(
		filepath.Join(repoDir, ".gitignore"),
		[]byte(agent.GitIgnore(agentType)),
		0644,
	); err != nil {
		return err
	}

	// Create agent-specific dirs and files
	switch agentType {
	case agent.Solver:
		os.MkdirAll(filepath.Join(repoDir, "architecture"), 0755)
		os.MkdirAll(filepath.Join(repoDir, "solution-deliverable"), 0755)
	case agent.Evaluator:
		os.MkdirAll(filepath.Join(repoDir, "architecture"), 0755)
		os.MkdirAll(filepath.Join(repoDir, "solution-deliverable"), 0755)
	case agent.Architect:
		// Create empty SPECIFICATION.md
		specPath := filepath.Join(repoDir, "SPECIFICATION.md")
		if _, err := os.Stat(specPath); os.IsNotExist(err) {
			if err := os.WriteFile(specPath, []byte("# Specification\n\nTODO: Write the problem specification here.\n"), 0644); err != nil {
				return err
			}
		}
	case agent.Reviewer:
		os.MkdirAll(filepath.Join(repoDir, "solution"), 0755)
	}

	// Git init + initial commit
	if err := gitInit(repoDir); err != nil {
		return err
	}

	fmt.Printf("[bobbi] Initialized %s repository at %s\n", agentType, repoDir)
	return nil
}

func gitInit(dir string) error {
	cmds := [][]string{
		{"git", "init"},
		{"git", "add", "-A"},
		{"git", "commit", "-m", "Initial commit"},
		{"git", "branch", "-m", "main"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git command %v: %w", args, err)
		}
	}
	return nil
}
