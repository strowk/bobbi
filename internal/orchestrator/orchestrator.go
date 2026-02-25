package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"bobbi/internal/agent"
	"bobbi/internal/queue"
)

// AgentInfo holds the current state of a single agent type for display.
type AgentInfo struct {
	Status       string // "idle", "running", "queued"
	Prompt       string
	InputTokens  int64
	OutputTokens int64
	HasRun       bool
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

func New(baseDir string, userPrompt string, rawMode bool) *Orchestrator {
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
		timeout:      30 * time.Minute,
		done:         make(chan struct{}),
		agentInfo:    info,
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
	return o.startTime
}

// GetTotalTokens returns aggregate token counts across all agents.
func (o *Orchestrator) GetTotalTokens() (int64, int64) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var totalIn, totalOut int64
	for _, info := range o.agentInfo {
		totalIn += info.InputTokens
		totalOut += info.OutputTokens
	}
	return totalIn, totalOut
}

// Done returns a channel that is closed when the orchestrator is done.
func (o *Orchestrator) Done() <-chan struct{} {
	return o.done
}

func (o *Orchestrator) log(format string, args ...interface{}) {
	if o.rawMode {
		fmt.Fprintf(os.Stderr, "[bobbi] "+format+"\n", args...)
	}
}

func (o *Orchestrator) Run(ctx context.Context) error {
	o.startTime = time.Now()

	// Apply time limit
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	// Create per-agent workers
	for _, at := range agent.AllTypes() {
		ch := make(chan workItem, 100)
		o.channels[at] = ch
		o.wg.Add(1)
		go o.worker(ctx, at, ch)
	}

	// Bootstrap: if queue is empty, enqueue start_architect
	requests, _, err := queue.ReadRequests(o.queuesDir)
	if err == nil && len(requests) == 0 {
		o.log("No pending requests, bootstrapping with start_architect")
		if _, err := queue.WriteRequest(o.queuesDir, "start_architect", "orchestrator", o.userPrompt); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}

	o.log("Time limit: %s", o.timeout)

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
	_, paths, err := queue.ReadRequests(o.queuesDir)
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
	requests, paths, err := queue.ReadRequests(o.queuesDir)
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
			o.handleHandoffSolution()
			if err := queue.MarkCompleted(item.requestPath, o.completedDir); err != nil {
				o.log("Error marking completed: %v", err)
			}
			atomic.AddInt32(&o.completedCount, 1)
			o.dispatchedMu.Lock()
			delete(o.dispatched, paths[i])
			o.dispatchedMu.Unlock()
			continue
		case "confirm_solution":
			o.log("Processing %s request (from: %s) directly", reqType, req.Request.From)
			o.handleConfirmSolution()
			if err := queue.MarkCompleted(item.requestPath, o.completedDir); err != nil {
				o.log("Error marking completed: %v", err)
			}
			atomic.AddInt32(&o.completedCount, 1)
			o.dispatchedMu.Lock()
			delete(o.dispatched, paths[i])
			o.dispatchedMu.Unlock()
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
			ch <- item
		}
	}
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
	for item := range ch {
		select {
		case <-ctx.Done():
			return
		default:
		}

		o.log("Processing %s request (from: %s) for %s agent",
			item.request.Request.Type, item.request.Request.From, agentType)
		o.processRequest(ctx, agentType, item)

		select {
		case <-ctx.Done():
			o.log("Agent %s was interrupted, leaving request in queue", agentType)
			return
		default:
		}
		if err := queue.MarkCompleted(item.requestPath, o.completedDir); err != nil {
			o.log("Error marking completed: %v", err)
		}
		atomic.AddInt32(&o.completedCount, 1)

		// Clean up dispatched entry to prevent unbounded map growth
		o.dispatchedMu.Lock()
		delete(o.dispatched, item.requestPath)
		o.dispatchedMu.Unlock()
	}
}

func (o *Orchestrator) processRequest(ctx context.Context, agentType agent.AgentType, item workItem) {
	reqType := item.request.Request.Type
	addCtx := item.request.Request.AdditionalContext

	select {
	case <-ctx.Done():
		o.log("Skipping %s agent (shutting down)", agentType)
		return
	default:
	}

	repoDir := filepath.Join(o.baseDir, agent.RepoDir(agentType))
	prompt := agent.BuildPrompt(agentType, reqType, addCtx)

	// Pre-agent: copy relevant content
	o.preCopy(agentType)

	// Update agent state to running
	o.mu.Lock()
	info := o.agentInfo[agentType]
	info.Status = "running"
	info.Prompt = prompt
	info.InputTokens = 0
	info.OutputTokens = 0
	o.mu.Unlock()

	// Build start options
	opts := &agent.StartOptions{
		OnTokens: func(input, output int64) {
			o.mu.Lock()
			info.InputTokens += input
			info.OutputTokens += output
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

	if err := agent.StartAgent(ctx, agentType, repoDir, prompt, opts); err != nil {
		o.log("Agent %s error: %v", agentType, err)
	}

	// Update agent state to idle
	o.mu.Lock()
	info.Status = "idle"
	info.HasRun = true
	o.mu.Unlock()

	// Post-agent: copy outputs
	o.postCopy(agentType)
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
		if err := CopyDir(archDir, dst); err != nil {
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
		if err := CopyDir(archDir, dst); err != nil {
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
			if err := CopyDir(archDir, dst); err != nil {
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
	} else if err := CopyDir(archDir, dstArch); err != nil {
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
