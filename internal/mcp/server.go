package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func Serve(agentType string, stdin io.Reader, stdout io.Writer) error {
	tools := ToolsForAgent(agentType)
	handlers := HandlersForAgent(agentType)

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	encoder := json.NewEncoder(stdout)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "[mcp] Failed to parse request: %v\n", err)
			continue
		}

		// Notifications (no id) - just acknowledge
		if req.ID == nil || string(req.ID) == "null" {
			// notifications like notifications/initialized don't need a response
			continue
		}

		var result interface{}
		var rpcErr *RPCError

		switch req.Method {
		case "initialize":
			result = InitializeResult{
				ProtocolVersion: "2024-11-05",
				Capabilities: ServerCapabilities{
					Tools: &ToolsCapability{},
				},
				ServerInfo: ServerInfo{
					Name:    "bobbcode",
					Version: "0.1.0",
				},
			}

		case "ping":
			result = map[string]interface{}{}

		case "tools/list":
			result = ToolsListResult{Tools: tools}

		case "tools/call":
			var params ToolCallParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				rpcErr = &RPCError{Code: -32602, Message: "Invalid params"}
			} else if handler, ok := handlers[params.Name]; ok {
				result = handler(params.Arguments)
			} else {
				rpcErr = &RPCError{Code: -32601, Message: fmt.Sprintf("Unknown tool: %s", params.Name)}
			}

		default:
			rpcErr = &RPCError{Code: -32601, Message: fmt.Sprintf("Unknown method: %s", req.Method)}
		}

		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
			Error:   rpcErr,
		}

		if err := encoder.Encode(resp); err != nil {
			fmt.Fprintf(os.Stderr, "[mcp] Failed to write response: %v\n", err)
		}
	}

	return scanner.Err()
}
