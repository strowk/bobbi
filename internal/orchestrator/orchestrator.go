package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bobbi/internal/agent"
	"bobbi/internal/config"
	"bobbi/internal/queue"
	"bobbi/internal/syncmgr"
)

// maxAgentRetries is the maximum number of times a failed work item will be
// retried before being permanently marked as failed (to avoid infinite loops).
const maxAgentRetries = 3

// AgentInfo holds the current state of a single agent type for display.
type AgentInfo struct {
	Status            string // "idle", "running", "queued"
	SessionID         string // Claude Code session ID (truncated for display)
	Prompt            string
	RequestType       string // e.g. "start_solver", "request_solution_change"
	AdditionalContext string // the additional_context from the request, if any
	InputTokens       int64  // tokens for the current/latest run
	OutputTokens      int64  // tokens for the current/latest run
	TotalInputTokens  int64  // cumulative tokens across all runs
	TotalOutputTokens int64  // cumulative tokens across all runs
	ToolUses          int    // total tool uses for the current/latest run
	ToolFailures      int    // tool use failures for the current/latest run
	TotalToolUses     int    // cumulative tool uses across all runs
	TotalToolFailures int    // cumulative tool use failures across all runs
	HasRun              bool
	DescriptionInjected bool               // true when agent description was prepended to prompt
	LogLines            []agent.LogLine    // all extracted log lines from JSONL stream
	SparklineData     []float64       // per-event total token values for sparkline
}

// logGroup represents a group of log lines from a single JSONL event, optionally tied to a message.id.
type logGroup struct {
	messageID string
	lines     []agent.LogLine
}

// agentLogState tracks deduplication state for an agent's log output.
type agentLogState struct {
	groups       []logGroup
	messageIndex map[string]int // message.id -> index in groups
}

// Orchestrator manages the lifecycle of agent processes.
type Orchestrator struct {
	baseDir      string
	queuesDir    string
	completedDir string
	failedDir    string
	userPrompt   string
	rawMode      bool
	NoSparklines bool

	// Per-agent work channels enforce serial execution per agent type
	channels map[agent.AgentType]chan workItem
	wg       sync.WaitGroup

	// Track which request files are already dispatched
	dispatched   map[string]bool
	dispatchedMu sync.Mutex

	// Time limit for the entire run
	timeout  time.Duration
	done     chan struct{} // closed on confirm_solution
	doneOnce sync.Once

	// shutdownCh is closed when any shutdown is initiated (SIGINT, timeout,
	// or confirm_solution). Workers check this to stop starting new batches.
	// Separate from 'done' which is only for confirm_solution.
	shutdownCh   chan struct{}
	shutdownOnce sync.Once

	// Shutdown reason: "confirm_solution", "timeout", or "interrupted"
	shutdownReason string

	// File logging
	logFile   *os.File
	logFileMu sync.Mutex

	// Retry tracking for pre-copy failures (protected by retryMu).
	// Agent process failures use the YAML-based attempts field instead.
	retryMu    sync.Mutex
	retryCount map[string]int // requestPath -> pre-copy retry count

	// Per-agent copy mutexes serialize all directory copy operations
	// targeting a given agent's repository (prevents races between
	// handleHandoffSolution goroutines and worker preCopy/postCopy).
	copyMu map[agent.AgentType]*sync.Mutex

	// State for TUI rendering (protected by mu)
	mu              sync.RWMutex
	agentInfo       map[agent.AgentType]*AgentInfo
	logState        map[agent.AgentType]*agentLogState // deduplication state per agent
	completedCount  int32                              // atomic
	failedCount     int32                              // atomic
	startTime       time.Time
	sharedMaxTokens float64 // shared maximum for sparkline normalization
	sessionCounts   map[agent.AgentType]int            // number of sessions per agent type

	// Synchronization
	syncMgr *syncmgr.Manager

	// Context cancelled on shutdown, used to interrupt blocking sync operations
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	// Agent configuration (which agents are enabled)
	cfg *config.Config
}

type workItem struct {
	request     queue.Request
	requestPath string
}

// workBatch represents one or more coalesced work items of the same request type.
type workBatch struct {
	requestType        string
	items              []workItem // ALL items (including dupes) for completion marking
	mergedContext      string     // Combined additional_context from all unique items
	itemCount          int        // Number of items after deduplication
	foldedChangeContext string    // Context from request_*_change items folded into a start_* batch
}

func New(baseDir string, userPrompt string, rawMode bool, timeout time.Duration, noSparklines bool, cfg *config.Config) *Orchestrator {
	if cfg == nil {
		cfg = &config.Config{}
	}
	info := make(map[agent.AgentType]*AgentInfo)
	cmu := make(map[agent.AgentType]*sync.Mutex)
	ls := make(map[agent.AgentType]*agentLogState)
	for _, at := range agent.AllTypes() {
		status := "idle"
		if !cfg.IsAgentEnabled(string(at)) {
			status = "disabled"
		}
		info[at] = &AgentInfo{Status: status}
		cmu[at] = &sync.Mutex{}
		ls[at] = &agentLogState{messageIndex: make(map[string]int)}
	}
	sc := make(map[agent.AgentType]int)
	for _, at := range agent.AllTypes() {
		sc[at] = 0
	}

	orch := &Orchestrator{
		baseDir:       baseDir,
		queuesDir:     filepath.Join(baseDir, ".bobbi", "queues"),
		completedDir:  filepath.Join(baseDir, ".bobbi", "completed"),
		failedDir:     filepath.Join(baseDir, ".bobbi", "failed"),
		userPrompt:    userPrompt,
		rawMode:       rawMode,
		NoSparklines:  noSparklines,
		channels:      make(map[agent.AgentType]chan workItem),
		dispatched:    make(map[string]bool),
		retryCount:    make(map[string]int),
		timeout:       timeout,
		done:          make(chan struct{}),
		shutdownCh:    make(chan struct{}),
		copyMu:        cmu,
		agentInfo:     info,
		logState:      ls,
		sessionCounts: sc,
		cfg:           cfg,
	}

	orch.syncMgr = syncmgr.New(baseDir, cfg, orch.log)

	return orch
}

// GetSharedMaxTokens returns the shared maximum token value for sparkline normalization.
func (o *Orchestrator) GetSharedMaxTokens() float64 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.sharedMaxTokens
}

// EnableFileLogging opens .bobbi/log.txt for writing log output.
func (o *Orchestrator) EnableFileLogging() error {
	logPath := filepath.Join(o.baseDir, ".bobbi", "log.txt")
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	o.logFile = f
	return nil
}

// CloseLogFile closes the log file if open.
func (o *Orchestrator) CloseLogFile() {
	o.logFileMu.Lock()
	defer o.logFileMu.Unlock()
	if o.logFile != nil {
		o.logFile.Close()
		o.logFile = nil
	}
}

// GetAgentInfo returns a deep copy of the agent info for the given type.
func (o *Orchestrator) GetAgentInfo(at agent.AgentType) AgentInfo {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if info, ok := o.agentInfo[at]; ok {
		cp := *info
		// Deep copy slices to avoid data races with concurrent appends.
		if info.LogLines != nil {
			cp.LogLines = make([]agent.LogLine, len(info.LogLines))
			copy(cp.LogLines, info.LogLines)
		}
		if info.SparklineData != nil {
			cp.SparklineData = make([]float64, len(info.SparklineData))
			copy(cp.SparklineData, info.SparklineData)
		}
		return cp
	}
	return AgentInfo{Status: "idle"}
}

