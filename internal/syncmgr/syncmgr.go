package syncmgr

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bobbi/internal/agent"
	"bobbi/internal/config"
	gh "bobbi/internal/github"
	gl "bobbi/internal/gitlab"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// LockFile represents the YAML content of a lock file.
type LockFile struct {
	Status      string `yaml:"status"`
	Owner       string `yaml:"owner"`
	AcquiredAt  string `yaml:"acquired_at"`
	HeartbeatAt string `yaml:"heartbeat_at"`
}

// Manager handles all synchronization operations.
type Manager struct {
	baseDir    string
	cfg        *config.Config
	instanceID string // UUID suffix
	identity   string // owner_prefix-uuid
	logFunc    func(format string, args ...interface{})

	// Track which agent types currently hold locks
	heldLocks   map[agent.AgentType]bool
	heldLocksMu sync.Mutex

	// Heartbeat cancellation per agent
	heartbeatCancel   map[agent.AgentType]func()
	heartbeatCancelMu sync.Mutex
}

// New creates a new sync manager.
func New(baseDir string, cfg *config.Config, logFunc func(format string, args ...interface{})) *Manager {
	return &Manager{
		baseDir:         baseDir,
		cfg:             cfg,
		logFunc:         logFunc,
		heldLocks:       make(map[agent.AgentType]bool),
		heartbeatCancel: make(map[agent.AgentType]func()),
	}
}

// Enabled returns true if synchronization is enabled.
func (m *Manager) Enabled() bool {
	return m.cfg != nil && m.cfg.Sync.Enabled
}

// IsSynced returns true if the given agent type is configured for synchronization.
func (m *Manager) IsSynced(agentType agent.AgentType) bool {
	if !m.Enabled() {
		return false
	}
	return m.cfg.IsSyncedAgent(string(agentType))
}

// Setup generates instance ID and creates sync repo if needed.
// Called during bobbi up startup.
func (m *Manager) Setup() error {
	if !m.Enabled() {
		return nil
	}

	// Generate and persist instance ID
	id := uuid.New().String()
	m.instanceID = id
	m.identity = m.cfg.Sync.OwnerPrefix + "-" + id

	idPath := filepath.Join(m.baseDir, ".bobbi", "instance_id")
	if err := os.WriteFile(idPath, []byte(id+"\n"), 0644); err != nil {
		return fmt.Errorf("write instance_id: %w", err)
	}
	m.log("Instance identity: %s", m.identity)

	// Create synchronization repo if it doesn't exist
	syncDir := filepath.Join(m.baseDir, "synchronization")
	if _, err := os.Stat(syncDir); os.IsNotExist(err) {
		if err := m.createSyncRepo(syncDir); err != nil {
			return fmt.Errorf("create sync repo: %w", err)
		}
	}

	return nil
}

// Cleanup deletes instance_id file. Called on clean shutdown.
func (m *Manager) Cleanup() {
	if !m.Enabled() {
		return
	}
	idPath := filepath.Join(m.baseDir, ".bobbi", "instance_id")
	os.Remove(idPath)
	m.log("Deleted instance_id")
}

// AcquireLock acquires the lock for the given agent type.
// Blocks until the lock is acquired (with exponential backoff on contention)
// or the context is cancelled.
func (m *Manager) AcquireLock(ctx context.Context, agentType agent.AgentType) error {
	lockFile := m.lockFileName(agentType)
	backoff := 5 * time.Second
	maxBackoff := 60 * time.Second

	for {
		err := m.tryAcquireLock(agentType, lockFile)
		if err == nil {
			m.heldLocksMu.Lock()
			m.heldLocks[agentType] = true
			m.heldLocksMu.Unlock()
			m.log("Acquired %s lock", lockFile)
			return nil
		}

		if isContention(err) {
			m.log("%v", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
			continue
		}

		return err
	}
}

// PullAgentRepo runs git pull in the agent's repository directory.
func (m *Manager) PullAgentRepo(agentType agent.AgentType) error {
	repoDir := filepath.Join(m.baseDir, agent.RepoDir(agentType))
	if err := m.gitPullSafe(repoDir); err != nil {
		return fmt.Errorf("git pull agent repo %s: %w", agentType, err)
	}
	return nil
}

// StartHeartbeat begins a background goroutine that updates the heartbeat for the given agent type.
func (m *Manager) StartHeartbeat(agentType agent.AgentType) {
	m.heartbeatCancelMu.Lock()
	// Cancel any existing heartbeat for this agent
	if cancel, ok := m.heartbeatCancel[agentType]; ok {
		cancel()
	}
	done := make(chan struct{})
	m.heartbeatCancel[agentType] = func() { close(done) }
	m.heartbeatCancelMu.Unlock()

	interval := m.cfg.GetHeartbeatInterval()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				m.sendHeartbeat(agentType)
			}
		}
	}()
}

