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
	"bobbi/internal/queue"
)

// maxAgentRetries is the maximum number of times a failed work item will be
// retried before being permanently marked as completed (to avoid infinite loops).
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
	HasRun            bool
	LogLines          []string  // all extracted text lines from JSONL stream
	SparklineData     []float64 // per-event total token values for sparkline
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
	completedCount  int32 // atomic
	failedCount     int32 // atomic
	startTime       time.Time
	sharedMaxTokens float64 // shared maximum for sparkline normalization
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

func New(baseDir string, userPrompt string, rawMode bool, timeout time.Duration, noSparklines bool) *Orchestrator {
	info := make(map[agent.AgentType]*AgentInfo)
	cmu := make(map[agent.AgentType]*sync.Mutex)
	for _, at := range agent.AllTypes() {
		info[at] = &AgentInfo{Status: "idle"}
		cmu[at] = &sync.Mutex{}
	}
	return &Orchestrator{
		baseDir:      baseDir,
		queuesDir:    filepath.Join(baseDir, ".bobbi", "queues"),
		completedDir: filepath.Join(baseDir, ".bobbi", "completed"),
		failedDir:    filepath.Join(baseDir, ".bobbi", "failed"),
		userPrompt:   userPrompt,
		rawMode:      rawMode,
		NoSparklines: noSparklines,
		channels:     make(map[agent.AgentType]chan workItem),
		dispatched:   make(map[string]bool),
		retryCount:   make(map[string]int),
		timeout:      timeout,
		done:         make(chan struct{}),
		shutdownCh:   make(chan struct{}),
		copyMu:       cmu,
		agentInfo:    info,
	}
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
			cp.LogLines = make([]string, len(info.LogLines))
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

func (o *Orchestrator) Run(ctx context.Context) error {
	// Always signal done when Run exits, so the TUI can detect it and quit.
	defer o.doneOnce.Do(func() { close(o.done) })

	o.mu.Lock()
	o.startTime = time.Now()
	o.mu.Unlock()

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
			o.shutdown()
			return nil
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				o.log("Time limit of %s reached, shutting down...", o.timeout)
			} else {
				o.log("Shutting down orchestrator...")
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
		for _, ch := range o.channels {
			close(ch)
		}
	})
	o.wg.Wait()
}