// GetCompletedCount returns the number of completed requests.
func (o *Orchestrator) GetCompletedCount() int {
	return int(atomic.LoadInt32(&o.completedCount))
}

// GetFailedCount returns the number of failed requests.
func (o *Orchestrator) GetFailedCount() int {
	return int(atomic.LoadInt32(&o.failedCount))
}

// GetQueueDepth returns the number of files in the queues directory.
func (o *Orchestrator) GetQueueDepth() int {
	entries, err := os.ReadDir(o.queuesDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			count++
		}
	}
	return count
}

// GetStartTime returns when the orchestrator started.
func (o *Orchestrator) GetStartTime() time.Time {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.startTime
}

// GetTotalTokens returns aggregate cumulative token counts across all agents.
func (o *Orchestrator) GetTotalTokens() (int64, int64) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var totalIn, totalOut int64
	for _, info := range o.agentInfo {
		totalIn += info.TotalInputTokens
		totalOut += info.TotalOutputTokens
	}
	return totalIn, totalOut
}

// GetTotalToolUses returns aggregate cumulative tool use counts across all agents.
func (o *Orchestrator) GetTotalToolUses() (int, int) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var totalUses, totalFailures int
	for _, info := range o.agentInfo {
		totalUses += info.TotalToolUses
		totalFailures += info.TotalToolFailures
	}
	return totalUses, totalFailures
}

// GetShutdownReason returns the shutdown reason.
func (o *Orchestrator) GetShutdownReason() string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.shutdownReason
}

// GetSessionCounts returns a copy of the per-agent session counts.
func (o *Orchestrator) GetSessionCounts() map[agent.AgentType]int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	result := make(map[agent.AgentType]int)
	for k, v := range o.sessionCounts {
		result[k] = v
	}
	return result
}

// SolutionConfirmed returns true if confirm_solution was processed.
func (o *Orchestrator) SolutionConfirmed() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.shutdownReason == "confirm_solution"
}

// RunningAgentCount returns the number of agents currently in "running" status.
func (o *Orchestrator) RunningAgentCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	count := 0
	for _, info := range o.agentInfo {
		if info.Status == "running" {
			count++
		}
	}
	return count
}

// Done returns a channel that is closed when the orchestrator is done.
func (o *Orchestrator) Done() <-chan struct{} {
	return o.done
}

// isShuttingDown returns true if the orchestrator has begun shutting down.
// It checks both shutdownCh (SIGINT/timeout) and done (confirm_solution)
// to close the race window between confirm_solution closing done and the
// main loop propagating to shutdownCh.
func (o *Orchestrator) isShuttingDown() bool {
	select {
	case <-o.shutdownCh:
		return true
	case <-o.done:
		return true
	default:
		return false
	}
}

// isAgentEnabled returns true if the given agent type is enabled in configuration.
func (o *Orchestrator) isAgentEnabled(agentType agent.AgentType) bool {
	return o.cfg.IsAgentEnabled(string(agentType))
}

func (o *Orchestrator) log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if o.rawMode {
		fmt.Fprintf(os.Stderr, "[bobbi] %s\n", msg)
	}
	o.writeLogLine("orchestrator", msg)
}

// writeLogLine writes a timestamped line to the log file.
func (o *Orchestrator) writeLogLine(source, content string) {
	o.logFileMu.Lock()
	defer o.logFileMu.Unlock()
	if o.logFile == nil {
		return
	}
	content = strings.TrimRight(content, "\n")
	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(o.logFile, "%s [%s] %s\n", ts, source, content)
}

// updateAgentClaudeMD updates the auto-managed <this_agent_description> blocks
// in each agent's CLAUDE.md file and commits the changes.
func (o *Orchestrator) updateAgentClaudeMD() {
	updated, err := agent.UpdateClaudeMDFiles(o.baseDir)
	if err != nil {
		o.log("Error updating agent CLAUDE.md files: %v", err)
		return
	}
	if len(updated) == 0 {
		return
	}

	o.log("Updated CLAUDE.md for: %v", updated)

	// Commit changes in the affected agent git repos
	for _, at := range updated {
		repoDir := filepath.Join(o.baseDir, agent.RepoDir(at))
		cmd := exec.Command("git", "add", ".claude/CLAUDE.md")
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			o.log("Error staging CLAUDE.md in %s: %v (%s)", at, err, string(out))
			continue
		}
		cmd = exec.Command("git", "diff", "--cached", "--quiet")
		cmd.Dir = repoDir
		if err := cmd.Run(); err != nil {
			// There are staged changes — commit them
			cmd = exec.Command("git", "commit", "-m", "Update auto-managed CLAUDE.md block")
			cmd.Dir = repoDir
			if out, err := cmd.CombinedOutput(); err != nil {
				o.log("Error committing CLAUDE.md in %s: %v (%s)", at, err, string(out))
			}
		}
	}
}

func (o *Orchestrator) Run(ctx context.Context) error {
	// Always signal done when Run exits, so the TUI can detect it and quit.
	defer o.doneOnce.Do(func() { close(o.done) })

	o.mu.Lock()
	o.startTime = time.Now()
	o.mu.Unlock()

	// Create a context that is cancelled on shutdown, used to interrupt
	// blocking sync operations (AcquireLock, pollPRValidation).
	o.shutdownCtx, o.shutdownCancel = context.WithCancel(ctx)
	defer o.shutdownCancel()

	// Step 2: Synchronization setup (when enabled)
	if o.syncMgr.Enabled() {
		if err := o.syncMgr.Setup(); err != nil {
			return fmt.Errorf("sync setup: %w", err)
		}
		defer o.syncMgr.Cleanup()
	}

	// Clean up stale MCP config files from previous runs
	o.cleanupMCPConfigs()
	defer o.cleanupMCPConfigs()

	// Step 3: Update agent CLAUDE.md files (auto-managed blocks)
	o.updateAgentClaudeMD()

	// Step 4: Log agent configuration
	for _, at := range agent.AllTypes() {
		if !o.isAgentEnabled(at) {
			o.log("Agent %s is disabled via configuration", at)
		}
	}

	// Apply time limit (0 means no timeout)
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}

	// Create per-agent workers
	for _, at := range agent.AllTypes() {
		ch := make(chan workItem, 100)
		o.channels[at] = ch
		o.wg.Add(1)
		go o.worker(at, ch)
	}

	// Bootstrap: if queue is empty, enqueue start_architect
	requests, _, err := queue.ReadRequests(o.queuesDir, o.log)
	if err == nil && len(requests) == 0 {
		o.log("No pending requests, bootstrapping with start_architect")
		from := "orchestrator"
		if o.userPrompt != "" {
			from = "user"
		}
		if _, err := queue.WriteRequest(o.queuesDir, "start_architect", from, o.userPrompt); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	} else if len(requests) > 0 {
		// Apply cross-agent-type sanitization on startup queue
		o.sanitizeStartupQueue()
	}

	if o.timeout > 0 {
		o.log("Time limit: %s", o.timeout)
	} else {
		o.log("Time limit: none (disabled)")
	}

	// Poll loop
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Immediate poll
	o.poll()

	for {
		select {
		case <-o.done:
			o.log("Solution confirmed, shutting down...")
			o.mu.Lock()
			if o.shutdownReason == "" {
				o.shutdownReason = "confirm_solution"
			}
			o.mu.Unlock()
			o.shutdown()
			return nil
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				o.log("Time limit of %s reached, shutting down...", o.timeout)
				o.mu.Lock()
				o.shutdownReason = "timeout"
				o.mu.Unlock()
			} else {
				o.log("Shutting down orchestrator...")
				o.mu.Lock()
				o.shutdownReason = "interrupted"
				o.mu.Unlock()
			}
			o.shutdown()
			return nil
		case <-ticker.C:
			o.poll()
		}
	}
}

