package syncmgr

import (
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
	repoDir := agent.RepoDir(agentType)
	return m.cfg.IsSyncedAgent(repoDir)
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
// Blocks until the lock is acquired (with exponential backoff on contention).
func (m *Manager) AcquireLock(agentType agent.AgentType) error {
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
			time.Sleep(backoff)
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
		// If pull fails, release lock
		m.ReleaseLock(agentType)
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
func (m *Manager) PostAgentSync(agentType agent.AgentType, agentExitErr error) bool {
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
			m.runRecoveryAgent(agentType, repoDir)

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

	// Create empty lock files
	for _, name := range []string{"architecture.lock", "solution.lock", "evaluation.lock", "review.lock"} {
		path := filepath.Join(syncDir, name)
		if err := os.WriteFile(path, []byte(""), 0644); err != nil {
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
	return agent.RepoDir(agentType) + ".lock"
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
		// 7. Push failed — race condition
		m.gitReset(syncDir)
		return m.tryAcquireLock(agentType, lockFile) // retry from step 1
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

func (m *Manager) runRecoveryAgent(agentType agent.AgentType, repoDir string) {
	prompt := "Your previous session ended with uncommitted changes in this repository. Please review the current state of your working directory (use git status and git diff), and commit all pending changes with an appropriate commit message. Do not make any new changes — only commit what is already there."

	opts := &agent.StartOptions{
		BaseDir: m.baseDir,
		LogFunc: m.logFunc,
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

// --- Git helpers ---

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