// sanitizeStartupQueue applies cross-agent-type sanitization rules to the
// startup queue. When request_solution_change is present, it drops stale
// start_evaluator/start_reviewer (without additional_context) and handoff_solution
// requests, since evaluating/reviewing a solution that is about to change is wasteful.
func (o *Orchestrator) sanitizeStartupQueue() {
	requests, paths, err := queue.ReadRequests(o.queuesDir, o.log)
	if err != nil || len(requests) == 0 {
		return
	}

	// Check if there's at least one request_solution_change
	hasRequestSolutionChange := false
	for _, req := range requests {
		if req.Request.Type == "request_solution_change" {
			hasRequestSolutionChange = true
			break
		}
	}
	if !hasRequestSolutionChange {
		return
	}

	dropped := 0
	for i, req := range requests {
		reqType := req.Request.Type

		// Rule 1: request_solution_change supersedes start_evaluator/start_reviewer without additional_context
		if (reqType == "start_evaluator" || reqType == "start_reviewer") && req.Request.AdditionalContext == "" {
			o.log("Startup sanitization: dropping %s (from %s) — superseded by request_solution_change", reqType, req.Request.From)
			if err := queue.MarkCompleted(paths[i], o.completedDir); err != nil {
				o.log("Error marking completed during sanitization: %v", err)
			}
			atomic.AddInt32(&o.completedCount, 1)
			dropped++
			continue
		}

		// Rule 2: request_solution_change supersedes handoff_solution
		if reqType == "handoff_solution" {
			o.log("Startup sanitization: dropping %s (from %s) — superseded by request_solution_change", reqType, req.Request.From)
			if err := queue.MarkCompleted(paths[i], o.completedDir); err != nil {
				o.log("Error marking completed during sanitization: %v", err)
			}
			atomic.AddInt32(&o.completedCount, 1)
			dropped++
			continue
		}
	}

	if dropped > 0 {
		o.log("Startup sanitization: dropped %d stale request(s)", dropped)
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
func (o *Orchestrator) handleDirectRequest(item workItem, handler func()) {
	defer o.wg.Done()
	handler()
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
	prompt := agent.BuildPrompt(agentType, batch.requestType, batch.mergedContext, batch.itemCount)
	// Append folded change-request context if present (start_* with superseded request_*_change)
	if batch.foldedChangeContext != "" {
		prompt += "\n\nAdditionally, the following feedback was received from other agents:\n\n" + batch.foldedChangeContext
	}

	// Pre-agent: copy relevant content (once per batch)
	if err := o.preCopy(agentType); err != nil {
		o.log("Pre-copy failed for %s agent: %v", agentType, err)
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
	info.Prompt = prompt
	info.RequestType = batch.requestType
	info.AdditionalContext = additionalCtx
	info.InputTokens = 0
	info.OutputTokens = 0
	info.LogLines = nil
	info.SparklineData = nil
	o.mu.Unlock()

	// Snapshot queue files before starting the architect, so we can detect
	// new request_solution_change entries created during the session.
	var preAgentQueueFiles map[string]bool
	if agentType == agent.Architect {
		preAgentQueueFiles = o.snapshotQueueFiles()
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
		OnText: func(text string) {
			o.mu.Lock()
			lines := strings.Split(text, "\n")
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

	agentErr := agent.StartAgent(agentType, repoDir, prompt, opts)

	// Update agent state to idle
	o.mu.Lock()
	info.Status = "idle"
	info.HasRun = true
	o.mu.Unlock()

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
	}
	return nil
}

func (o *Orchestrator) postCopy(agentType agent.AgentType, batch workBatch, preAgentQueueFiles map[string]bool) {
	switch agentType {
	case agent.Architect:
		archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))

		// Always copy architecture to solver and evaluator repos
		for _, target := range []agent.AgentType{agent.Solver, agent.Evaluator} {
			o.copyMu[target].Lock()
			dst := filepath.Join(o.baseDir, agent.RepoDir(target), "architecture")
			if err := os.RemoveAll(dst); err != nil {
				o.log("Error removing old architecture in %s: %v", target, err)
			}
			if err := os.MkdirAll(dst, 0755); err != nil {
				o.log("Error creating architecture dir in %s: %v", target, err)
				o.copyMu[target].Unlock()
				continue
			}
			if err := CopyArchitectureContract(archDir, dst); err != nil {
				o.log("Error copying architecture to %s: %v", target, err)
			}
			o.copyMu[target].Unlock()
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

func (o *Orchestrator) handleHandoffSolution() {
	solverDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Solver))
	archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))

	// Copy evaluator targets under evaluator lock to prevent races with
	// evaluator preCopy which operates on the same directories.
	o.copyMu[agent.Evaluator].Lock()
	func() {
		defer o.copyMu[agent.Evaluator].Unlock()

		// 1. Copy solution-deliverable to evaluator
		srcDeliverable := filepath.Join(solverDir, "solution-deliverable")
		dstDeliverable := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "solution-deliverable")
		if err := os.RemoveAll(dstDeliverable); err != nil {
			o.log("Error removing old deliverable in evaluator: %v", err)
		}
		if err := os.MkdirAll(dstDeliverable, 0755); err != nil {
			o.log("Error creating deliverable dir in evaluator: %v", err)
		} else if err := CopyDir(srcDeliverable, dstDeliverable); err != nil {
			o.log("Error copying deliverable to evaluator: %v", err)
		}

		// 2. Copy architecture to evaluator
		dstArch := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "architecture")
		if err := os.RemoveAll(dstArch); err != nil {
			o.log("Error removing old architecture in evaluator: %v", err)
		}
		if err := os.MkdirAll(dstArch, 0755); err != nil {
			o.log("Error creating architecture dir in evaluator: %v", err)
		} else if err := CopyArchitectureContract(archDir, dstArch); err != nil {
			o.log("Error copying architecture to evaluator: %v", err)
		}
	}()

	// Copy reviewer targets under reviewer lock.
	o.copyMu[agent.Reviewer].Lock()
	func() {
		defer o.copyMu[agent.Reviewer].Unlock()

		// 3. Copy solution source to reviewer (excluding .git/, architecture/, solution-deliverable/)
		dstSolution := filepath.Join(o.baseDir, agent.RepoDir(agent.Reviewer), "solution")
		if err := os.RemoveAll(dstSolution); err != nil {
			o.log("Error removing old solution in reviewer: %v", err)
		}
		if err := os.MkdirAll(dstSolution, 0755); err != nil {
			o.log("Error creating solution dir in reviewer: %v", err)
		} else if err := CopySolutionSource(solverDir, dstSolution); err != nil {
			o.log("Error copying solution to reviewer: %v", err)
		}

		// 4. Copy architecture to reviewer
		dstArchReview := filepath.Join(o.baseDir, agent.RepoDir(agent.Reviewer), "architecture")
		if err := os.RemoveAll(dstArchReview); err != nil {
			o.log("Error removing old architecture in reviewer: %v", err)
		}
		if err := os.MkdirAll(dstArchReview, 0755); err != nil {
			o.log("Error creating architecture dir in reviewer: %v", err)
		} else if err := CopyArchitectureContract(archDir, dstArchReview); err != nil {
			o.log("Error copying architecture to reviewer: %v", err)
		}
	}()

	// 5. Enqueue start_evaluator
	if _, err := queue.WriteRequest(o.queuesDir, "start_evaluator", "orchestrator", ""); err != nil {
		o.log("Error queuing start_evaluator: %v", err)
	}
	// 6. Enqueue start_reviewer
	if _, err := queue.WriteRequest(o.queuesDir, "start_reviewer", "orchestrator", ""); err != nil {
		o.log("Error queuing start_reviewer: %v", err)
	}
}

func (o *Orchestrator) handleConfirmSolution(item workItem) {
	// 1. Copy evaluation/solution-deliverable/ to output/
	// Acquire evaluator copy mutex to prevent races with handleHandoffSolution
	// which writes to the same evaluator directory concurrently.
	o.copyMu[agent.Evaluator].Lock()
	srcDeliverable := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "solution-deliverable")
	dstOutput := filepath.Join(o.baseDir, "output")
	if err := os.RemoveAll(dstOutput); err != nil {
		o.log("Error removing old output dir: %v", err)
	}
	if err := os.MkdirAll(dstOutput, 0755); err != nil {
		o.log("Error creating output dir: %v", err)
	} else if err := CopyDir(srcDeliverable, dstOutput); err != nil {
		o.log("Error copying deliverable to output: %v", err)
	}
	o.copyMu[agent.Evaluator].Unlock()

	// 2. Move confirm_solution request to completed (it has been processed)
	if err := queue.MarkCompleted(item.requestPath, o.completedDir); err != nil {
		o.log("Error marking confirm_solution completed: %v", err)
	}
	atomic.AddInt32(&o.completedCount, 1)

	// 3. Log confirmation
	o.log("========================================")
	o.log("Solution confirmed and delivered!")
	o.log("Output available in: output/")
	o.log("========================================")

	// 4. Signal orchestrator to enter graceful shutdown
	o.doneOnce.Do(func() { close(o.done) })
}
