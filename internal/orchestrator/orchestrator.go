package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bobbi/internal/agent"
	"bobbi/internal/queue"
)

// AgentInfo holds the current state of a single agent type for display.
type AgentInfo struct {
	Status            string // "idle", "running", "queued"
	Prompt            string
	InputTokens       int64 // tokens for the current/latest run
	OutputTokens      int64 // tokens for the current/latest run
	TotalInputTokens  int64 // cumulative tokens across all runs
	TotalOutputTokens int64 // cumulative tokens across all runs
	HasRun            bool
}

// Orchestrator manages the lifecycle of agent processes.
type Orchestrator struct {
	baseDir      string
	queuesDir    string
	completedDir string
	userPrompt   string
	rawMode      bool

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

	// File logging
	logFile   *os.File
	logFileMu sync.Mutex

	// State for TUI rendering (protected by mu)
	mu             sync.RWMutex
	agentInfo      map[agent.AgentType]*AgentInfo
	completedCount int32 // atomic
	startTime      time.Time
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

func New(baseDir string, userPrompt string, rawMode bool, timeout time.Duration) *Orchestrator {
	info := make(map[agent.AgentType]*AgentInfo)
	for _, at := range agent.AllTypes() {
		info[at] = &AgentInfo{Status: "idle"}
	}
	return &Orchestrator{
		baseDir:      baseDir,
		queuesDir:    filepath.Join(baseDir, ".bobbi", "queues"),
		completedDir: filepath.Join(baseDir, ".bobbi", "completed"),
		userPrompt:   userPrompt,
		rawMode:      rawMode,
		channels:     make(map[agent.AgentType]chan workItem),
		dispatched:   make(map[string]bool),
		timeout:      timeout,
		done:         make(chan struct{}),
		agentInfo:    info,
	}
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

// GetAgentInfo returns a copy of the agent info for the given type.
func (o *Orchestrator) GetAgentInfo(at agent.AgentType) AgentInfo {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if info, ok := o.agentInfo[at]; ok {
		return *info
	}
	return AgentInfo{Status: "idle"}
}

// GetCompletedCount returns the number of completed requests.
func (o *Orchestrator) GetCompletedCount() int {
	return int(atomic.LoadInt32(&o.completedCount))
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
		go o.worker(ctx, at, ch)
	}

	// Bootstrap: if queue is empty, enqueue start_architect
	requests, _, err := queue.ReadRequests(o.queuesDir, o.log)
	if err == nil && len(requests) == 0 {
		o.log("No pending requests, bootstrapping with start_architect")
		if _, err := queue.WriteRequest(o.queuesDir, "start_architect", "orchestrator", o.userPrompt); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
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
			o.drainQueues()
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
	for _, ch := range o.channels {
		close(ch)
	}
	o.wg.Wait()
}

func (o *Orchestrator) drainQueues() {
	_, paths, err := queue.ReadRequests(o.queuesDir, o.log)
	if err != nil {
		return
	}
	for _, p := range paths {
		if err := queue.MarkCompleted(p, o.completedDir); err != nil {
			o.log("Error draining queue file %s: %v", p, err)
		}
	}
	if len(paths) > 0 {
		o.log("Drained %d remaining queue file(s) to completed", len(paths))
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
			o.wg.Add(1)
			go o.handleDirectRequest(item, o.handleConfirmSolution)
			continue
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
				o.log("Channel buffer full for %s agent, dropping request %s", targetAgent, item.requestPath)
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

func (o *Orchestrator) worker(ctx context.Context, agentType agent.AgentType, ch <-chan workItem) {
	defer o.wg.Done()
	for {
		// Block until first item arrives (or channel closes)
		item, ok := <-ch
		if !ok {
			return
		}

		select {
		case <-ctx.Done():
			return
		default:
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
			select {
			case <-ctx.Done():
				return
			default:
			}
			o.processBatch(ctx, agentType, batch)
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
				foldedContext := mergeContext(groups[t])
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
// with attributed sections.
func mergeContext(items []workItem) string {
	if len(items) == 1 {
		return items[0].request.Request.AdditionalContext
	}

	var parts []string
	for i, item := range items {
		ctx := item.request.Request.AdditionalContext
		if ctx == "" {
			continue
		}
		from := item.request.Request.From
		parts = append(parts, fmt.Sprintf(
			"--- Feedback %d (from %s) ---\n%s",
			i+1, from, ctx,
		))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
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
func (o *Orchestrator) processBatch(ctx context.Context, agentType agent.AgentType, batch workBatch) {
	select {
	case <-ctx.Done():
		o.log("Skipping %s agent (shutting down)", agentType)
		return
	default:
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
	o.preCopy(agentType)

	// Update agent state to running
	o.mu.Lock()
	info := o.agentInfo[agentType]
	info.Status = "running"
	info.Prompt = prompt
	info.InputTokens = 0
	info.OutputTokens = 0
	o.mu.Unlock()

	o.log("Starting %s agent", agentType)

	// Build start options
	opts := &agent.StartOptions{
		OnTokens: func(input, output int64) {
			o.mu.Lock()
			info.InputTokens += input
			info.OutputTokens += output
			info.TotalInputTokens += input
			info.TotalOutputTokens += output
			o.mu.Unlock()
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

	if err := agent.StartAgent(ctx, agentType, repoDir, prompt, opts); err != nil {
		o.log("Agent %s error: %v", agentType, err)
	}

	// Update agent state to idle
	o.mu.Lock()
	info.Status = "idle"
	info.HasRun = true
	o.mu.Unlock()

	o.log("Agent %s finished", agentType)

	// Post-agent: copy outputs (once per batch, skip if cancelled)
	select {
	case <-ctx.Done():
		return
	default:
	}
	o.postCopy(agentType)

	// Mark ALL items in the batch as completed
	o.markItemsCompleted(batch.items)
}

func (o *Orchestrator) preCopy(agentType agent.AgentType) {
	archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))

	switch agentType {
	case agent.Solver:
		dst := filepath.Join(o.baseDir, agent.RepoDir(agent.Solver), "architecture")
		if err := os.RemoveAll(dst); err != nil {
			o.log("Error removing old architecture in solver: %v", err)
		}
		if err := os.MkdirAll(dst, 0755); err != nil {
			o.log("Error creating architecture dir in solver: %v", err)
			return
		}
		if err := CopyArchitectureContract(archDir, dst); err != nil {
			o.log("Error copying architecture to solver: %v", err)
		}
	case agent.Evaluator:
		dst := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "architecture")
		if err := os.RemoveAll(dst); err != nil {
			o.log("Error removing old architecture in evaluator: %v", err)
		}
		if err := os.MkdirAll(dst, 0755); err != nil {
			o.log("Error creating architecture dir in evaluator: %v", err)
			return
		}
		if err := CopyArchitectureContract(archDir, dst); err != nil {
			o.log("Error copying architecture to evaluator: %v", err)
		}
	}
}

func (o *Orchestrator) postCopy(agentType agent.AgentType) {
	switch agentType {
	case agent.Architect:
		archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))
		for _, target := range []agent.AgentType{agent.Solver, agent.Evaluator} {
			dst := filepath.Join(o.baseDir, agent.RepoDir(target), "architecture")
			if err := os.RemoveAll(dst); err != nil {
				o.log("Error removing old architecture in %s: %v", target, err)
			}
			if err := os.MkdirAll(dst, 0755); err != nil {
				o.log("Error creating architecture dir in %s: %v", target, err)
				continue
			}
			if err := CopyArchitectureContract(archDir, dst); err != nil {
				o.log("Error copying architecture to %s: %v", target, err)
			}
		}
		// After architect, start solver
		if _, err := queue.WriteRequest(o.queuesDir, "start_solver", "orchestrator", ""); err != nil {
			o.log("Error queuing start_solver: %v", err)
		}
	}
}

func (o *Orchestrator) handleHandoffSolution() {
	solverDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Solver))

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

	// 2. Copy solution source to reviewer (excluding .git/, architecture/, solution-deliverable/)
	dstSolution := filepath.Join(o.baseDir, agent.RepoDir(agent.Reviewer), "solution")
	if err := os.RemoveAll(dstSolution); err != nil {
		o.log("Error removing old solution in reviewer: %v", err)
	}
	if err := os.MkdirAll(dstSolution, 0755); err != nil {
		o.log("Error creating solution dir in reviewer: %v", err)
	} else if err := CopySolutionSource(solverDir, dstSolution); err != nil {
		o.log("Error copying solution to reviewer: %v", err)
	}

	// 3. Copy architecture to evaluator
	archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))
	dstArch := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "architecture")
	if err := os.RemoveAll(dstArch); err != nil {
		o.log("Error removing old architecture in evaluator: %v", err)
	}
	if err := os.MkdirAll(dstArch, 0755); err != nil {
		o.log("Error creating architecture dir in evaluator: %v", err)
	} else if err := CopyArchitectureContract(archDir, dstArch); err != nil {
		o.log("Error copying architecture to evaluator: %v", err)
	}

	// 4. Enqueue start_evaluator
	if _, err := queue.WriteRequest(o.queuesDir, "start_evaluator", "orchestrator", ""); err != nil {
		o.log("Error queuing start_evaluator: %v", err)
	}
	// 5. Enqueue start_reviewer
	if _, err := queue.WriteRequest(o.queuesDir, "start_reviewer", "orchestrator", ""); err != nil {
		o.log("Error queuing start_reviewer: %v", err)
	}
}

func (o *Orchestrator) handleConfirmSolution() {
	// 1. Copy evaluation/solution-deliverable/ to output/
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

	o.log("========================================")
	o.log("Solution confirmed and delivered!")
	o.log("Output available in: output/")
	o.log("========================================")

	// 2. Signal orchestrator to shut down
	o.doneOnce.Do(func() { close(o.done) })
}