// StopHeartbeat stops the heartbeat goroutine for the given agent type.
func (m *Manager) StopHeartbeat(agentType agent.AgentType) {
	m.heartbeatCancelMu.Lock()
	if cancel, ok := m.heartbeatCancel[agentType]; ok {
		cancel()
		delete(m.heartbeatCancel, agentType)
	}
	m.heartbeatCancelMu.Unlock()
}

// PostAgentSync performs post-agent completion synchronization:
// porcelain check, commit recovery, push, and lock release.
// Returns true if the agent should be treated as failed (recovery exhaustion).
func (m *Manager) PostAgentSync(agentType agent.AgentType, agentExitErr error, recoveryOpts *agent.StartOptions) bool {
	m.StopHeartbeat(agentType)

	repoDir := filepath.Join(m.baseDir, agent.RepoDir(agentType))
	treatAsFailed := false

	// Porcelain check
	dirty, err := m.isDirty(repoDir)
	if err != nil {
		m.log("Error checking porcelain for %s: %v", agentType, err)
	}

	if dirty {
		// Commit recovery attempts (up to 3)
		recovered := false
		for attempt := 1; attempt <= 3; attempt++ {
			m.log("Commit recovery attempt %d/3 for %s", attempt, agentType)
			m.runRecoveryAgent(agentType, repoDir, recoveryOpts)

			dirty, err = m.isDirty(repoDir)
			if err != nil {
				m.log("Error checking porcelain after recovery %d for %s: %v", attempt, agentType, err)
				continue
			}
			if !dirty {
				recovered = true
				m.log("Commit recovery succeeded for %s on attempt %d", agentType, attempt)
				break
			}
		}

		if !recovered {
			m.log("WARNING: Commit recovery failed after 3 attempts for %s — not pushing", agentType)
			m.ReleaseLock(agentType)
			return true // treat as failed
		}
	}

	// Push agent repo (even on agent failure, committed changes should be synced)
	if err := m.pushAgentRepo(repoDir, agentType); err != nil {
		m.log("Error pushing %s repo: %v", agentType, err)
	}

	// Release lock
	m.ReleaseLock(agentType)

	return treatAsFailed
}

// ReleaseLock releases the lock for the given agent type.
func (m *Manager) ReleaseLock(agentType agent.AgentType) {
	lockFile := m.lockFileName(agentType)
	syncDir := filepath.Join(m.baseDir, "synchronization")

	for attempt := 1; attempt <= 3; attempt++ {
		if err := m.gitPullSafe(syncDir); err != nil {
			m.log("Error pulling sync repo for lock release (attempt %d): %v", attempt, err)
		}

		released := LockFile{
			Status:      "released",
			Owner:       "",
			AcquiredAt:  "",
			HeartbeatAt: "",
		}
		if err := m.writeLockFile(syncDir, lockFile, &released); err != nil {
			m.log("Error writing released lock file (attempt %d): %v", attempt, err)
			continue
		}

		if err := m.gitCommitAndPush(syncDir, lockFile, fmt.Sprintf("Release %s", lockFile)); err != nil {
			m.log("Error pushing lock release (attempt %d): %v", attempt, err)
			// Reset and retry
			m.gitReset(syncDir)
			continue
		}

		m.heldLocksMu.Lock()
		delete(m.heldLocks, agentType)
		m.heldLocksMu.Unlock()
		m.log("Released %s lock", lockFile)
		return
	}

	m.log("ERROR: Failed to release %s lock after 3 attempts — will be detected as stale", lockFile)
	m.heldLocksMu.Lock()
	delete(m.heldLocks, agentType)
	m.heldLocksMu.Unlock()
}

// ReleaseAllLocks releases all currently held locks (for forced shutdown).
func (m *Manager) ReleaseAllLocks() {
	m.heldLocksMu.Lock()
	held := make([]agent.AgentType, 0, len(m.heldLocks))
	for at := range m.heldLocks {
		held = append(held, at)
	}
	m.heldLocksMu.Unlock()

	for _, at := range held {
		m.StopHeartbeat(at)
		m.ReleaseLock(at)
	}
}

// --- Internal methods ---

func (m *Manager) log(format string, args ...interface{}) {
	if m.logFunc != nil {
		m.logFunc("[sync] "+format, args...)
	}
}