func (o *Orchestrator) shutdown() {
	o.shutdownOnce.Do(func() {
		close(o.shutdownCh)
		if o.shutdownCancel != nil {
			o.shutdownCancel()
		}
		for _, ch := range o.channels {
			close(ch)
		}
	})
	o.wg.Wait()
}

// cleanupMCPConfigs removes stale MCP config files from .bobbi/.
func (o *Orchestrator) cleanupMCPConfigs() {
	pattern := filepath.Join(o.baseDir, ".bobbi", "mcp-config-*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, f := range matches {
		os.Remove(f)
	}
}

// sanitizeStartupQueue applies cross-agent-type sanitization rules to the
// startup queue:
// - Rules 1 & 2: When request_solution_change is present, drops stale
//   start_evaluator/start_reviewer (without additional_context) and handoff_solution.
// - Rule 3: When confirm_solution is present alongside any other request with
//   non-empty additional_context, drops confirm_solution to prevent immediate
//   shutdown when there is pending meaningful work.
func (o *Orchestrator) sanitizeStartupQueue() {
	requests, paths, err := queue.ReadRequests(o.queuesDir, o.log)
	if err != nil || len(requests) == 0 {
		return
	}

	// Pre-scan: determine which rules are applicable
	hasRequestSolutionChange := false
	hasConfirmSolution := false
	confirmSolutionIdx := -1
	hasOtherWithContext := false
	for i, req := range requests {
		switch req.Request.Type {
		case "request_solution_change":
			hasRequestSolutionChange = true
		case "confirm_solution":
			hasConfirmSolution = true
			confirmSolutionIdx = i
		}
	}

	// Check if any non-confirm_solution request has non-empty additional_context
	if hasConfirmSolution {
		for i, req := range requests {
			if i != confirmSolutionIdx && req.Request.AdditionalContext != "" {
				hasOtherWithContext = true
				break
			}
		}
	}

	if !hasRequestSolutionChange && !(hasConfirmSolution && hasOtherWithContext) {
		return
	}

	dropped := make(map[int]bool)

	// Rules 1 & 2: only apply when request_solution_change is present
	if hasRequestSolutionChange {
		for i, req := range requests {
			reqType := req.Request.Type

			// Rule 1: request_solution_change supersedes start_evaluator/start_reviewer without additional_context
			if (reqType == "start_evaluator" || reqType == "start_reviewer") && req.Request.AdditionalContext == "" {
				o.log("Startup sanitization: dropping %s (from %s) — superseded by request_solution_change", reqType, req.Request.From)
				if err := queue.MarkCompleted(paths[i], o.completedDir); err != nil {
					o.log("Error marking completed during sanitization: %v", err)
				}
				atomic.AddInt32(&o.completedCount, 1)
				dropped[i] = true
				continue
			}

			// Rule 2: request_solution_change supersedes handoff_solution
			if reqType == "handoff_solution" {
				o.log("Startup sanitization: dropping %s (from %s) — superseded by request_solution_change", reqType, req.Request.From)
				if err := queue.MarkCompleted(paths[i], o.completedDir); err != nil {
					o.log("Error marking completed during sanitization: %v", err)
				}
				atomic.AddInt32(&o.completedCount, 1)
				dropped[i] = true
				continue
			}
		}
	}

	// Rule 3: confirm_solution is dropped when other requests have additional_context
	if hasConfirmSolution && hasOtherWithContext && !dropped[confirmSolutionIdx] {
		o.log("Startup sanitization: dropping confirm_solution (from %s) — other requests have additional_context", requests[confirmSolutionIdx].Request.From)
		if err := queue.MarkCompleted(paths[confirmSolutionIdx], o.completedDir); err != nil {
			o.log("Error marking completed during sanitization: %v", err)
		}
		atomic.AddInt32(&o.completedCount, 1)
		dropped[confirmSolutionIdx] = true
	}

	if len(dropped) > 0 {
		o.log("Startup sanitization: dropped %d stale request(s)", len(dropped))
	}
}

func (o *Orchestrator) poll() {
	requests, paths, err := queue.ReadRequests(o.queuesDir, o.log)
	if err != nil {
		o.log("Error reading queues: %v", err)
		return
	}

	for i, req := range requests {
		o.dispatchedMu.Lock()
		if o.dispatched[paths[i]] {
			o.dispatchedMu.Unlock()
			continue
		}
		o.dispatched[paths[i]] = true
		o.dispatchedMu.Unlock()

		item := workItem{request: req, requestPath: paths[i]}
		reqType := req.Request.Type

		switch reqType {
		case "handoff_solution":
			o.log("Processing %s request (from: %s) directly", reqType, req.Request.From)
			o.wg.Add(1)
			go o.handleDirectRequest(item, o.handleHandoffSolution)
			continue
		case "confirm_solution":
			o.log("Processing %s request (from: %s) directly", reqType, req.Request.From)
			// Process synchronously so done is closed before we dispatch anything else.
			// This ensures no new agent sessions are started after confirm_solution
			// (CONTRACT Section 6.5, step 4a).
			o.handleConfirmSolution(item)
			o.dispatchedMu.Lock()
			delete(o.dispatched, item.requestPath)
			o.dispatchedMu.Unlock()
			// Return immediately — stop dispatching. Undispatched requests
			// remain in .bobbi/queues/ for the next run (CONTRACT Section 6.5, step 5).
			return
		}

		targetAgent := o.routeRequest(reqType)
		if targetAgent == "" {
			o.log("Unknown request type: %s", reqType)
			continue
		}

		// Skip requests targeting disabled agents
		if !o.isAgentEnabled(targetAgent) {
			o.log("Skipping %s request (from: %s) — %s agent is disabled", reqType, req.Request.From, targetAgent)
			if err := queue.MarkCompleted(paths[i], o.completedDir); err != nil {
				o.log("Error marking completed: %v", err)
			}
			atomic.AddInt32(&o.completedCount, 1)
			continue
		}

		// Mark agent as queued if it's idle
		o.mu.Lock()
		if info, ok := o.agentInfo[targetAgent]; ok && info.Status == "idle" {
			info.Status = "queued"
		}
		o.mu.Unlock()

		if ch, ok := o.channels[targetAgent]; ok {
			select {
			case ch <- item:
			default:
				o.log("Channel buffer full for %s agent, deferring request %s to next poll", targetAgent, item.requestPath)
				o.dispatchedMu.Lock()
				delete(o.dispatched, item.requestPath)
				o.dispatchedMu.Unlock()
			}
		}
	}
}

