// Package mcp wires the pbrainctl MCP server: register tool handlers
// against an injected set of dependencies (index, working memory,
// embedder, vault dir).
//
// Tools live in sibling files (recall.go, perceive.go, etc.) so each
// is self-contained and easy to find. Server is the thin glue.
//
// The dependency container ServerDeps is constructed by the cobra
// command (cmd/pbrainctl/main.go); this package does NOT open
// databases or read env vars itself, which keeps the test surface
// narrow and makes tools easy to unit-test with fakes.
package mcp

import (
	"github.com/mark3labs/mcp-go/server"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
	"github.com/neverprepared/mcp-phantom-brain/internal/index"
	"github.com/neverprepared/mcp-phantom-brain/internal/working"
)

// ServerDeps is the dependency container the MCP tool handlers close
// over.
type ServerDeps struct {
	// Index is the per-brain vectors.db handle. Tools that read or
	// write the vector + FTS5 search index go through it.
	Index *index.Index

	// Working is the per-process working memory database. The
	// task_* tools own most of its surface; brain_* tools may
	// optionally log into it.
	Working *working.DB

	// Embedder is whatever produces embedding vectors for text. In
	// production this is *internal/ollama.Client; tests pass a fake.
	Embedder index.Embedder

	// VaultDir is the absolute path to the brain's vault/ dir —
	// where Wiki/, Raw/{gathered,curated,attachments}/, _queue/
	// live. Tools that write markdown (perceive, learn, attach)
	// resolve paths against it.
	VaultDir string

	// Lifecycle is the running brain's lifecycle handle, populated
	// when mcpCmd is invoked under the v5.0 agent contract
	// (CL_BRAIN_*). The brain_death / brain_checkpoint /
	// brain_status tools delegate to it. Nil in the legacy
	// BRAIN_VAULT_PATH startup mode — those tools then return a
	// helpful "not available in legacy mode" error rather than
	// segfaulting.
	Lifecycle *brain.Lifecycle
}

// Server is the MCP tool registry. Construct once per process, hand
// to Register, then ServeStdio.
type Server struct {
	deps ServerDeps
}

// NewServer wraps a ServerDeps so its tools can be registered against
// an mcp-go server instance.
func NewServer(deps ServerDeps) *Server {
	return &Server{deps: deps}
}

// Register attaches every Phase-0-ported brain_* and task_* tool to
// the supplied mcp-go server. Each tool delegates to a small handler
// in a sibling file.
func (s *Server) Register(srv *server.MCPServer) {
	srv.AddTool(recallTool(), s.handleRecall)
	srv.AddTool(perceiveTool(), s.handlePerceive)
	srv.AddTool(learnTool(), s.handleLearn)
	srv.AddTool(attachTool(), s.handleAttach)
	srv.AddTool(traceTool(), s.handleTrace)

	srv.AddTool(taskStartTool(), s.handleTaskStart)
	srv.AddTool(taskUpdateTool(), s.handleTaskUpdate)
	srv.AddTool(taskCompleteTool(), s.handleTaskComplete)
	srv.AddTool(taskGetTool(), s.handleTaskGet)

	srv.AddTool(brainStatusTool(), s.handleBrainStatus)
	srv.AddTool(brainCheckpointTool(), s.handleBrainCheckpoint)
	srv.AddTool(brainDeathTool(), s.handleBrainDeath)
}
