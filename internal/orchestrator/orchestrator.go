package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"bobbi/internal/agent"
	"bobbi/internal/queue"
)

type Orchestrator struct {
	baseDir      string
	queuesDir    string
	completedDir string

	// User-supplied prompt for the architect bootstrap
	userPrompt string

	// Per-agent work channels enforce serial execution per agent type
	channels map[agent.AgentType]chan workItem
	wg       sync.WaitGroup

	// Track which request files are already dispatched
	dispatched   map[string]bool
	dispatchedMu sync.Mutex

	// Time limit for the entire run
	timeout time.Duration
	done    chan struct{} // closed on confirm_solution
}

type workItem struct {
	request     queue.Request
	requestPath string
}

func New(baseDir string, userPrompt string) *Orchestrator {
	return &Orchestrator{
		baseDir:      baseDir,
		queuesDir:    filepath.Join(baseDir, ".bobbi", "queues"),
		completedDir: filepath.Join(baseDir, ".bobbi", "completed"),
		userPrompt:   userPrompt,
		channels:     make(map[agent.AgentType]chan workItem),
		dispatched:   make(map[string]bool),
		timeout:      30 * time.Minute,
		done:         make(chan struct{}),
	}
}

func (o *Orchestrator) Run(ctx context.Context) error {
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
		fmt.Println("[bobbi] No pending requests, bootstrapping with start_architect")
		if _, err := queue.WriteRequest(o.queuesDir, "start_architect", "orchestrator", o.userPrompt); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}

	fmt.Printf("[bobbi] Time limit: %s\n", o.timeout)

	// Poll loop
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Do an immediate poll before waiting
	o.poll()

	for {
		select {
		case <-o.done:
			fmt.Println("[bobbi] Solution confirmed, shutting down...")
			o.shutdown()
			o.drainQueues()
			return nil
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				fmt.Printf("[bobbi] Time limit of %s reached, shutting down...\n", o.timeout)
			} else {
				fmt.Println("[bobbi] Shutting down orchestrator...")
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

// drainQueues moves any remaining queue files to completed on shutdown.
func (o *Orchestrator) drainQueues() {
	_, paths, err := queue.ReadRequests(o.queuesDir)
	if err != nil {
		return
	}
	for _, p := range paths {
		if err := queue.MarkCompleted(p, o.completedDir); err != nil {
			fmt.Fprintf(os.Stderr, "[bobbi] Error draining queue file %s: %v\n", p, err)
		}
	}
	if len(paths) > 0 {
		fmt.Printf("[bobbi] Drained %d remaining queue file(s) to completed\n", len(paths))
	}
}

func (o *Orchestrator) poll() {
	requests, paths, err := queue.ReadRequests(o.queuesDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[bobbi] Error reading queues: %v\n", err)
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
		targetAgent := o.routeRequest(req.Request.Type)
		if targetAgent == "" {
			fmt.Fprintf(os.Stderr, "[bobbi] Unknown request type: %s\n", req.Request.Type)
			continue
		}

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
	case "start_evaluator", "handoff_solution", "request_evaluation_change":
		return agent.Evaluator
	case "start_reviewer":
		return agent.Reviewer
	case "confirm_solution":
		return agent.Evaluator // handled specially
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

		fmt.Printf("[bobbi] Processing %s request (from: %s) for %s agent\n",
			item.request.Request.Type, item.request.Request.From, agentType)
		o.processRequest(ctx, agentType, item)

		// Only mark completed if the context wasn't cancelled (i.e. not interrupted)
		select {
		case <-ctx.Done():
			fmt.Printf("[bobbi] Agent %s was interrupted, leaving request in queue\n", agentType)
			return
		default:
		}
		if err := queue.MarkCompleted(item.requestPath, o.completedDir); err != nil {
			fmt.Fprintf(os.Stderr, "[bobbi] Error marking completed: %v\n", err)
		}
	}
}

func (o *Orchestrator) processRequest(ctx context.Context, agentType agent.AgentType, item workItem) {
	reqType := item.request.Request.Type
	addCtx := item.request.Request.AdditionalContext

	switch reqType {
	case "confirm_solution":
		o.handleConfirmSolution()
		return

	case "handoff_solution":
		o.handleHandoffSolution()
		return
	}

	// Check context before starting a potentially long agent run
	select {
	case <-ctx.Done():
		fmt.Printf("[bobbi] Skipping %s agent (shutting down)\n", agentType)
		return
	default:
	}

	// Run the agent
	repoDir := filepath.Join(o.baseDir, agent.RepoDir(agentType))

	// Pre-agent: copy relevant content into agent's repo
	o.preCopy(agentType)

	prompt := agent.BuildPrompt(agentType, reqType, addCtx)
	if err := agent.StartAgent(ctx, agentType, repoDir, prompt); err != nil {
		fmt.Fprintf(os.Stderr, "[bobbi] Agent %s error: %v\n", agentType, err)
	}

	// Post-agent: copy outputs to dependent repos
	o.postCopy(agentType)
}

func (o *Orchestrator) preCopy(agentType agent.AgentType) {
	archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))

	switch agentType {
	case agent.Solver:
		dst := filepath.Join(o.baseDir, agent.RepoDir(agent.Solver), "architecture")
		os.RemoveAll(dst)
		os.MkdirAll(dst, 0755)
		if err := CopyRepo(archDir, dst); err != nil {
			fmt.Fprintf(os.Stderr, "[bobbi] Error copying architecture to solver: %v\n", err)
		}

	case agent.Evaluator:
		dst := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "architecture")
		os.RemoveAll(dst)
		os.MkdirAll(dst, 0755)
		if err := CopyRepo(archDir, dst); err != nil {
			fmt.Fprintf(os.Stderr, "[bobbi] Error copying architecture to evaluator: %v\n", err)
		}
	}
}