// handleDirectRequest runs a direct handler (handoff/confirm) in a goroutine,
// completing the request and cleaning up the dispatched entry when done.
// If the handler returns an error, the request is retried using in-memory
// retry tracking (similar to handlePreCopyFailure) or moved to failed/.
//
// NOTE: Retry counts are tracked in-memory (retryCount map), not via the YAML
// attempts field used for agent process retries. This means handoff retry counts
// do not survive an orchestrator restart. This is acceptable because handoff is
// a fast orchestrator-internal operation (file copy), not a long-running agent
// session, so surviving restarts is less critical than for agent retries.
func (o *Orchestrator) handleDirectRequest(item workItem, handler func() error) {
	defer o.wg.Done()
	if err := handler(); err != nil {
		o.log("Direct request handler failed: %v", err)

		o.retryMu.Lock()
		o.retryCount[item.requestPath]++
		exhausted := o.retryCount[item.requestPath] >= maxAgentRetries
		if exhausted {
			delete(o.retryCount, item.requestPath)
		}
		o.retryMu.Unlock()

		if exhausted {
			o.log("Handoff failed %d times, giving up", maxAgentRetries)
			if err := queue.MarkFailed(item.requestPath, o.failedDir); err != nil {
				o.log("Error marking failed: %v", err)
			}
			atomic.AddInt32(&o.failedCount, 1)
		} else {
			o.log("Will retry handoff on next poll cycle")
		}
		o.dispatchedMu.Lock()
		delete(o.dispatched, item.requestPath)
		o.dispatchedMu.Unlock()
		return
	}

	// Success — clean up retry count and mark completed
	o.retryMu.Lock()
	delete(o.retryCount, item.requestPath)
	o.retryMu.Unlock()

	if err := queue.MarkCompleted(item.requestPath, o.completedDir); err != nil {
		o.log("Error marking completed: %v", err)
	}
	atomic.AddInt32(&o.completedCount, 1)
	o.dispatchedMu.Lock()
	delete(o.dispatched, item.requestPath)
	o.dispatchedMu.Unlock()
}

func (o *Orchestrator) routeRequest(reqType string) agent.AgentType {
	switch reqType {
	case "start_architect", "request_architecture_change":
		return agent.Architect
	case "start_solver", "request_solution_change":
		return agent.Solver
	case "start_evaluator", "request_evaluation_change":
		return agent.Evaluator
	case "start_reviewer":
		return agent.Reviewer
	}
	return ""
}

func (o *Orchestrator) worker(agentType agent.AgentType, ch <-chan workItem) {
	defer o.wg.Done()
	for {
		// Block until first item arrives (or channel closes)
		item, ok := <-ch
		if !ok {
			return
		}

		// If shutdown has been initiated, exit without starting new work.
		// Remaining queue files stay in .bobbi/queues/ for the next run.
		if o.isShuttingDown() {
			return
		}

		// Non-blocking drain of all currently-pending items
		pending := []workItem{item}
	drainLoop:
		for {
			select {
			case extra, ok := <-ch:
				if !ok {
					break drainLoop
				}
				pending = append(pending, extra)
			default:
				break drainLoop
			}
		}

		// Group by request type, apply coalescing rules
		batches := o.groupAndCoalesce(agentType, pending)

		// Process each batch in priority order
		for _, batch := range batches {
			// If shutting down, skip remaining batches.
			// Items stay in .bobbi/queues/ — they were never dispatched to an agent.
			if o.isShuttingDown() {
				o.log("Skipping %s batch (shutting down)", agentType)
				continue
			}
			o.processBatch(agentType, batch)
		}
	}
}

// groupAndCoalesce groups work items by request type and applies coalescing rules.
// start_* groups are processed first; request_*_change groups are dropped if a start_* is present.
func (o *Orchestrator) groupAndCoalesce(agentType agent.AgentType, items []workItem) []workBatch {
	// 1. Group by request type, preserving chronological order
	groups := map[string][]workItem{}
	var typeOrder []string
	for _, item := range items {
		t := item.request.Request.Type
		if _, exists := groups[t]; !exists {
			typeOrder = append(typeOrder, t)
		}
		groups[t] = append(groups[t], item)
	}

	// 2. Check for start_* vs request_*_change conflict
	hasStart := false
	for _, t := range typeOrder {
		if strings.HasPrefix(t, "start_") {
			hasStart = true
			break
		}
	}

	var batches []workBatch

	// 3. Process start_* groups first
	for _, t := range typeOrder {
		if !strings.HasPrefix(t, "start_") {
			continue
		}
		batch := o.coalesceSameType(t, groups[t])
		batches = append(batches, batch)
	}

	// 4. Process request_*_change groups
	for _, t := range typeOrder {
		if strings.HasPrefix(t, "start_") {
			continue
		}
		if hasStart {
			// Check if any change request has non-empty additional_context
			hasContext := false
			for _, item := range groups[t] {
				if item.request.Request.AdditionalContext != "" {
					hasContext = true
					break
				}
			}
			if hasContext {
				// Fold change-request context into the start_* batch prompt
				foldedContext := mergeContextAttributed(groups[t])
				if len(batches) > 0 {
					if batches[0].foldedChangeContext == "" {
						batches[0].foldedChangeContext = foldedContext
					} else {
						batches[0].foldedChangeContext += "\n\n" + foldedContext
					}
					// Add items to the start batch for completion tracking
					batches[0].items = append(batches[0].items, groups[t]...)
				}
				o.log("Folding %d %s request(s) with context into start_* session",
					len(groups[t]), t)
			} else {
				// Drop stale change requests with no context
				o.log("Dropping %d stale %s request(s) (superseded by start_* request, no context)",
					len(groups[t]), t)
				o.markItemsCompleted(groups[t])
			}
			continue
		}
		batch := o.coalesceSameType(t, groups[t])
		batches = append(batches, batch)
	}

	return batches
}

// coalesceSameType merges items of the same request type, deduplicating by (from, additional_context).
func (o *Orchestrator) coalesceSameType(requestType string, items []workItem) workBatch {
	// Deduplicate by (from, additional_context)
	seen := map[string]bool{}
	var unique []workItem
	for _, item := range items {
		key := item.request.Request.From + "\x00" + item.request.Request.AdditionalContext
		if seen[key] {
			o.log("Deduplicated identical %s from %s", requestType, item.request.Request.From)
			continue
		}
		seen[key] = true
		unique = append(unique, item)
	}

	// Merge context
	merged := mergeContext(unique)

	return workBatch{
		requestType:   requestType,
		items:         items, // Keep ALL items (including dupes) for completion marking
		mergedContext: merged,
		itemCount:     len(unique),
	}
}

// mergeContext combines the additional_context from multiple work items into a single string
// with attributed sections. For a single item, the raw context is returned (no attribution
// header) since the prompt template already provides framing.
func mergeContext(items []workItem) string {
	if len(items) == 1 {
		return items[0].request.Request.AdditionalContext
	}

	return mergeContextAttributed(items)
}

