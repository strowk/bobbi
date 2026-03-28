package cmd

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"bobbi/internal/agent"

	"gopkg.in/yaml.v3"
)

func Init(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	solutionRemote := fs.String("solution-remote", "", "Remote URL to clone for solution repository")
	syncRemote := fs.String("sync-remote", "", "Remote URL to clone for synchronization repository")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// Check if already initialized
	bobbiDir := filepath.Join(cwd, ".bobbi")
	if _, err := os.Stat(bobbiDir); err == nil {
		return fmt.Errorf("already initialized (.bobbi/ directory exists)")
	}

	// Create .bobbi directories
	for _, dir := range []string{".bobbi", ".bobbi/queues", ".bobbi/completed", ".bobbi/failed", ".bobbi/backlog"} {
		if err := os.MkdirAll(filepath.Join(cwd, dir), 0755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	for _, agentType := range agent.AllTypes() {
		repoDir := filepath.Join(cwd, agent.RepoDir(agentType))
		// Use --solution-remote for solver agent
		if agentType == agent.Solver && *solutionRemote != "" {
			if err := initAgentRepoFromClone(repoDir, agentType, *solutionRemote); err != nil {
				return fmt.Errorf("clone %s repo: %w", agentType, err)
			}
		} else {
			if err := initAgentRepo(repoDir, agentType); err != nil {
				return fmt.Errorf("init %s repo: %w", agentType, err)
			}
		}
	}

	// Create output directory for final deliverables
	if err := os.MkdirAll(filepath.Join(cwd, "output"), 0755); err != nil {
		return fmt.Errorf("create output/: %w", err)
	}

	// Handle --sync-remote
	if *syncRemote != "" {
		if err := initSyncRepo(cwd, *syncRemote); err != nil {
			return fmt.Errorf("init sync repo: %w", err)
		}
	}

	fmt.Println("[bobbi] Initialized successfully")
	return nil
}

// initAgentRepoFromClone clones from a remote URL instead of git init,
// then ensures CLAUDE.md and .gitignore are up to date.
func initAgentRepoFromClone(repoDir string, agentType agent.AgentType, remoteURL string) error {
	fmt.Printf("[bobbi] Cloning %s repository from %s\n", agentType, remoteURL)

	cmd := exec.Command("git", "clone", remoteURL, repoDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %w", remoteURL, err)
	}

	// Ensure .claude/CLAUDE.md exists and is up to date
	claudeDir := filepath.Join(repoDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return err
	}

	expectedClaudeMD := agent.ClaudeMD(agentType)
	expectedGitignore := agent.GitIgnore(agentType)

	needsCommit := false

	// Check/update CLAUDE.md
	claudeMDPath := filepath.Join(claudeDir, "CLAUDE.md")
	existing, err := os.ReadFile(claudeMDPath)
	if err != nil || string(existing) != expectedClaudeMD {
		if err := os.WriteFile(claudeMDPath, []byte(expectedClaudeMD), 0644); err != nil {
			return err
		}
		needsCommit = true
	}

	// Check/update .gitignore
	gitignorePath := filepath.Join(repoDir, ".gitignore")
	existing, err = os.ReadFile(gitignorePath)
	if err != nil || string(existing) != expectedGitignore {
		if err := os.WriteFile(gitignorePath, []byte(expectedGitignore), 0644); err != nil {
			return err
		}
		needsCommit = true
	}

	// Create agent-specific dirs
	switch agentType {
	case agent.Solver, agent.Evaluator:
		os.MkdirAll(filepath.Join(repoDir, "architecture"), 0755)
		os.MkdirAll(filepath.Join(repoDir, "solution-deliverable"), 0755)
	case agent.Architect:
		specPath := filepath.Join(repoDir, "SPECIFICATION.md")
		if _, err := os.Stat(specPath); os.IsNotExist(err) {
			if err := os.WriteFile(specPath, []byte("# Specification\n\nTODO: Write the problem specification here.\n"), 0644); err != nil {
				return err
			}
			needsCommit = true
		}
	case agent.Reviewer:
		os.MkdirAll(filepath.Join(repoDir, "architecture"), 0755)
		os.MkdirAll(filepath.Join(repoDir, "solution"), 0755)
	}

	// Commit if anything changed
	if needsCommit {
		cmds := [][]string{
			{"git", "add", "-A"},
			{"git", "commit", "-m", "Update CLAUDE.md and .gitignore from bobbi init"},
		}
		for _, args := range cmds {
			c := exec.Command(args[0], args[1:]...)
			c.Dir = repoDir
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("git command %v: %w", args, err)
			}
		}
	}

	fmt.Printf("[bobbi] Initialized %s repository (cloned) at %s\n", agentType, repoDir)
	return nil
}

// releasedLockYAML returns the YAML content for a released lock file.
func releasedLockYAML() []byte {
	type lockFile struct {
		Status      string `yaml:"status"`
		Owner       string `yaml:"owner"`
		AcquiredAt  string `yaml:"acquired_at"`
		HeartbeatAt string `yaml:"heartbeat_at"`
	}
	lock := lockFile{
		Status:      "released",
		Owner:       "",
		AcquiredAt:  "",
		HeartbeatAt: "",
	}
	data, _ := yaml.Marshal(&lock)
	return data
}

// initSyncRepo clones the sync remote and ensures all four lock files exist.
func initSyncRepo(cwd, remoteURL string) error {
	syncDir := filepath.Join(cwd, "synchronization")
	fmt.Printf("[bobbi] Cloning synchronization repository from %s\n", remoteURL)

	cmd := exec.Command("git", "clone", remoteURL, syncDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone sync repo %s: %w", remoteURL, err)
	}

	// Check for required lock files
	requiredLocks := []string{
		"architecture.lock.yaml",
		"solution.lock.yaml",
		"evaluation.lock.yaml",
		"review.lock.yaml",
	}

	createdAny := false
	for _, name := range requiredLocks {
		path := filepath.Join(syncDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Printf("[bobbi] Creating missing lock file: %s\n", name)
			if err := os.WriteFile(path, releasedLockYAML(), 0644); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
			createdAny = true
		}
	}

	// Commit and push if any lock files were created
	if createdAny {
		cmds := [][]string{
			{"git", "add", "-A"},
			{"git", "commit", "-m", "Add missing lock files"},
			{"git", "push"},
		}
		for _, args := range cmds {
			c := exec.Command(args[0], args[1:]...)
			c.Dir = syncDir
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if err := c.Run(); err != nil {
				// Don't fail on push if there's no remote configured
				if args[0] == "git" && args[1] == "push" {
					// Check if remote exists
					out, _ := exec.Command("git", "-C", syncDir, "remote").Output()
					if strings.TrimSpace(string(out)) == "" {
						continue
					}
				}
				return fmt.Errorf("git command %v: %w", args, err)
			}
		}
	}

	fmt.Println("[bobbi] Synchronization repository initialized")
	return nil
}

func initAgentRepo(repoDir string, agentType agent.AgentType) error {
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
	case agent.Solver, agent.Evaluator:
		if err := os.MkdirAll(filepath.Join(repoDir, "architecture"), 0755); err != nil {
			return fmt.Errorf("create %s architecture dir: %w", agentType, err)
		}
		if err := os.MkdirAll(filepath.Join(repoDir, "solution-deliverable"), 0755); err != nil {
			return fmt.Errorf("create %s solution-deliverable dir: %w", agentType, err)
		}
	case agent.Architect:
		// Create empty SPECIFICATION.md
		specPath := filepath.Join(repoDir, "SPECIFICATION.md")
		if _, err := os.Stat(specPath); os.IsNotExist(err) {
			if err := os.WriteFile(specPath, []byte("# Specification\n\nTODO: Write the problem specification here.\n"), 0644); err != nil {
				return err
			}
		}
	case agent.Reviewer:
		if err := os.MkdirAll(filepath.Join(repoDir, "architecture"), 0755); err != nil {
			return fmt.Errorf("create reviewer architecture dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(repoDir, "solution"), 0755); err != nil {
			return fmt.Errorf("create reviewer solution dir: %w", err)
		}
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
		{"git", "init", "-b", "main"},
		{"git", "add", "-A"},
		{"git", "commit", "-m", "Initial commit"},
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