func (m *Manager) createSyncRepo(syncDir string) error {
	if err := os.MkdirAll(syncDir, 0755); err != nil {
		return err
	}

	// git init
	if err := gitCmd(syncDir, "init", "-b", "main"); err != nil {
		return fmt.Errorf("git init sync repo: %w", err)
	}

	// Create lock files with released-state YAML
	for _, name := range []string{"architect.lock.yaml", "solver.lock.yaml", "evaluator.lock.yaml", "reviewer.lock.yaml"} {
		path := filepath.Join(syncDir, name)
		released := LockFile{
			Status:      "released",
			Owner:       "",
			AcquiredAt:  "",
			HeartbeatAt: "",
		}
		data, _ := yaml.Marshal(&released)
		if err := os.WriteFile(path, data, 0644); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}

	// Initial commit
	if err := gitCmd(syncDir, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if err := gitCmd(syncDir, "commit", "-m", "Initialize synchronization repository"); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	m.log("Created synchronization repository")

	// Try to push if a remote is configured
	if hasRemote(syncDir) {
		if err := gitCmd(syncDir, "push", "-u", "origin", "main"); err != nil {
			m.log("Could not push initial sync commit (remote may not be configured): %v", err)
		}
	}

	return nil
}

func (m *Manager) lockFileName(agentType agent.AgentType) string {
	return string(agentType) + ".lock.yaml"
}

type contentionError struct {
	msg string
}

func (e *contentionError) Error() string { return e.msg }

func isContention(err error) bool {
	_, ok := err.(*contentionError)
	return ok
}

func (m *Manager) tryAcquireLock(agentType agent.AgentType, lockFile string) error {
	syncDir := filepath.Join(m.baseDir, "synchronization")

	// 1. Pull
	if err := m.gitPullSafe(syncDir); err != nil {
		// Non-fatal for local-only repos (no remote)
		if !hasRemote(syncDir) {
			// No remote — just proceed
		} else {
			return fmt.Errorf("pull sync repo: %w", err)
		}
	}

	// 2. Read lock file
	lock, err := m.readLockFile(syncDir, lockFile)
	if err != nil {
		return fmt.Errorf("read lock file: %w", err)
	}

	// 3. Check lock status
	if lock != nil && lock.Status == "acquired" {
		if lock.Owner == m.identity {
			// Re-acquire our own lock
		} else {
			// Check staleness
			stale := m.isStale(lock)
			if !stale {
				return &contentionError{
					msg: fmt.Sprintf("Waiting for %s held by %s, last heartbeat %s",
						lockFile, lock.Owner, lock.HeartbeatAt),
				}
			}
			m.log("Taking over stale %s (owner: %s, heartbeat: %s)", lockFile, lock.Owner, lock.HeartbeatAt)
		}
	}

	// 4. Write lock
	now := time.Now().UTC().Format(time.RFC3339)
	newLock := LockFile{
		Status:      "acquired",
		Owner:       m.identity,
		AcquiredAt:  now,
		HeartbeatAt: now,
	}
	if err := m.writeLockFile(syncDir, lockFile, &newLock); err != nil {
		return fmt.Errorf("write lock: %w", err)
	}

	// 5. Commit
	// 6. Push
	if err := m.gitCommitAndPush(syncDir, lockFile, fmt.Sprintf("Acquire %s", lockFile)); err != nil {
		// 7. Push failed — race condition; reset and signal retry via contention error
		m.gitReset(syncDir)
		return &contentionError{
			msg: fmt.Sprintf("Push race for %s — will retry", lockFile),
		}
	}

	return nil
}

func (m *Manager) isStale(lock *LockFile) bool {
	if lock.HeartbeatAt == "" {
		return true
	}
	hb, err := time.Parse(time.RFC3339, lock.HeartbeatAt)
	if err != nil {
		return true
	}
	return time.Since(hb) > m.cfg.GetStaleThreshold()
}

func (m *Manager) readLockFile(syncDir, lockFile string) (*LockFile, error) {
	path := filepath.Join(syncDir, lockFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil // empty file = available
	}
	var lock LockFile
	if err := yaml.Unmarshal(data, &lock); err != nil {
		return nil, nil // malformed = treat as available
	}
	return &lock, nil
}

func (m *Manager) writeLockFile(syncDir, lockFile string, lock *LockFile) error {
	data, err := yaml.Marshal(lock)
	if err != nil {
		return err
	}
	path := filepath.Join(syncDir, lockFile)
	return os.WriteFile(path, data, 0644)
}

func (m *Manager) gitCommitAndPush(dir, file, message string) error {
	if err := gitCmd(dir, "add", file); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if err := gitCmd(dir, "commit", "-m", message); err != nil {
		// "nothing to commit" means the file content is identical to what's
		// already committed — treat this as success rather than an error.
		if isNothingToCommit(err) {
			return nil
		}
		return fmt.Errorf("git commit: %w", err)
	}
	if hasRemote(dir) {
		if err := gitCmd(dir, "push"); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
	}
	return nil
}

func (m *Manager) gitPullSafe(dir string) error {
	if !hasRemote(dir) {
		return nil
	}
	err := gitCmd(dir, "pull")
	if err != nil {
		// Try reset and pull again
		m.gitReset(dir)
		return gitCmd(dir, "pull")
	}
	return nil
}

func (m *Manager) gitReset(dir string) {
	// Try origin/HEAD first, fall back to HEAD~1
	if hasRemote(dir) {
		// Determine the current branch
		branch, err := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
		if err == nil {
			branch = strings.TrimSpace(branch)
			if gitCmd(dir, "reset", "--hard", "origin/"+branch) == nil {
				return
			}
		}
	}
	gitCmd(dir, "reset", "--hard", "HEAD~1")
}

func (m *Manager) sendHeartbeat(agentType agent.AgentType) {
	lockFile := m.lockFileName(agentType)
	syncDir := filepath.Join(m.baseDir, "synchronization")

	for attempt := 1; attempt <= 3; attempt++ {
		if err := m.gitPullSafe(syncDir); err != nil {
			m.log("Heartbeat pull failed for %s (attempt %d): %v", lockFile, attempt, err)
			continue
		}

		// Read and verify ownership
		lock, err := m.readLockFile(syncDir, lockFile)
		if err != nil || lock == nil {
			m.log("Heartbeat: could not read %s (attempt %d): %v", lockFile, attempt, err)
			continue
		}
		if lock.Owner != m.identity {
			m.log("ERROR: Lock %s was stolen! Owner is now %s", lockFile, lock.Owner)
			return
		}

		// Update heartbeat
		lock.HeartbeatAt = time.Now().UTC().Format(time.RFC3339)
		if err := m.writeLockFile(syncDir, lockFile, lock); err != nil {
			m.log("Heartbeat: write failed for %s (attempt %d): %v", lockFile, attempt, err)
			continue
		}

		if err := m.gitCommitAndPush(syncDir, lockFile, fmt.Sprintf("Heartbeat %s", lockFile)); err != nil {
			m.log("Heartbeat push failed for %s (attempt %d): %v", lockFile, attempt, err)
			m.gitReset(syncDir)
			continue
		}

		return // success
	}

	m.log("WARNING: Heartbeat failed after 3 attempts for %s — continuing anyway", lockFile)
}

func (m *Manager) isDirty(repoDir string) (bool, error) {
	out, err := gitOutput(repoDir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func (m *Manager) runRecoveryAgent(agentType agent.AgentType, repoDir string, callerOpts *agent.StartOptions) {
	prompt := "Your previous session ended with uncommitted changes in this repository. Please review the current state of your working directory (use git status and git diff), and commit all pending changes with an appropriate commit message. Do not make any new changes — only commit what is already there."

	opts := &agent.StartOptions{
		BaseDir: m.baseDir,
		LogFunc: m.logFunc,
	}

	// Forward TUI callbacks from the caller so that recovery session
	// token usage, tool use counts, and log lines are tracked.
	if callerOpts != nil {
		opts.OnTokens = callerOpts.OnTokens
		opts.OnToolUse = callerOpts.OnToolUse
		opts.OnLogEntry = callerOpts.OnLogEntry
		opts.OnSessionID = callerOpts.OnSessionID
	}

	if err := agent.StartAgent(agentType, repoDir, prompt, opts); err != nil {
		m.log("Recovery agent for %s failed: %v", agentType, err)
	}
}

func (m *Manager) pushAgentRepo(repoDir string, agentType agent.AgentType) error {
	if !hasRemote(repoDir) {
		return nil
	}
	for attempt := 1; attempt <= 3; attempt++ {
		err := gitCmd(repoDir, "push")
		if err == nil {
			m.log("Pushed %s repo", agentType)
			return nil
		}
		m.log("Push failed for %s (attempt %d): %v — trying pull --rebase", agentType, attempt, err)
		gitCmd(repoDir, "pull", "--rebase")
	}
	return fmt.Errorf("push failed after 3 attempts")
}

// --- Green CI methods ---

// IsGreenCI returns true if Green CI is enabled for the given agent type.
func (m *Manager) IsGreenCI(agentType agent.AgentType) bool {
	return m.cfg.IsGreenCI(string(agentType))
}

// CreateFeatureBranch creates and switches to a feature branch for Green CI.
// Must be called after lock acquisition and agent repo pull.
// Returns the feature branch name.
func (m *Manager) CreateFeatureBranch(agentType agent.AgentType) (string, error) {
	repoDir := filepath.Join(m.baseDir, agent.RepoDir(agentType))
	trunk := m.cfg.GetTrunkBranch(string(agentType))

	// Ensure we're on the trunk branch
	currentBranch, err := gitOutput(repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("get current branch: %w", err)
	}
	currentBranch = strings.TrimSpace(currentBranch)
	if currentBranch != trunk {
		if err := gitCmd(repoDir, "checkout", trunk); err != nil {
			return "", fmt.Errorf("checkout trunk %s: %w", trunk, err)
		}
		if err := m.gitPullSafe(repoDir); err != nil {
			m.log("Warning: pull after trunk checkout failed: %v", err)
		}
	}

	// Create feature branch name: <identity>/<YYYY_MM_DDTHH_MM_SSZ>
	now := time.Now().UTC()
	dateTime := fmt.Sprintf("%04d_%02d_%02dT%02d_%02d_%02dZ",
		now.Year(), now.Month(), now.Day(),
		now.Hour(), now.Minute(), now.Second())
	branchName := m.identity + "/" + dateTime

	if err := gitCmd(repoDir, "checkout", "-b", branchName); err != nil {
		return "", fmt.Errorf("create feature branch %s: %w", branchName, err)
	}

	m.log("Created feature branch %s for %s", branchName, agentType)
	return branchName, nil
}

// GreenCIResult represents the outcome of the Green CI flow.
type GreenCIResult struct {
	TreatAsFailed bool   // true if the request should be treated as failed
	PRNumber      int    // the PR number (for recovery tracking)
	FeatureBranch string // the feature branch name
}

// PostAgentGreenCI performs the Green CI post-agent workflow:
// push feature branch, create PR/MR, validate, merge or recover.
// Returns whether the agent should be treated as failed.
// The caller must handle attempt tracking and retry logic.
func (m *Manager) PostAgentGreenCI(
	ctx context.Context,
	agentType agent.AgentType,
	agentExitErr error,
	recoveryOpts *agent.StartOptions,
	featureBranch string,
	currentAttempts int,
	maxAttempts int,
) (treatAsFailed bool) {
	m.StopHeartbeat(agentType)

	repoDir := filepath.Join(m.baseDir, agent.RepoDir(agentType))
	trunk := m.cfg.GetTrunkBranch(string(agentType))
	provider := m.cfg.GreenCIProvider(string(agentType))

	// Porcelain check + commit recovery
	dirty, err := m.isDirty(repoDir)
	if err != nil {
		m.log("Error checking porcelain for %s: %v", agentType, err)
	}

	porcelainFailed := false
	if dirty {
		recovered := false
		for attempt := 1; attempt <= 3; attempt++ {
			m.log("Commit recovery attempt %d/3 for %s", attempt, agentType)
			m.runRecoveryAgent(agentType, repoDir, recoveryOpts)
			dirty, err = m.isDirty(repoDir)
			if err != nil {
				m.log("Error checking porcelain after recovery %d for %s: %v", attempt, agentType, err)
				continue
			}
			if !dirty {
				recovered = true
				m.log("Commit recovery succeeded for %s on attempt %d", agentType, attempt)
				break
			}
		}
		if !recovered {
			porcelainFailed = true
			m.log("WARNING: Commit recovery failed after 3 attempts for %s", agentType)
		}
	}

	// Push feature branch (even if porcelain failed, push whatever committed state exists)
	if hasRemote(repoDir) {
		if err := gitCmd(repoDir, "push", "-u", "origin", featureBranch); err != nil {
			m.log("Error pushing feature branch %s: %v", featureBranch, err)
		}
	}

	// If porcelain failed, no PR/MR — release lock and fail
	if porcelainFailed {
		m.log("Porcelain failed for %s — no PR/MR created, releasing lock", agentType)
		m.ReleaseLock(agentType)
		return true
	}

	// Get the head SHA we just pushed
	pushedSHA, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		m.log("ERROR: Could not get HEAD SHA: %v", err)
		m.ReleaseLock(agentType)
		return true
	}
	pushedSHA = strings.TrimSpace(pushedSHA)

	// Restart heartbeat for validation polling
	m.StartHeartbeat(agentType)

	var success bool
	switch provider {
	case "github":
		success = m.postAgentGitHub(ctx, agentType, repoDir, pushedSHA, featureBranch, trunk, recoveryOpts, currentAttempts, maxAttempts)
	case "gitlab":
		success = m.postAgentGitLab(ctx, agentType, repoDir, pushedSHA, featureBranch, trunk, recoveryOpts, currentAttempts, maxAttempts)
	default:
		m.log("ERROR: Unknown Green CI provider %q for %s", provider, agentType)
		m.StopHeartbeat(agentType)
		m.ReleaseLock(agentType)
		return true
	}

	m.StopHeartbeat(agentType)

	if success {
		// Merge succeeded — checkout trunk, pull, release lock
		if err := gitCmd(repoDir, "checkout", trunk); err != nil {
			m.log("Error checking out trunk %s: %v", trunk, err)
		}
		if err := m.gitPullSafe(repoDir); err != nil {
			m.log("Error pulling trunk after merge: %v", err)
		}
		m.ReleaseLock(agentType)
		return false
	}

	// Validation failed and retries exhausted — switch to trunk, release lock
	if err := gitCmd(repoDir, "checkout", trunk); err != nil {
		m.log("Error checking out trunk after failure: %v", err)
	}
	m.ReleaseLock(agentType)
	return true
}

// postAgentGitHub handles the GitHub-specific Green CI flow (PR creation, validation, merge/close).
func (m *Manager) postAgentGitHub(
	ctx context.Context,
	agentType agent.AgentType,
	repoDir, pushedSHA, featureBranch, trunk string,
	recoveryOpts *agent.StartOptions,
	currentAttempts, maxAttempts int,
) bool {
	owner, repo := m.getOwnerRepo(repoDir)
	if owner == "" || repo == "" {
		m.log("ERROR: Could not determine owner/repo for %s", agentType)
		return false
	}

	ghClient := gh.NewClient()
	gci := m.cfg.GetGreenCIConfig(string(agentType))

	// Create PR
	pr, err := ghClient.CreatePR(owner, repo,
		fmt.Sprintf("Green CI: %s", featureBranch),
		featureBranch, trunk,
		fmt.Sprintf("Automated PR from bobbi Green CI for %s agent", agentType))
	if err != nil {
		m.log("ERROR: Failed to create PR: %v", err)
		return false
	}
	m.log("Created PR #%d for %s (%s -> %s)", pr.Number, agentType, featureBranch, trunk)

	success := m.validateAndMergeGitHub(ctx, agentType, ghClient, owner, repo, pr.Number, pushedSHA, gci.GitHub.RequiredChecks, featureBranch, trunk, recoveryOpts, currentAttempts, maxAttempts)

	if !success {
		m.log("Green CI validation failed for %s — closing PR #%d", agentType, pr.Number)
		if err := ghClient.ClosePR(owner, repo, pr.Number); err != nil {
			m.log("Error closing PR #%d: %v", pr.Number, err)
		}
	}
	return success
}

// postAgentGitLab handles the GitLab-specific Green CI flow (MR creation, validation, merge/close).
func (m *Manager) postAgentGitLab(
	ctx context.Context,
	agentType agent.AgentType,
	repoDir, pushedSHA, featureBranch, trunk string,
	recoveryOpts *agent.StartOptions,
	currentAttempts, maxAttempts int,
) bool {
	remoteURL, err := gitOutput(repoDir, "remote", "get-url", "origin")
	if err != nil {
		m.log("ERROR: Could not get remote URL for %s: %v", agentType, err)
		return false
	}
	projectID := gl.ProjectID(strings.TrimSpace(remoteURL))
	if projectID == "" {
		m.log("ERROR: Could not determine project ID for %s", agentType)
		return false
	}

	glClient := gl.NewClient()
	gci := m.cfg.GetGreenCIConfig(string(agentType))

	// Create MR
	mr, err := glClient.CreateMR(projectID,
		fmt.Sprintf("Green CI: %s", featureBranch),
		featureBranch, trunk,
		fmt.Sprintf("Automated MR from bobbi Green CI for %s agent", agentType))
	if err != nil {
		m.log("ERROR: Failed to create MR: %v", err)
		return false
	}
	m.log("Created MR !%d for %s (%s -> %s)", mr.IID, agentType, featureBranch, trunk)

	success := m.validateAndMergeGitLab(ctx, agentType, glClient, projectID, mr.IID, pushedSHA, gci.GitLab.RequiredJobs, featureBranch, trunk, recoveryOpts, currentAttempts, maxAttempts)

	if !success {
		m.log("Green CI validation failed for %s — closing MR !%d", agentType, mr.IID)
		if err := glClient.CloseMR(projectID, mr.IID); err != nil {
			m.log("Error closing MR !%d: %v", mr.IID, err)
		}
	}
	return success
}

// validateAndMergeGitHub polls the PR for validation and handles recovery on failure.
func (m *Manager) validateAndMergeGitHub(
	ctx context.Context,
	agentType agent.AgentType,
	ghClient *gh.Client,
	owner, repo string,
	prNumber int,
	expectedSHA string,
	requiredChecks []string,
	featureBranch, trunk string,
	recoveryOpts *agent.StartOptions,
	currentAttempts, maxAttempts int,
) bool {
	for attempt := currentAttempts; attempt <= maxAttempts; attempt++ {
		valid, err := m.pollGitHubValidation(ctx, agentType, ghClient, owner, repo, prNumber, expectedSHA, requiredChecks)
		if err != nil {
			m.log("PR validation error for %s: %v", agentType, err)
		}

		if valid {
			_, err := ghClient.MergePR(owner, repo, prNumber)
			if err != nil {
				m.log("Error merging PR #%d: %v", prNumber, err)
				return false
			}
			m.log("Merged PR #%d for %s", prNumber, agentType)
			return true
		}

		if attempt >= maxAttempts {
			m.log("Green CI: max attempts (%d) exhausted for %s", maxAttempts, agentType)
			return false
		}

		// Run GitHub recovery agent
		m.log("Green CI: running recovery agent for %s (attempt %d/%d)", agentType, attempt+1, maxAttempts)
		repoDir := filepath.Join(m.baseDir, agent.RepoDir(agentType))

		recoveryPrompt := fmt.Sprintf(`The CI checks on your pull request failed. Your changes were pushed to a feature branch and a pull request was created, but it cannot be merged because CI validation did not pass.

Pull request number: %d
Target branch: %s

Please use the `+"`gh`"+` CLI to inspect the pull request and failed checks:
- Run `+"`gh pr view %d`"+` to see the PR status
- Run `+"`gh pr checks %d`"+` to see which checks failed
- Run `+"`gh run list --branch %s`"+` to find the workflow run
- Run `+"`gh run view <run_id> --log-failed`"+` to see the failed step logs

Diagnose the failures and fix the code so that CI passes. Commit and push your fixes to the current branch.`,
			prNumber, trunk, prNumber, prNumber, featureBranch)

		expectedSHA = m.runGreenCIRecovery(agentType, repoDir, recoveryPrompt, recoveryOpts)
	}

	return false
}

// validateAndMergeGitLab polls the MR for validation and handles recovery on failure.
func (m *Manager) validateAndMergeGitLab(
	ctx context.Context,
	agentType agent.AgentType,
	glClient *gl.Client,
	projectID string,
	mrIID int,
	expectedSHA string,
	requiredJobs []string,
	featureBranch, trunk string,
	recoveryOpts *agent.StartOptions,
	currentAttempts, maxAttempts int,
) bool {
	for attempt := currentAttempts; attempt <= maxAttempts; attempt++ {
		valid, err := m.pollGitLabValidation(ctx, agentType, glClient, projectID, mrIID, expectedSHA, requiredJobs)
		if err != nil {
			m.log("MR validation error for %s: %v", agentType, err)
		}

		if valid {
			_, err := glClient.MergeMR(projectID, mrIID)
			if err != nil {
				m.log("Error merging MR !%d: %v", mrIID, err)
				return false
			}
			m.log("Merged MR !%d for %s", mrIID, agentType)
			return true
		}

		if attempt >= maxAttempts {
			m.log("Green CI: max attempts (%d) exhausted for %s", maxAttempts, agentType)
			return false
		}

		// Run GitLab recovery agent
		m.log("Green CI: running recovery agent for %s (attempt %d/%d)", agentType, attempt+1, maxAttempts)
		repoDir := filepath.Join(m.baseDir, agent.RepoDir(agentType))

		recoveryPrompt := fmt.Sprintf(`The CI pipeline on your merge request failed. Your changes were pushed to a feature branch and a merge request was created, but it cannot be merged because CI validation did not pass.

Merge request IID: %d
Target branch: %s

Please use the `+"`glab`"+` CLI to inspect the merge request and failed jobs:
- Run `+"`glab mr view %d`"+` to see the MR status
- Run `+"`glab ci view %d`"+` to see pipeline status and failed jobs
- Run `+"`glab ci trace <job_id>`"+` to see the failed job logs

Diagnose the failures and fix the code so that CI passes. Commit and push your fixes to the current branch.`,
			mrIID, trunk, mrIID, mrIID)

		expectedSHA = m.runGreenCIRecovery(agentType, repoDir, recoveryPrompt, recoveryOpts)
	}

	return false
}

// runGreenCIRecovery runs a recovery agent session and returns the new HEAD SHA.
func (m *Manager) runGreenCIRecovery(agentType agent.AgentType, repoDir, recoveryPrompt string, recoveryOpts *agent.StartOptions) string {
	opts := &agent.StartOptions{
		BaseDir: m.baseDir,
		LogFunc: m.logFunc,
	}
	if recoveryOpts != nil {
		opts.OnTokens = recoveryOpts.OnTokens
		opts.OnToolUse = recoveryOpts.OnToolUse
		opts.OnLogEntry = recoveryOpts.OnLogEntry
		opts.OnSessionID = recoveryOpts.OnSessionID
	}

	if err := agent.StartAgent(agentType, repoDir, recoveryPrompt, opts); err != nil {
		m.log("Recovery agent for %s failed: %v", agentType, err)
	}

	// Porcelain check + commit recovery after recovery agent
	dirty, err := m.isDirty(repoDir)
	if err != nil {
		m.log("Error checking porcelain after recovery agent for %s: %v", agentType, err)
	}
	if dirty {
		recovered := false
		for recAttempt := 1; recAttempt <= 3; recAttempt++ {
			m.log("Post-recovery commit recovery %d/3 for %s", recAttempt, agentType)
			m.runRecoveryAgent(agentType, repoDir, recoveryOpts)
			dirty, err = m.isDirty(repoDir)
			if err == nil && !dirty {
				recovered = true
				break
			}
		}
		if !recovered {
			m.log("WARNING: Post-recovery commit recovery failed for %s", agentType)
		}
	}

	// Push recovery changes
	if hasRemote(repoDir) {
		if err := gitCmd(repoDir, "push"); err != nil {
			m.log("Error pushing recovery changes for %s: %v", agentType, err)
		}
	}

	// Return updated SHA
	newSHA, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(newSHA)
}

// pollGitHubValidation polls the PR until validation succeeds or fails.
func (m *Manager) pollGitHubValidation(
	ctx context.Context,
	agentType agent.AgentType,
	ghClient *gh.Client,
	owner, repo string,
	prNumber int,
	expectedSHA string,
	requiredChecks []string,
) (bool, error) {
	pollInterval := 15 * time.Second

	for {
		pr, err := ghClient.GetPR(owner, repo, prNumber)
		if err != nil {
			return false, fmt.Errorf("get PR #%d: %w", prNumber, err)
		}

		if pr.Mergeable == nil {
			m.log("PR #%d mergeable status is null, continuing to poll...", prNumber)
		} else if !*pr.Mergeable {
			m.log("PR #%d is not mergeable", prNumber)
			return false, nil
		} else if pr.Head.SHA != expectedSHA {
			m.log("PR #%d head SHA mismatch: expected %s, got %s", prNumber, expectedSHA, pr.Head.SHA)
			return false, nil
		} else {
			checkRuns, err := ghClient.GetCheckRuns(owner, repo, expectedSHA)
			if err != nil {
				return false, fmt.Errorf("get check runs: %w", err)
			}

			checkMap := make(map[string]*gh.CheckRun)
			for i := range checkRuns.CheckRuns {
				checkMap[checkRuns.CheckRuns[i].Name] = &checkRuns.CheckRuns[i]
			}

			anyPending := false
			for _, name := range requiredChecks {
				cr, ok := checkMap[name]
				if !ok {
					anyPending = true
					continue
				}
				if cr.Status != "completed" {
					anyPending = true
					continue
				}
				if cr.Conclusion != "success" {
					m.log("Check %q failed with conclusion %q", name, cr.Conclusion)
					return false, nil
				}
			}

			if anyPending {
				m.log("Some required checks still pending for PR #%d, continuing to poll...", prNumber)
			} else {
				m.log("All required checks passed for PR #%d", prNumber)
				return true, nil
			}
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// pollGitLabValidation polls the MR until validation succeeds or fails.
func (m *Manager) pollGitLabValidation(
	ctx context.Context,
	agentType agent.AgentType,
	glClient *gl.Client,
	projectID string,
	mrIID int,
	expectedSHA string,
	requiredJobs []string,
) (bool, error) {
	pollInterval := 15 * time.Second

	for {
		mr, err := glClient.GetMR(projectID, mrIID)
		if err != nil {
			return false, fmt.Errorf("get MR !%d: %w", mrIID, err)
		}

		// Merge status: checking = retry, can_be_merged = proceed, cannot_be_merged = fail
		if mr.MergeStatus == "checking" {
			m.log("MR !%d merge status is checking, continuing to poll...", mrIID)
		} else if mr.MergeStatus == "cannot_be_merged" {
			m.log("MR !%d cannot be merged", mrIID)
			return false, nil
		} else if mr.MergeStatus != "can_be_merged" {
			m.log("MR !%d unknown merge status %q, continuing to poll...", mrIID, mr.MergeStatus)
		} else if mr.SHA != expectedSHA {
			m.log("MR !%d head SHA mismatch: expected %s, got %s", mrIID, expectedSHA, mr.SHA)
			return false, nil
		} else {
			// Get pipeline for the SHA
			pipeline, err := glClient.GetPipelineForSHA(projectID, expectedSHA)
			if err != nil {
				return false, fmt.Errorf("get pipeline for SHA %s: %w", expectedSHA, err)
			}
			if pipeline == nil {
				m.log("No pipeline found for SHA %s, continuing to poll...", expectedSHA)
			} else {
				// Get pipeline jobs
				jobs, err := glClient.GetPipelineJobs(projectID, pipeline.ID)
				if err != nil {
					return false, fmt.Errorf("get pipeline jobs: %w", err)
				}

				jobMap := make(map[string]*gl.PipelineJob)
				for i := range jobs {
					jobMap[jobs[i].Name] = &jobs[i]
				}

				anyPending := false
				terminalFailure := map[string]bool{"failed": true, "canceled": true}
				pendingStatus := map[string]bool{"pending": true, "running": true, "created": true}

				for _, name := range requiredJobs {
					job, ok := jobMap[name]
					if !ok {
						anyPending = true
						continue
					}
					if job.Status == "success" {
						continue
					}
					if terminalFailure[job.Status] {
						m.log("Job %q failed with status %q", name, job.Status)
						return false, nil
					}
					if pendingStatus[job.Status] {
						anyPending = true
						continue
					}
					// Unknown status — treat as pending
					anyPending = true
				}

				if anyPending {
					m.log("Some required jobs still pending for MR !%d, continuing to poll...", mrIID)
				} else {
					m.log("All required jobs passed for MR !%d", mrIID)
					return true, nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// getOwnerRepo extracts the owner and repo from the agent's remote URL.
func (m *Manager) getOwnerRepo(repoDir string) (string, string) {
	remoteURL, err := gitOutput(repoDir, "remote", "get-url", "origin")
	if err != nil {
		return "", ""
	}
	return gh.OwnerRepo(strings.TrimSpace(remoteURL))
}

// --- Git helpers ---

// isNothingToCommit checks if a git commit error indicates there was nothing
// to commit (file content identical to what's already committed).
func isNothingToCommit(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "nothing to commit")
}

func gitCmd(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s (%w)", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func hasRemote(dir string) bool {
	out, err := gitOutput(dir, "remote")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}