// mergeContextAttributed combines additional_context from work items into an attributed
// string with "--- Feedback N (from <agent>) ---" headers on every item, even single ones.
// Used when folding request_*_change context into start_* prompts.
func mergeContextAttributed(items []workItem) string {
	var parts []string
	counter := 0
	for _, item := range items {
		ctx := item.request.Request.AdditionalContext
		if ctx == "" {
			continue
		}
		counter++
		from := item.request.Request.From
		parts = append(parts, fmt.Sprintf(
			"--- Feedback %d (from %s) ---\n%s",
			counter, from, ctx,
		))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// handlePreCopyFailure handles retry logic for pre-copy failures. Since the agent
// was never started, YAML attempts are not incremented. Uses in-memory retry tracking.
func (o *Orchestrator) handlePreCopyFailure(batch workBatch, agentType agent.AgentType) {
	o.retryMu.Lock()
	key := batch.items[0].requestPath
	o.retryCount[key]++
	exhausted := o.retryCount[key] >= maxAgentRetries
	if exhausted {
		delete(o.retryCount, key)
	}
	o.retryMu.Unlock()

	if exhausted {
		o.log("Pre-copy failed %d times for %s agent, giving up", maxAgentRetries, agentType)
		o.markItemsFailed(batch.items)
	} else {
		o.log("Will retry %s agent on next poll cycle (pre-copy failed)", agentType)
		o.unDispatchItems(batch.items)
	}
}

// handleAgentFailure handles retry logic for agent process failures (non-zero exit code).
// Uses the YAML-based attempts field which was already incremented before starting the agent.
func (o *Orchestrator) handleAgentFailure(batch workBatch, agentType agent.AgentType, attempts int) {
	if attempts >= maxAgentRetries {
		o.log("Giving up on %s agent after %d attempts", agentType, attempts)
		o.markItemsFailed(batch.items)
	} else {
		o.log("Will retry %s agent (attempt %d/%d)", agentType, attempts, maxAgentRetries)
		o.unDispatchItems(batch.items)
	}
}

// unDispatchItems removes items from the dispatched set so they can be picked up again.
func (o *Orchestrator) unDispatchItems(items []workItem) {
	o.dispatchedMu.Lock()
	for _, item := range items {
		delete(o.dispatched, item.requestPath)
	}
	o.dispatchedMu.Unlock()
}

// markItemsCompleted moves all items' queue files to completed without processing.
func (o *Orchestrator) markItemsCompleted(items []workItem) {
	for _, item := range items {
		if err := queue.MarkCompleted(item.requestPath, o.completedDir); err != nil {
			o.log("Error marking completed: %v", err)
		}
		atomic.AddInt32(&o.completedCount, 1)
		o.dispatchedMu.Lock()
		delete(o.dispatched, item.requestPath)
		o.dispatchedMu.Unlock()
	}
}

// markItemsFailed moves all items' queue files to the failed directory.
func (o *Orchestrator) markItemsFailed(items []workItem) {
	for _, item := range items {
		if err := queue.MarkFailed(item.requestPath, o.failedDir); err != nil {
			o.log("Error marking failed: %v", err)
		}
		atomic.AddInt32(&o.failedCount, 1)
		o.dispatchedMu.Lock()
		delete(o.dispatched, item.requestPath)
		o.dispatchedMu.Unlock()
	}
}

// incrementBatchAttempts sets the attempts field for all items in the batch.
// Returns the new attempts value (max of current attempts + 1).
func (o *Orchestrator) incrementBatchAttempts(batch workBatch) int {
	maxAttempts := 0
	for _, item := range batch.items {
		if item.request.Attempts > maxAttempts {
			maxAttempts = item.request.Attempts
		}
	}
	newAttempts := maxAttempts + 1

	for _, item := range batch.items {
		if err := queue.SetAttempts(item.requestPath, newAttempts); err != nil {
			o.log("Error setting attempts for %s: %v", item.requestPath, err)
		}
	}

	return newAttempts
}

// logWriter is an io.Writer that writes each line to the orchestrator log file.
type logWriter struct {
	orch   *Orchestrator
	source string
}

func (w *logWriter) Write(p []byte) (n int, err error) {
	w.orch.writeLogLine(w.source, string(p))
	return len(p), nil
}

// mergeLogLines merges new streaming log lines into existing ones for the same message.
// In Claude Code's stream-json format, each event for the same message.id contains only
// the currently-active content block. Text and thinking blocks are progressive updates
// that replace earlier versions, while tool_use and tool_error lines accumulate.
func mergeLogLines(existing, incoming []agent.LogLine) []agent.LogLine {
	// Determine which replaceable types are in the incoming set
	newHasText := false
	newHasThinking := false
	for _, l := range incoming {
		switch l.Type {
		case agent.LogText:
			newHasText = true
		case agent.LogThinking:
			newHasThinking = true
		}
	}

	// Keep existing lines unless their type is being replaced by incoming
	var merged []agent.LogLine
	for _, l := range existing {
		if l.Type == agent.LogText && newHasText {
			continue
		}
		if l.Type == agent.LogThinking && newHasThinking {
			continue
		}
		merged = append(merged, l)
	}

	// Append all incoming lines
	merged = append(merged, incoming...)
	return merged
}

// flattenLogGroups rebuilds the flattened LogLine slice from all log groups.
func flattenLogGroups(groups []logGroup) []agent.LogLine {
	var result []agent.LogLine
	for _, g := range groups {
		result = append(result, g.lines...)
	}
	return result
}

// processBatch runs a single agent session for a coalesced batch of work items.
func (o *Orchestrator) processBatch(agentType agent.AgentType, batch workBatch) {
	if o.isShuttingDown() {
		o.log("Skipping %s agent (shutting down)", agentType)
		return
	}

	if batch.itemCount > 1 {
		o.log("Processing batch of %d %s requests for %s agent",
			batch.itemCount, batch.requestType, agentType)
	} else {
		o.log("Processing %s request for %s agent", batch.requestType, agentType)
	}

	repoDir := filepath.Join(o.baseDir, agent.RepoDir(agentType))
	taskPrompt := agent.BuildPrompt(agentType, batch.requestType, batch.mergedContext, batch.itemCount)
	// Append folded change-request context if present (start_* with superseded request_*_change)
	if batch.foldedChangeContext != "" {
		taskPrompt += "\n\nAdditionally, the following feedback was received from other agents:\n\n" + batch.foldedChangeContext
	}

	// Check if agent's CLAUDE.md has the <this_agent_description> tag.
	// If not, prepend the agent description to the prompt (fallback).
	descriptionInjected := false
	fullPrompt := taskPrompt
	if !agent.HasAgentDescriptionTag(o.baseDir, agentType) {
		descriptionInjected = true
		fullPrompt = agent.AgentInstructions(agentType) + "\n" + taskPrompt
		o.log("Agent description injected into prompt for %s (CLAUDE.md tag absent)", agentType)
	}

	// Synchronization: acquire lock and pull agent repo before starting
	isSynced := o.syncMgr.IsSynced(agentType)
	isGreenCI := o.syncMgr.IsGreenCI(agentType)
	var featureBranch string
	if isSynced {
		if err := o.syncMgr.AcquireLock(o.shutdownCtx, agentType); err != nil {
			o.log("Lock acquisition failed for %s: %v", agentType, err)
			o.handlePreCopyFailure(batch, agentType)
			return
		}
		if err := o.syncMgr.PullAgentRepo(agentType); err != nil {
			o.log("Agent repo pull failed for %s: %v", agentType, err)
			o.syncMgr.ReleaseLock(agentType)
			o.handlePreCopyFailure(batch, agentType)
			return
		}
		// Create feature branch for Green CI
		if isGreenCI {
			var err error
			featureBranch, err = o.syncMgr.CreateFeatureBranch(agentType)
			if err != nil {
				o.log("Feature branch creation failed for %s: %v", agentType, err)
				o.syncMgr.ReleaseLock(agentType)
				o.handlePreCopyFailure(batch, agentType)
				return
			}
		}
	}

	// Pre-agent: copy relevant content (once per batch)
	if err := o.preCopy(agentType); err != nil {
		o.log("Pre-copy failed for %s agent: %v", agentType, err)
		if isSynced {
			o.syncMgr.ReleaseLock(agentType)
		}
		o.handlePreCopyFailure(batch, agentType)
		return
	}

	// Increment attempts for all items (agent is about to start)
	batchAttempts := o.incrementBatchAttempts(batch)

	// Determine the additional context for display priority
	additionalCtx := batch.mergedContext
	if additionalCtx == "" && len(batch.items) > 0 {
		additionalCtx = batch.items[0].request.Request.AdditionalContext
	}

	// Update agent state to running
	o.mu.Lock()
	info := o.agentInfo[agentType]
	info.Status = "running"
	info.SessionID = ""
	info.Prompt = taskPrompt
	info.DescriptionInjected = descriptionInjected
	info.RequestType = batch.requestType
	info.AdditionalContext = additionalCtx
	info.InputTokens = 0
	info.OutputTokens = 0
	info.ToolUses = 0
	info.ToolFailures = 0
	info.LogLines = nil
	info.SparklineData = nil
	// Reset deduplication state for new session
	o.logState[agentType] = &agentLogState{messageIndex: make(map[string]int)}
	// Track session count
	o.sessionCounts[agentType]++
	o.mu.Unlock()

	// Snapshot queue files before starting the architect, so we can detect
	// new request_solution_change entries created during the session.
	var preAgentQueueFiles map[string]bool
	if agentType == agent.Architect {
		preAgentQueueFiles = o.snapshotQueueFiles()
	}

	// Start heartbeat for synchronized agents
	if isSynced {
		o.syncMgr.StartHeartbeat(agentType)
	}

	o.log("Starting %s agent", agentType)

	// Build start options
	opts := &agent.StartOptions{
		BaseDir: o.baseDir,
		OnTokens: func(input, output int64) {
			o.mu.Lock()
			info.InputTokens += input
			info.OutputTokens += output
			info.TotalInputTokens += input
			info.TotalOutputTokens += output
			// Track sparkline data: total tokens per event
			eventTotal := float64(input + output)
			info.SparklineData = append(info.SparklineData, eventTotal)
			if eventTotal > o.sharedMaxTokens {
				o.sharedMaxTokens = eventTotal
			}
			o.mu.Unlock()
		},
		OnToolUse: func(total, failures int) {
			o.mu.Lock()
			info.ToolUses += total
			info.ToolFailures += failures
			info.TotalToolUses += total
			info.TotalToolFailures += failures
			o.mu.Unlock()
		},
		OnLogEntry: func(messageID string, lines []agent.LogLine) {
			o.mu.Lock()
			ls := o.logState[agentType]
			if messageID != "" {
				if idx, ok := ls.messageIndex[messageID]; ok {
					// Streaming dedup: merge new lines with existing.
					// Each streaming event for the same message.id contains only
					// the currently-active content block, not all previous blocks.
					// Text and thinking blocks are progressive updates (replace),
					// while tool_use lines are always new (append).
					ls.groups[idx].lines = mergeLogLines(ls.groups[idx].lines, lines)
					info.LogLines = flattenLogGroups(ls.groups)
					o.mu.Unlock()
					return
				}
				ls.messageIndex[messageID] = len(ls.groups)
			}
			ls.groups = append(ls.groups, logGroup{messageID: messageID, lines: lines})
			info.LogLines = append(info.LogLines, lines...)
			o.mu.Unlock()
		},
		OnSessionID: func(sessionID string) {
			o.log("Captured session_id %s for %s agent", sessionID, agentType)
			o.mu.Lock()
			info.SessionID = sessionID
			o.mu.Unlock()
			for _, item := range batch.items {
				if err := queue.UpdateSessionID(item.requestPath, sessionID); err != nil {
					o.log("Error updating session_id in %s: %v", item.requestPath, err)
				}
			}
		},
		LogFunc: func(format string, args ...interface{}) {
			o.log(format, args...)
		},
	}

	if o.rawMode {
		opts.StdoutWriter = os.Stdout
		opts.StderrWriter = os.Stderr
	}

	// In TUI mode (or non-TTY fallback with logging), write agent output to log file
	if o.logFile != nil {
		agentLogWriter := &logWriter{orch: o, source: string(agentType)}
		if opts.StdoutWriter != nil {
			opts.StdoutWriter = io.MultiWriter(opts.StdoutWriter, agentLogWriter)
		} else {
			opts.StdoutWriter = agentLogWriter
		}
	}

	agentErr := agent.StartAgent(agentType, repoDir, fullPrompt, opts)

	// Update agent state to idle
	o.mu.Lock()
	info.Status = "idle"
	info.HasRun = true
	o.mu.Unlock()

	// Post-agent synchronization for synchronized agents
	if isSynced {
		if isGreenCI {
			treatAsFailed := o.syncMgr.PostAgentGreenCI(o.shutdownCtx, agentType, agentErr, opts, featureBranch, batchAttempts, maxAgentRetries)
			if treatAsFailed {
				o.log("Agent %s: Green CI failed, treating as failed attempt", agentType)
				o.handleAgentFailure(batch, agentType, maxAgentRetries) // exhausted
				return
			}
		} else {
			treatAsFailed := o.syncMgr.PostAgentSync(agentType, agentErr, opts)
			if treatAsFailed {
				o.log("Agent %s: sync recovery failed, treating as failed attempt", agentType)
				o.handleAgentFailure(batch, agentType, batchAttempts)
				return
			}
		}
	}

	if agentErr != nil {
		o.log("Agent %s error: %v", agentType, agentErr)
		o.handleAgentFailure(batch, agentType, batchAttempts)
		return
	}

	o.log("Agent %s finished", agentType)

	// Clear pre-copy retry counts for successfully completed items
	o.retryMu.Lock()
	for _, item := range batch.items {
		delete(o.retryCount, item.requestPath)
	}
	o.retryMu.Unlock()

	// Post-agent: copy outputs (once per batch).
	// Skip post-copy if solution was confirmed (no new work should be created).
	select {
	case <-o.done:
		// Solution confirmed — skip post-copy to avoid creating new queue files.
	default:
		o.postCopy(agentType, batch, preAgentQueueFiles)
	}

	// Mark ALL items in the batch as completed
	o.markItemsCompleted(batch.items)
}

func (o *Orchestrator) preCopy(agentType agent.AgentType) error {
	archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))

	switch agentType {
	case agent.Solver:
		o.copyMu[agent.Solver].Lock()
		defer o.copyMu[agent.Solver].Unlock()

		dst := filepath.Join(o.baseDir, agent.RepoDir(agent.Solver), "architecture")
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("remove old architecture in solver: %w", err)
		}
		if err := os.MkdirAll(dst, 0755); err != nil {
			return fmt.Errorf("create architecture dir in solver: %w", err)
		}
		if err := CopyArchitectureContract(archDir, dst); err != nil {
			return fmt.Errorf("copy architecture to solver: %w", err)
		}
	case agent.Evaluator:
		o.copyMu[agent.Evaluator].Lock()
		defer o.copyMu[agent.Evaluator].Unlock()

		dst := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "architecture")
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("remove old architecture in evaluator: %w", err)
		}
		if err := os.MkdirAll(dst, 0755); err != nil {
			return fmt.Errorf("create architecture dir in evaluator: %w", err)
		}
		if err := CopyArchitectureContract(archDir, dst); err != nil {
			return fmt.Errorf("copy architecture to evaluator: %w", err)
		}
	case agent.Reviewer:
		o.copyMu[agent.Reviewer].Lock()
		defer o.copyMu[agent.Reviewer].Unlock()

		// Copy architecture to reviewer
		dst := filepath.Join(o.baseDir, agent.RepoDir(agent.Reviewer), "architecture")
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("remove old architecture in reviewer: %w", err)
		}
		if err := os.MkdirAll(dst, 0755); err != nil {
			return fmt.Errorf("create architecture dir in reviewer: %w", err)
		}
		if err := CopyArchitectureContract(archDir, dst); err != nil {
			return fmt.Errorf("copy architecture to reviewer: %w", err)
		}

		// Also refresh solution source from solver for robustness during retries
		solverDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Solver))
		dstSolution := filepath.Join(o.baseDir, agent.RepoDir(agent.Reviewer), "solution")
		if err := os.RemoveAll(dstSolution); err != nil {
			return fmt.Errorf("remove old solution in reviewer: %w", err)
		}
		if err := os.MkdirAll(dstSolution, 0755); err != nil {
			return fmt.Errorf("create solution dir in reviewer: %w", err)
		}
		if err := CopySolutionSource(solverDir, dstSolution); err != nil {
			return fmt.Errorf("copy solution source to reviewer: %w", err)
		}
	}
	return nil
}

