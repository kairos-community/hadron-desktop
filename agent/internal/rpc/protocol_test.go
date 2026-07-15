package rpc

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSocketPathsAreDistinctAndAbsolute(t *testing.T) {
	paths := []string{SessionSocketPath, RootSocketPath, ControlSocketPath}
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" || p[0] != '/' {
			t.Fatalf("socket path %q is not absolute", p)
		}
		if seen[p] {
			t.Fatalf("duplicate socket path %q", p)
		}
		seen[p] = true
	}
	if SessionSocketPath != "/run/hadron-agent/session/session.sock" {
		t.Fatalf("unexpected SessionSocketPath: %s", SessionSocketPath)
	}
	if RootSocketPath != "/run/hadron-agent/root/root.sock" {
		t.Fatalf("unexpected RootSocketPath: %s", RootSocketPath)
	}
	if ControlSocketPath != "/run/hadron-agent/control/control.sock" {
		t.Fatalf("unexpected ControlSocketPath: %s", ControlSocketPath)
	}
}

func TestMaxBodyBytesIsTwoMiB(t *testing.T) {
	if MaxBodyBytes != 2<<20 {
		t.Fatalf("MaxBodyBytes = %d, want %d", MaxBodyBytes, 2<<20)
	}
}

func TestCallRequestRoundTrip(t *testing.T) {
	req := CallRequest{
		RequestID: "req-1",
		Tool:      "terminal",
		Arguments: json.RawMessage(`{"command":"echo hi"}`),
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got CallRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RequestID != req.RequestID || got.Tool != req.Tool {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, req)
	}
	if string(got.Arguments) != string(req.Arguments) {
		t.Fatalf("arguments mismatch: got %s, want %s", got.Arguments, req.Arguments)
	}
}

func TestCallResponseIsMCPCallToolResult(t *testing.T) {
	// CallResponse must be usable interchangeably with *mcp.CallToolResult
	// (it's a type alias), so any value produced elsewhere in the codebase
	// (package cua, package api) can flow through this protocol unchanged.
	var resp *CallResponse = &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "hello"}},
	}
	var _ *mcp.CallToolResult = resp
}
