package mcp

import (
	"fmt"

	"bobbi/internal/agent"
	"bobbi/internal/queue"
)

type ToolHandler func(args map[string]interface{}) ToolResult

func ToolsForAgent(agentType agent.AgentType) []Tool {
	switch agentType {
	case agent.Solver:
		return []Tool{
			{
				Name:        "handoff_solution",
				Description: "Submit the solution for evaluation and review.",
				InputSchema: InputSchema{
					Type:       "object",
					Properties: map[string]Property{},
				},
			},
			{
				Name:        "request_architecture_change",
				Description: "Request a change to the technical contract.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]Property{
						"reason": {Type: "string", Description: "Description of the architecture change needed"},
					},
					Required: []string{"reason"},
				},
			},
		}
	case agent.Evaluator:
		return []Tool{
			{
				Name:        "request_architecture_change",
				Description: "Request a change to the technical contract.",
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
				Description: "Request a change to the solution.",
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
				Description: "Accept the solution as meeting the contract.",
				InputSchema: InputSchema{
					Type:       "object",
					Properties: map[string]Property{},
				},
			},
		}
	case agent.Reviewer:
		return []Tool{
			{
				Name:        "request_solution_change",
				Description: "Request a change to the solution.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]Property{
						"reason": {Type: "string", Description: "Code review feedback describing what needs to be changed"},
					},
					Required: []string{"reason"},
				},
			},
		}
	case agent.Architect:
		return []Tool{
			{
				Name:        "request_solution_change",
				Description: "Describe architecture changes for the implementer to act on.",
				InputSchema: InputSchema{
					Type: "object",
					Properties: map[string]Property{
						"reason": {Type: "string", Description: "Description of the architecture changes for the implementer"},
					},
					Required: []string{"reason"},
				},
			},
		}
	}
	return []Tool{}
}

func HandlersForAgent(agentType agent.AgentType, queuesDir string) map[string]ToolHandler {
	handlers := make(map[string]ToolHandler)

	switch agentType {
	case agent.Solver:
		handlers["handoff_solution"] = func(args map[string]interface{}) ToolResult {
			_, err := queue.WriteRequest(queuesDir, "handoff_solution", "solver", "")
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue handoff: %v", err))
			}
			return TextResult("Solution handed off for evaluation and review.")
		}
		handlers["request_architecture_change"] = makeArchChangeHandler(queuesDir, "solver")

	case agent.Evaluator:
		handlers["request_architecture_change"] = makeArchChangeHandler(queuesDir, "evaluator")
		handlers["request_solution_change"] = func(args map[string]interface{}) ToolResult {
			reason, ok := args["reason"].(string)
			if !ok || reason == "" {
				return ErrorResult("reason parameter is required")
			}
			_, err := queue.WriteRequest(queuesDir, "request_solution_change", "evaluator", reason)
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue request: %v", err))
			}
			return TextResult("Solution change requested.")
		}
		handlers["confirm_solution"] = func(args map[string]interface{}) ToolResult {
			_, err := queue.WriteRequest(queuesDir, "confirm_solution", "evaluator", "")
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue confirmation: %v", err))
			}
			return TextResult("Solution confirmed! The deliverable will be copied to the output directory.")
		}

	case agent.Reviewer:
		handlers["request_solution_change"] = func(args map[string]interface{}) ToolResult {
			reason, ok := args["reason"].(string)
			if !ok || reason == "" {
				return ErrorResult("reason parameter is required")
			}
			_, err := queue.WriteRequest(queuesDir, "request_solution_change", "reviewer", reason)
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue request: %v", err))
			}
			return TextResult("Solution change requested.")
		}

	case agent.Architect:
		handlers["request_solution_change"] = func(args map[string]interface{}) ToolResult {
			reason, ok := args["reason"].(string)
			if !ok || reason == "" {
				return ErrorResult("reason parameter is required")
			}
			_, err := queue.WriteRequest(queuesDir, "request_solution_change", "architect", reason)
			if err != nil {
				return ErrorResult(fmt.Sprintf("Failed to queue request: %v", err))
			}
			return TextResult("Solution change requested.")
		}
	}

	return handlers
}

func makeArchChangeHandler(queuesDir, from string) ToolHandler {
	return func(args map[string]interface{}) ToolResult {
		reason, ok := args["reason"].(string)
		if !ok || reason == "" {
			return ErrorResult("reason parameter is required")
		}
		_, err := queue.WriteRequest(queuesDir, "request_architecture_change", from, reason)
		if err != nil {
			return ErrorResult(fmt.Sprintf("Failed to queue request: %v", err))
		}
		return TextResult("Architecture change requested.")
	}
}