func (o *Orchestrator) postCopy(agentType agent.AgentType, batch workBatch, preAgentQueueFiles map[string]bool) {
	switch agentType {
	case agent.Architect:
		archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))
		copyFailed := false

		// Copy architecture to solver (always) and evaluator (only if enabled)
		targets := []agent.AgentType{agent.Solver}
		if o.isAgentEnabled(agent.Evaluator) {
			targets = append(targets, agent.Evaluator)
		} else {
			o.log("Skipping architecture copy to evaluator — evaluator is disabled")
		}
		for _, target := range targets {
			o.copyMu[target].Lock()
			dst := filepath.Join(o.baseDir, agent.RepoDir(target), "architecture")
			if err := os.RemoveAll(dst); err != nil {
				o.log("Error removing old architecture in %s: %v", target, err)
				copyFailed = true
				o.copyMu[target].Unlock()
				continue
			}
			if err := os.MkdirAll(dst, 0755); err != nil {
				o.log("Error creating architecture dir in %s: %v", target, err)
				copyFailed = true
				o.copyMu[target].Unlock()
				continue
			}
			if err := CopyArchitectureContract(archDir, dst); err != nil {
				o.log("Error copying architecture to %s: %v", target, err)
				copyFailed = true
			}
			o.copyMu[target].Unlock()
		}

		if copyFailed {
			o.log("Skipping start_solver due to architecture copy failures in postCopy")
			return
		}

		// Check if the architect created request_solution_change entries during its session.
		// If so, skip auto-enqueueing start_solver — the architect's entries will
		// trigger the solver independently via the normal queue flow.
		if o.hasNewSolverRequestsFromArchitect(preAgentQueueFiles) {
			o.log("Architect created request_solution_change entries; skipping auto start_solver")
			return
		}

		// Determine if this was a bootstrap (start_architect with no context)
		isBootstrap := batch.requestType == "start_architect" &&
			batch.mergedContext == "" &&
			batch.foldedChangeContext == ""

		var solverContext string
		if !isBootstrap {
			// Capture architect's most recent commit message
			commitMsg, err := gitCommandOutput(archDir, "log", "-1", "--format=%B")
			if err != nil {
				o.log("Error getting architect commit message: %v", err)
			}

			// Capture architect's most recent diff (fall back to empty tree
			// for single-commit repos where HEAD~1 doesn't exist).
			archDiff, err := gitCommandOutput(archDir, "diff", "HEAD~1")
			if err != nil {
				archDiff, err = gitCommandOutput(archDir, "diff", "4b825dc642b2f65e9749303d8217d414044dd1f4", "HEAD")
				if err != nil {
					o.log("Error getting architect diff: %v", err)
				}
			}

			// Determine the originator and original context
			from := "orchestrator"
			originalContext := batch.mergedContext
			if len(batch.items) > 0 {
				from = batch.items[0].request.Request.From
			}

			solverContext = fmt.Sprintf("Change trigger (%s): %s\n\nArchitect's summary of changes:\n%s\n\nDiff of architecture changes:\n%s",
				from, originalContext, strings.TrimSpace(commitMsg), archDiff)
		}

		// After architect, start solver
		if _, err := queue.WriteRequest(o.queuesDir, "start_solver", "orchestrator", solverContext); err != nil {
			o.log("Error queuing start_solver: %v", err)
		}
	}
}

