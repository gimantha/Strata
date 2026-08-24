// Package mcp exposes the graph as tools an agent can call (AGENTS.md section 26).
//
// The Model Context Protocol is JSON-RPC 2.0 over a byte stream — newline-delimited on stdio.
// It is implemented here directly rather than through an SDK, for the same reason the LLM and
// embedding providers are behind interfaces: a protocol shim is a hundred lines, and taking a
// dependency for it would put a third party between this system and its own contract
// (AGENTS.md section 3, provider independence).
//
// The tool surface is deliberately small and stable. It exposes what an agent needs — ingest,
// search, context, entity, assertion, explain, temporal query — rather than mirroring internal
// services, so the internals can change without breaking every agent.
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2024-11-05"

// ServerName and ServerVersion identify this implementation to a client.
const (
	ServerName    = "strata"
	ServerVersion = "1"
)

// request is one JSON-RPC 2.0 call.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether a message expects no reply.
//
// JSON-RPC notifications carry no id, and answering one is a protocol violation that some
// clients treat as fatal. MCP sends `notifications/initialized` on every connection, so
// getting this wrong breaks the handshake with every client rather than none.
func (r request) isNotification() bool { return len(r.ID) == 0 }

// response is one JSON-RPC 2.0 reply.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC error codes, plus the one MCP adds.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

func newResponse(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func newError(id json.RawMessage, code int, message string, data any) response {
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{
		Code: code, Message: message, Data: data,
	}}
}

// initializeResult is the handshake reply.
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolDefinition is one entry in tools/list.
type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// callParams is the argument shape of tools/call.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// callResult is what a tool returns.
//
// MCP models tool output as content blocks. Everything here returns one text block holding
// JSON, because an agent reasoning over structured results needs the structure — and because
// canonical ids in that JSON are what let it follow a reference instead of asking for a
// bigger payload (AGENTS.md section 26).
type callResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// textResult renders a value as a JSON text block.
func textResult(value any) (callResult, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return callResult{}, fmt.Errorf("cannot encode tool result: %w", err)
	}
	return callResult{Content: []contentBlock{{Type: "text", Text: string(encoded)}}}, nil
}

// errorResult reports a tool failure to the agent.
//
// A failed tool is not a failed protocol call: the agent asked a reasonable question and got
// an answer it can act on, so this is a successful response carrying isError rather than a
// JSON-RPC error. Returning a transport error instead tends to make clients drop the
// conversation rather than retry with different arguments.
func errorResult(err error) callResult {
	return callResult{
		Content: []contentBlock{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}
