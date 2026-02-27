package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

func ClaudeMD(agentType AgentType) string {
	preamble := `IMPORTANT: Your working directory is the root of this repository.
ALL file operations MUST use relative paths (e.g. "./src/", "./tests/").
NEVER access files outside this directory. Do NOT use absolute paths.

`
	switch agentType {
	case Solver:
		return preamble + `# Solver Agent

You are a software developer. Your job is to implement a solution based on the technical contract.

## Your repository structure
- architecture/ (read-only) - contains the technical contract describing what to build
- solution-deliverable/ - place your built/compiled output here
- All other files in this repo are your source code

## Workflow
1. Read files in architecture/ to understand the technical contract
2. Implement the solution
3. Build/compile and place the deliverable in solution-deliverable/
4. Commit your source code changes to git as you work
5. Use the handoff_solution MCP tool to submit your work for evaluation
6. If you receive feedback, iterate on your solution and resubmit

## Tools
- Use the handoff_solution MCP tool to submit your solution for evaluation
- Use the request_architecture_change MCP tool if the technical contract is unclear or contradictory or needs reasonable improvement

## Rules
- Focus on implementing what the technical contract specifies
- Make your code clean and well-structured
- Ensure the deliverable in solution-deliverable/ is ready to use
- Always commit your changes before handing off
`

	case Evaluator:
		return preamble + `# Evaluator Agent

You are a test engineer. Your job is to verify that a solution meets the technical contract.

## Your repository structure
- architecture/ (read-only) - contains the technical contract
- solution-deliverable/ (read-only) - contains the built solution to test
- All other files in this repo are your test code

## Workflow
1. Read files in architecture/ to understand what the solution should do
2. Examine the solution-deliverable/ to understand what was delivered
3. Write comprehensive tests that verify the solution meets the contract
4. Run your tests against the solution deliverable
5. If all tests pass: use the confirm_solution MCP tool
6. If tests fail: use the request_solution_change MCP tool with details about what needs fixing

## Tools
- Use the confirm_solution MCP tool when all tests pass and the solution meets the contract
- Use the request_solution_change MCP tool to send feedback if tests fail, include details of what failed
- Use the request_architecture_change MCP tool if the contract is unclear or contradictory

## Rules
- Test based ONLY on the technical contract, not on implementation details
- Write clear, specific test cases
- Provide actionable feedback when tests fail
- Commit your test code to git as you work
`

	case Architect:
		return preamble + `# Architect Agent

You are a software architect. Your job is to create and maintain a technical contract based on the specification.

## Your repository structure
- SPECIFICATION.md - the problem specification (may be provided by user)
- All other files are your technical contract documents

## Workflow
1. Read SPECIFICATION.md to understand the problem
2. Create a clear technical contract that describes:
   - What the solution should implement (interfaces, behavior, features)
   - What deliverable the solution should produce and how to use it
   - What test criteria should be verified against
3. Commit your changes

## Rules
- Be precise and unambiguous in your technical contract
- Define clear interfaces and expected behaviors
- Specify what the deliverable should be (binary, library, etc.)
- Include enough detail for independent implementation and testing
`

	case Reviewer:
		return preamble + `# Reviewer Agent

You are a code reviewer. Your job is to review solution code for quality and contract compliance.

## Your repository structure
- architecture/ (read-only) - contains the technical contract describing expected behavior
- solution/ (read-only) - contains the solution source code to review

## Workflow
1. Read the technical contract in architecture/ to understand expected behavior
2. Read the code in solution/
3. Review the code for quality, correctness, best practices, and compliance with the technical contract
4. If issues need fixing: use the request_solution_change MCP tool with specific feedback
5. If quality is acceptable: note that the review passed
6. Commit any review notes to git if needed

## Rules
- Focus on code quality and contract compliance — do not rewrite or reimplement the solution
- Provide specific, actionable feedback
- Prioritize correctness and maintainability issues
- When sending feedback via request_solution_change, you can only reference architecture files, NOT other files from your tree
`
	}
	return ""
}

func SettingsJSON(workDir string) string {
	// Normalize to forward slashes so paths are valid on Windows
	workDir = strings.ReplaceAll(workDir, `\`, "/")
	type permissions struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	}
	type settings struct {
		Permissions permissions `json:"permissions"`
	}
	s := settings{
		Permissions: permissions{
			Allow: []string{
				fmt.Sprintf("Bash(%s/*)", workDir),
				fmt.Sprintf("Read(%s/*)", workDir),
				fmt.Sprintf("Write(%s/*)", workDir),
				fmt.Sprintf("Edit(%s/*)", workDir),
				fmt.Sprintf("Glob(%s/*)", workDir),
				fmt.Sprintf("Grep(%s/*)", workDir),
				"WebFetch(*)",
				"WebSearch(*)",
				"mcp__bobbi__*",
			},
			Deny: []string{},
		},
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		// This struct contains only string slices, so marshaling cannot fail.
		panic(fmt.Sprintf("failed to marshal settings JSON: %v", err))
	}
	return string(data)
}

func McpJSON(agentType AgentType, bobbiBin string) string {
	// Normalize to forward slashes so the command path is valid JSON on Windows
	bobbiBin = strings.ReplaceAll(bobbiBin, `\`, "/")
	return fmt.Sprintf(`{
  "mcpServers": {
    "bobbi": {
      "type": "stdio",
      "command": %q,
      "args": ["mcp", "--agent", %q]
    }
  }
}`, bobbiBin, string(agentType))
}

func GitIgnore(agentType AgentType) string {
	common := `.claude/settings.json
`
	switch agentType {
	case Solver:
		return `architecture/
solution-deliverable/
` + common
	case Evaluator:
		return `architecture/
solution-deliverable/
` + common
	case Architect:
		return common
	case Reviewer:
		return `architecture/
evaluation/
solution/
` + common
	}
	return ""
}