// snapshotQueueFiles returns a set of filenames currently in the queues directory.
func (o *Orchestrator) snapshotQueueFiles() map[string]bool {
	entries, err := os.ReadDir(o.queuesDir)
	if err != nil {
		return map[string]bool{}
	}
	files := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files[e.Name()] = true
		}
	}
	return files
}

// hasNewSolverRequestsFromArchitect checks if any new request_solution_change
// queue files from the architect appeared since the pre-agent snapshot.
func (o *Orchestrator) hasNewSolverRequestsFromArchitect(preSnapshot map[string]bool) bool {
	reqs, paths, err := queue.ReadRequests(o.queuesDir, func(string, ...interface{}) {})
	if err != nil {
		return false
	}
	for i, req := range reqs {
		filename := filepath.Base(paths[i])
		if preSnapshot[filename] {
			continue // existed before the agent started
		}
		if req.Request.Type == "request_solution_change" && req.Request.From == "architect" {
			return true
		}
	}
	return false
}

// gitCommandOutput runs a git command in the given directory and returns stdout.
func gitCommandOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (o *Orchestrator) handleHandoffSolution() error {
	solverDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Solver))
	archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))
	evalCopyFailed := false
	reviewCopyFailed := false

	evalEnabled := o.isAgentEnabled(agent.Evaluator)
	reviewEnabled := o.isAgentEnabled(agent.Reviewer)

	// Copy evaluator targets under evaluator lock (only if evaluator is enabled)
	if evalEnabled {
		o.copyMu[agent.Evaluator].Lock()
		func() {
			defer o.copyMu[agent.Evaluator].Unlock()

			// 1. Copy solution-deliverable to evaluator
			srcDeliverable := filepath.Join(solverDir, "solution-deliverable")
			dstDeliverable := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "solution-deliverable")
			if err := os.RemoveAll(dstDeliverable); err != nil {
				o.log("Error removing old deliverable in evaluator: %v", err)
				evalCopyFailed = true
			} else if err := os.MkdirAll(dstDeliverable, 0755); err != nil {
				o.log("Error creating deliverable dir in evaluator: %v", err)
				evalCopyFailed = true
			} else if err := CopyDir(srcDeliverable, dstDeliverable); err != nil {
				o.log("Error copying deliverable to evaluator: %v", err)
				evalCopyFailed = true
			}

			// 2. Copy architecture to evaluator
			dstArch := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "architecture")
			if err := os.RemoveAll(dstArch); err != nil {
				o.log("Error removing old architecture in evaluator: %v", err)
				evalCopyFailed = true
			} else if err := os.MkdirAll(dstArch, 0755); err != nil {
				o.log("Error creating architecture dir in evaluator: %v", err)
				evalCopyFailed = true
			} else if err := CopyArchitectureContract(archDir, dstArch); err != nil {
				o.log("Error copying architecture to evaluator: %v", err)
				evalCopyFailed = true
			}
		}()
	} else {
		o.log("Skipping evaluator copy during handoff — evaluator is disabled")
	}

	// Copy reviewer targets under reviewer lock (only if reviewer is enabled)
	if reviewEnabled {
		o.copyMu[agent.Reviewer].Lock()
		func() {
			defer o.copyMu[agent.Reviewer].Unlock()

			// 3. Copy solution source to reviewer (excluding .git/, architecture/, solution-deliverable/)
			dstSolution := filepath.Join(o.baseDir, agent.RepoDir(agent.Reviewer), "solution")
			if err := os.RemoveAll(dstSolution); err != nil {
				o.log("Error removing old solution in reviewer: %v", err)
				reviewCopyFailed = true
			} else if err := os.MkdirAll(dstSolution, 0755); err != nil {
				o.log("Error creating solution dir in reviewer: %v", err)
				reviewCopyFailed = true
			} else if err := CopySolutionSource(solverDir, dstSolution); err != nil {
				o.log("Error copying solution to reviewer: %v", err)
				reviewCopyFailed = true
			}

			// 4. Copy architecture to reviewer
			dstArchReview := filepath.Join(o.baseDir, agent.RepoDir(agent.Reviewer), "architecture")
			if err := os.RemoveAll(dstArchReview); err != nil {
				o.log("Error removing old architecture in reviewer: %v", err)
				reviewCopyFailed = true
			} else if err := os.MkdirAll(dstArchReview, 0755); err != nil {
				o.log("Error creating architecture dir in reviewer: %v", err)
				reviewCopyFailed = true
			} else if err := CopyArchitectureContract(archDir, dstArchReview); err != nil {
				o.log("Error copying architecture to reviewer: %v", err)
				reviewCopyFailed = true
			}
		}()
	} else {
		o.log("Skipping reviewer copy during handoff — reviewer is disabled")
	}

	// Enqueue only enabled agents whose copies succeeded.
	if !evalEnabled {
		// Already logged above
	} else if evalCopyFailed {
		o.log("Skipping start_evaluator due to copy failures during handoff")
	} else {
		if _, err := queue.WriteRequest(o.queuesDir, "start_evaluator", "orchestrator", ""); err != nil {
			o.log("Error queuing start_evaluator: %v", err)
		}
	}
	if !reviewEnabled {
		// Already logged above
	} else if reviewCopyFailed {
		o.log("Skipping start_reviewer due to copy failures during handoff")
	} else {
		if _, err := queue.WriteRequest(o.queuesDir, "start_reviewer", "orchestrator", ""); err != nil {
			o.log("Error queuing start_reviewer: %v", err)
		}
	}

	// Only report error if an enabled agent's copy failed
	if (evalEnabled && evalCopyFailed) || (reviewEnabled && reviewCopyFailed) {
		return fmt.Errorf("some copy operations failed during handoff")
	}
	return nil
}