func (o *Orchestrator) postCopy(agentType agent.AgentType) {
	switch agentType {
	case agent.Architect:
		archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))

		for _, target := range []agent.AgentType{agent.Solver, agent.Evaluator} {
			dst := filepath.Join(o.baseDir, agent.RepoDir(target), "architecture")
			os.RemoveAll(dst)
			os.MkdirAll(dst, 0755)
			if err := CopyRepo(archDir, dst); err != nil {
				fmt.Fprintf(os.Stderr, "[bobbi] Error copying architecture to %s: %v\n", target, err)
			}
		}

		// After architect, start solver
		if _, err := queue.WriteRequest(o.queuesDir, "start_solver", "orchestrator", ""); err != nil {
			fmt.Fprintf(os.Stderr, "[bobbi] Error queuing start_solver: %v\n", err)
		}
	}
}

func (o *Orchestrator) handleHandoffSolution() {
	solverDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Solver))

	// Copy solution-deliverable to evaluator
	srcDeliverable := filepath.Join(solverDir, "solution-deliverable")
	dstDeliverable := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "solution-deliverable")
	os.RemoveAll(dstDeliverable)
	os.MkdirAll(dstDeliverable, 0755)
	if err := CopyDir(srcDeliverable, dstDeliverable); err != nil {
		fmt.Fprintf(os.Stderr, "[bobbi] Error copying deliverable to evaluator: %v\n", err)
	}

	// Copy solution source to reviewer (excluding .git/, .claude/, architecture/, solution-deliverable/)
	dstSolution := filepath.Join(o.baseDir, agent.RepoDir(agent.Reviewer), "solution")
	os.RemoveAll(dstSolution)
	os.MkdirAll(dstSolution, 0755)
	if err := CopyRepo(solverDir, dstSolution); err != nil {
		fmt.Fprintf(os.Stderr, "[bobbi] Error copying solution to reviewer: %v\n", err)
	}

	// Copy architecture to evaluator (repo copy — skip .git/.claude)
	archDir := filepath.Join(o.baseDir, agent.RepoDir(agent.Architect))
	dstArch := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "architecture")
	os.RemoveAll(dstArch)
	os.MkdirAll(dstArch, 0755)
	if err := CopyRepo(archDir, dstArch); err != nil {
		fmt.Fprintf(os.Stderr, "[bobbi] Error copying architecture to evaluator: %v\n", err)
	}

	// Start evaluator and reviewer
	if _, err := queue.WriteRequest(o.queuesDir, "start_evaluator", "orchestrator", ""); err != nil {
		fmt.Fprintf(os.Stderr, "[bobbi] Error queuing start_evaluator: %v\n", err)
	}
	if _, err := queue.WriteRequest(o.queuesDir, "start_reviewer", "orchestrator", ""); err != nil {
		fmt.Fprintf(os.Stderr, "[bobbi] Error queuing start_reviewer: %v\n", err)
	}
}

func (o *Orchestrator) handleConfirmSolution() {
	// Copy solution deliverable to output directory
	srcDeliverable := filepath.Join(o.baseDir, agent.RepoDir(agent.Evaluator), "solution-deliverable")
	dstOutput := filepath.Join(o.baseDir, "output")
	os.RemoveAll(dstOutput)
	os.MkdirAll(dstOutput, 0755)
	if err := CopyDir(srcDeliverable, dstOutput); err != nil {
		fmt.Fprintf(os.Stderr, "[bobbi] Error copying deliverable to output: %v\n", err)
	}

	fmt.Println("[bobbi] ========================================")
	fmt.Println("[bobbi] Solution confirmed and delivered!")
	fmt.Println("[bobbi] Output available in: output/")
	fmt.Println("[bobbi] ========================================")

	// Signal orchestrator to shut down
	close(o.done)
}
