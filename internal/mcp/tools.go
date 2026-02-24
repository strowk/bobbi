package mcp

import (
	"fmt"
	"os"
	"path/filepath"

	"bobbi/internal/queue"
)

type ToolHandler func(args map[string]interface{}) ToolResult

func ToolsForAgent(agentType string) []Tool {
	switch agentType {
	case "solver":
		return []Tool{
			{
				Name:        "handoff_solution",
				Description: "Submit your solution for evaluation. Call this after you've built your solution and placed the deliverable in solution-deliverable/.",
				InputSchema: InputSchema{
					Type:       "object",
					Properties: map[string]Property{},
				},
			},
			{
				Name:        "request_architecture_change",
				Description: "Request a change to the technical architecture/contract.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]Property{
						"reason": {Type: "string", Description: "Description of the architecture change needed"},
					},
					Required: []string{"reason"},
				},
			},
		}
	case "evaluator":
		return []Tool{
			{
				Name:        "request_architecture_change",
				Description: "Request a change to the technical architecture/contract.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]Property{
						"reason": {Type: "string", Description: "Description of the architecture change needed"},
					},
					Required: []string{"reason"},
				},
			},
			{
				Name:        "request_solution_change",
				Description: "Request the solver to make changes to the solution.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]Property{
						"reason": {Type: "string", Description: "Description of what needs to be changed in the solution"},
					},
					Required: []string{"reason"},
				},
			},
			{
				Name:        "confirm_solution",
				Description: "Confirm that the solution passes all tests and meets the specification.",
				InputSchema: InputSchema{
					Type:       "object",
					Properties: map[string]Property{},
				},
			},
		}
	case "reviewer":
		return []Tool{
			{
				Name:        "request_solution_change",
				Description: "Request the solver to make changes to the solution based on code review feedback.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]Property{
						"reason": {Type: "string", Description: "Code review feedback describing what needs to be changed"},
					},
					Required: []string{"reason"},
				},
			},
		}
	}
	return nil
}

func HandlersForAgent(agentType string) map[string]ToolHandler {
	handlers := make(map[string]ToolHandler)

	// Resolve queues dir: agent repos are at <project>/<agent-dir>,
	// so queues are at ../.bobbi/queues/ relative to cwd
	queuesDir := resolveQueuesDir()

	switch agentType {
	case "solver":
		handlers["handoff_solution"] = func(args map[string]interface{}) ToolResult {
			_, err := queue.WriteRequest(queuesDir, "handoff_solution", "solver", "")
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue handoff: %v", err))
			}
			return TextResult("Solution handed off for evaluation. The evaluator will run tests against your deliverable.")
		}
		handlers["request_architecture_change"] = makeArchChangeHandler(queuesDir, "solver")

	case "evaluator":
		handlers["request_architecture_change"] = makeArchChangeHandler(queuesDir, "evaluator")
		handlers["request_solution_change"] = func(args map[string]interface{}) ToolResult {
			reason, _ := args["reason"].(string)
			_, err := queue.WriteRequest(queuesDir, "request_solution_change", "evaluator", reason)
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue request: %v", err))
			}
			return TextResult("Solution change requested. The solver will be notified.")
		}
		handlers["confirm_solution"] = func(args map[string]interface{}) ToolResult {
			_, err := queue.WriteRequest(queuesDir, "confirm_solution", "evaluator", "")
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue confirmation: %v", err))
			}
			return TextResult("Solution confirmed! The deliverable will be copied to the output directory.")
		}

	case "reviewer":
		handlers["request_solution_change"] = func(args map[string]interface{}) ToolResult {
			reason, _ := args["reason"].(string)
			_, err := queue.WriteRequest(queuesDir, "request_solution_change", "reviewer", reason)
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue request: %v", err))
			}
			return TextResult("Solution change requested based on code review. The solver will be notified.")
		}
	}

	return handlers
}

func makeArchChangeHandler(queuesDir, from string) ToolHandler {
	return func(args map[string]interface{}) ToolResult {
		reason, _ := args["reason"].(string)
		_, err := queue.WriteRequest(queuesDir, "request_architecture_change", from, reason)
		if err != nil {
			return ErrorResult(fmt.Sprintf("Failed to queue request: %v", err))
		}
		return TextResult("Architecture change requested. The architect will be notified.")
	}
}

func resolveQueuesDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return filepath.Join("..", ".bobbi", "queues")
	}
	return filepath.Join(cwd, "..", ".bobbi", "queues")
}