// ForceReleaseLocks immediately releases all held sync locks (for forced shutdown).
func (o *Orchestrator) ForceReleaseLocks() {
	o.syncMgr.ReleaseAllLocks()
}

// PrintRunSummary writes a plain text run summary to the given writer.
func (o *Orchestrator) PrintRunSummary(w io.Writer) {
	reason := o.GetShutdownReason()
	if reason == "" {
		reason = "unknown"
	}

	elapsed := time.Since(o.GetStartTime()).Truncate(time.Second)

	// Build session counts string
	sessionCounts := o.GetSessionCounts()
	var sessionParts []string
	for _, at := range agentOrder {
		if count, ok := sessionCounts[at]; ok && count > 0 {
			sessionParts = append(sessionParts, fmt.Sprintf("%s: %d", at, count))
		}
	}
	sessionsStr := "none"
	if len(sessionParts) > 0 {
		sessionsStr = strings.Join(sessionParts, ", ")
	}

	completed := o.GetCompletedCount()
	failed := o.GetFailedCount()
	remaining := o.GetQueueDepth()

	totalIn, totalOut := o.GetTotalTokens()

	solutionStatus := "not confirmed"
	if o.SolutionConfirmed() {
		solutionStatus = "confirmed"
	}

	fmt.Fprintf(w, "\n── BOBBI run summary ──────────────────────────\n")
	fmt.Fprintf(w, "Shutdown: %s\n", reason)
	fmt.Fprintf(w, "Elapsed:  %s\n", elapsed)
	fmt.Fprintf(w, "Sessions: %s\n", sessionsStr)
	fmt.Fprintf(w, "Requests: %d completed, %d failed, %d remaining\n", completed, failed, remaining)
	fmt.Fprintf(w, "Tokens:   %s input / %s output\n", formatNumber(totalIn), formatNumber(totalOut))
	fmt.Fprintf(w, "Solution: %s\n", solutionStatus)
	fmt.Fprintf(w, "────────────────────────────────────────────────\n")
}

func (o *Orchestrator) handleConfirmSolution(item workItem) {
	// Retry the copy operation up to maxAgentRetries times (CONTRACT Section 5.3).
	// confirm_solution is processed synchronously in poll() so it cannot use the
	// async handleDirectRequest path; we implement retry inline instead.
	var lastErr error
	for attempt := 1; attempt <= maxAgentRetries; attempt++ {
		lastErr = o.copyDeliverableToOutput()
		if lastErr == nil {
			break
		}
		o.log("confirm_solution copy attempt %d/%d failed: %v", attempt, maxAgentRetries, lastErr)
		if attempt < maxAgentRetries {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	if lastErr != nil {
		o.log("ERROR: Failed to copy deliverable to output after %d attempts — confirm_solution failed", maxAgentRetries)
		if err := queue.MarkFailed(item.requestPath, o.failedDir); err != nil {
			o.log("Error marking confirm_solution failed: %v", err)
		}
		atomic.AddInt32(&o.failedCount, 1)
		return
	}

	// Move confirm_solution request to completed (it has been processed)
	if err := queue.MarkCompleted(item.requestPath, o.completedDir); err != nil {
		o.log("Error marking confirm_solution completed: %v", err)
	}
	atomic.AddInt32(&o.completedCount, 1)

	// Log confirmation
	o.log("========================================")
	o.log("Solution confirmed and delivered!")
	o.log("Output available in: output/")
	o.log("========================================")

	// Signal orchestrator to enter graceful shutdown
	o.mu.Lock()
	o.shutdownReason = "confirm_solution"
	o.mu.Unlock()
	o.doneOnce.Do(func() { close(o.done) })
}

// copyDeliverableToOutput copies solution-deliverable/ to output/.
// Returns an error if any step fails, allowing the caller to retry.
func (o *Orchestrator) copyDeliverableToOutput() error {
	// When evaluator is disabled, evaluation/solution-deliverable/ is never
	// populated, so fall back to copying from solution/solution-deliverable/.
	var srcDeliverable string
	if o.isAgentEnabled(agent.Evaluator) {
		srcDeliverable = filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "solution-deliverable")
	} else {
		o.log("Evaluator is disabled — copying deliverable from solution/ instead of evaluation/")
		srcDeliverable = filepath.Join(o.baseDir, agent.RepoDir(agent.Solver), "solution-deliverable")
	}
	// Acquire evaluator copy mutex to prevent races with handleHandoffSolution
	// which writes to the same evaluator directory concurrently.
	o.copyMu[agent.Evaluator].Lock()
	defer o.copyMu[agent.Evaluator].Unlock()

	dstOutput := filepath.Join(o.baseDir, "output")
	if err := os.RemoveAll(dstOutput); err != nil {
		return fmt.Errorf("remove old output dir: %w", err)
	}
	if err := os.MkdirAll(dstOutput, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	if err := CopyDir(srcDeliverable, dstOutput); err != nil {
		return fmt.Errorf("copy deliverable to output: %w", err)
	}
	return nil
}
