// Package mcpwire holds the JSON-RPC and MCP Streamable HTTP pieces the api's MCP servers and its MCP client share.
package mcpwire

import (
	"fmt"
	"slices"
	"time"
)

const (
	Version = "2.0"

	// Latest is the newest MCP protocol version, answered to a client that asks for one this side lacks.
	Latest           = "2025-11-25"
	SessionHeader    = "Mcp-Session-Id"
	EventStream      = "text/event-stream"
	KeepaliveComment = ": keepalive\n\n"
	MaxBody          = 1 << 20
	// Keepalive spaces the comments that keep an idle event stream open through proxies.
	Keepalive = 25 * time.Second

	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

var versions = []string{Latest, "2025-06-18"}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message) }

// CodeOf is e's code, 0 for no error.
func CodeOf(e *Error) int {
	if e == nil {
		return 0
	}
	return e.Code
}

// Negotiate answers an initialize's protocol version: the requested one when supported, else Latest.
func Negotiate(requested string) string {
	if slices.Contains(versions, requested) {
		return requested
	}
	return Latest
}

// Event frames data as one server-sent "message" event.
func Event(data []byte) []byte {
	return fmt.Appendf(nil, "event: message\ndata: %s\n\n", data)
}
