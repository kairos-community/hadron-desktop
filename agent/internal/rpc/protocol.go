// Package rpc defines hadron-agent's fixed internal Unix-socket RPC
// protocol: the four endpoints (POST /v1/call, GET /v1/health, POST
// /v1/pause, POST /v1/resume) the public gateway (a later Phase-2 task)
// uses to reach the two brokers -- the unprivileged session broker and the
// privileged root helper -- plus the local control socket. It is the
// internal transport only; it carries no authentication policy of its own
// beyond requiring (and forwarding) the raw Authorization header on sockets
// configured to require one. Nothing in this package is part of the public
// MCP contract frozen by package api.
package rpc

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Socket paths
// ---------------------------------------------------------------------------

// Deployment socket paths for the three brokers. This package never creates
// these paths or the directories that contain them -- it only accepts an
// already-created net.Listener (server side) or dials an already-existing
// path (client side). Creating the listener, and setting its file
// permissions/ownership, is systemd's and Phase 3's responsibility.
const (
	// SessionSocketPath is the unprivileged session broker's socket.
	SessionSocketPath = "/run/hadron-agent/session/session.sock"
	// RootSocketPath is the privileged root helper's socket. A Server
	// bound to this path must be constructed with Config.RequireAuth set.
	RootSocketPath = "/run/hadron-agent/root/root.sock"
	// ControlSocketPath is the local control socket used for pause/resume
	// and health of the gateway itself.
	ControlSocketPath = "/run/hadron-agent/control/control.sock"
)

// ---------------------------------------------------------------------------
// HTTP paths
// ---------------------------------------------------------------------------

// The four HTTP endpoints are the entire wire surface of this protocol.
// Any other path, or the right path with the wrong method, is rejected by
// Server with 404 or 405 before ever reaching a Handler.
const (
	PathCall   = "/v1/call"
	PathHealth = "/v1/health"
	PathPause  = "/v1/pause"
	PathResume = "/v1/resume"
)

// MaxBodyBytes bounds every request body Server accepts on any endpoint.
// Server enforces this with http.MaxBytesReader, so an oversized body is
// rejected as soon as the limit is crossed, without ever buffering more
// than MaxBodyBytes into memory.
const MaxBodyBytes = 2 << 20 // 2 MiB

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// CallRequest is the JSON body of POST /v1/call. Arguments is left as raw
// JSON so this package never needs to know any tool's argument shape --
// that decoding happens inside whatever Handler.Call ultimately dispatches
// to (package api's typed tool inputs).
type CallRequest struct {
	RequestID string          `json:"request_id"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallResponse is the JSON body of a successful POST /v1/call response: the
// serialized MCP tool result, unchanged. It is a type alias (not a new
// type) so callers can pass a *mcp.CallToolResult produced anywhere in the
// codebase (e.g. package cua) straight through without conversion.
type CallResponse = mcp.CallToolResult

// HealthStatus is the JSON body of a successful GET /v1/health response.
type HealthStatus struct {
	// OK reports whether the broker behind this socket is currently able
	// to serve calls.
	OK bool `json:"ok"`
	// Paused reports whether the broker is currently paused (see
	// StatusResponse / Handler.Pause).
	Paused bool `json:"paused"`
}

// StatusResponse is the JSON body of a successful POST /v1/pause or POST
// /v1/resume response.
type StatusResponse struct {
	OK bool `json:"ok"`
}

// ErrorResponse is the JSON body of any non-2xx response from Server.
// Message is always a short, clean protocol-level description (malformed
// request, unauthenticated, method/path not found, ...); it never carries
// an internal error's raw text or a stack trace.
type ErrorResponse struct {
	Error string `json:"error"`
}
