package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/gimantha/strata/internal/domain"
)

// Server speaks MCP over a byte stream.
type Server struct {
	tools   *Tools
	logger  *slog.Logger
	writeMu sync.Mutex
}

func NewServer(tools *Tools, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{tools: tools, logger: logger}
}

// maxMessage bounds one JSON-RPC line. An ingest call can carry a document; anything past
// this is a client malfunction rather than a large request.
const maxMessage = 16 << 20

// Serve reads requests until the stream ends or the context is cancelled.
//
// Sequential on purpose. MCP over stdio is one conversation with one client, and concurrency
// here would buy nothing but interleaved writes and a harder-to-reason-about authorization
// path.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64<<10), maxMessage)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if writeErr := s.write(out, newError(nil, codeParseError,
				"cannot parse the request", err.Error())); writeErr != nil {
				return writeErr
			}
			continue
		}
		if req.JSONRPC != "2.0" {
			if err := s.write(out, newError(req.ID, codeInvalidRequest,
				"jsonrpc must be 2.0", nil)); err != nil {
				return err
			}
			continue
		}

		resp, reply := s.dispatch(ctx, req)
		if !reply {
			continue
		}
		if err := s.write(out, resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// dispatch routes one request, reporting whether a reply is owed.
func (s *Server) dispatch(ctx context.Context, req request) (response, bool) {
	// Notifications get no reply, ever. MCP sends notifications/initialized on every
	// connection, and answering it breaks the handshake with clients that are strict about
	// the spec.
	if req.isNotification() {
		s.logger.DebugContext(ctx, "mcp notification", slog.String("method", req.Method))
		return response{}, false
	}

	switch req.Method {
	case "initialize":
		return newResponse(req.ID, initializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    serverCapabilities{Tools: &toolsCapability{}},
			ServerInfo:      serverInfo{Name: ServerName, Version: ServerVersion},
			Instructions: "Query and extend a Strata context graph. Results carry canonical " +
				"ids; follow them with context_graph_get_entity or " +
				"context_graph_get_assertion rather than asking for larger payloads.",
		}), true

	case "ping":
		return newResponse(req.ID, map[string]any{}), true

	case "tools/list":
		return newResponse(req.ID, map[string]any{"tools": s.tools.Definitions()}), true

	case "tools/call":
		var params callParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return newError(req.ID, codeInvalidParams, "cannot parse tool arguments",
				err.Error()), true
		}

		result, err := s.tools.Call(ctx, params.Name, params.Arguments)
		if err != nil {
			// An unknown tool is a protocol error; a tool that ran and failed is a
			// successful call carrying isError, which the tools layer returns instead.
			if errors.Is(err, errUnknownTool) {
				return newError(req.ID, codeMethodNotFound, err.Error(), nil), true
			}
			return newError(req.ID, codeInternalError, err.Error(), nil), true
		}
		return newResponse(req.ID, result), true

	default:
		return newError(req.ID, codeMethodNotFound,
			fmt.Sprintf("unknown method %q", req.Method), nil), true
	}
}

func (s *Server) write(out io.Writer, resp response) error {
	encoded, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("cannot encode response: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("cannot write response: %w", err)
	}
	if flusher, ok := out.(interface{ Flush() error }); ok {
		// stdio to a parent process is usually pipe-buffered; an unflushed reply looks to
		// the client like a hung server.
		return flusher.Flush()
	}
	return nil
}

// Principal is the identity every MCP call runs as.
//
// MCP is not a privileged bypass (AGENTS.md section 26). The server authenticates once from
// its configuration and every tool call goes through the same workspace resolution and policy
// evaluation as an HTTP request from the same principal.
type Principal struct {
	Principal domain.Principal
	Scope     domain.Scope
}
