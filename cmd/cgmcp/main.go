// Command cgmcp serves the context graph as MCP tools over stdio (AGENTS.md section 26).
//
// It is a separate binary from the HTTP server because an MCP client launches it as a
// subprocess and talks to it on stdin and stdout. That has one consequence worth stating
// loudly: nothing may be written to stdout except protocol messages, so every log line goes
// to stderr.
//
//	cgmcp --graph-space <id> --key-id <id>
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gimantha/strata/internal/api/mcp"
	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "cgmcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	graphSpace := flag.String("graph-space", os.Getenv("CG_GRAPH_SPACE_ID"),
		"graph space to serve (required)")
	keyID := flag.String("key-id", os.Getenv("CG_KEY_ID"),
		"api key id to act as (required)")
	sourceName := flag.String("source", "mcp",
		"registered source that ingested content is attributed to")
	flag.Parse()

	if *graphSpace == "" {
		return fmt.Errorf("--graph-space is required")
	}
	if *keyID == "" {
		return fmt.Errorf("--key-id is required: MCP is not a privileged bypass, so this " +
			"server acts as a specific principal")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	application, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer application.Close(ctx)

	// The principal is resolved once, at startup, and every tool call runs as it — through
	// the same policy evaluation the HTTP API uses. An MCP client cannot choose who it is
	// (AGENTS.md section 26).
	principal, err := application.Identity.PrincipalForKeyID(ctx, *keyID)
	if err != nil {
		return err
	}
	scope, err := application.Identity.ResolveGraphSpace(ctx, principal,
		domain.GraphSpaceID(*graphSpace), domain.RoleReader)
	if err != nil {
		return err
	}

	tools := mcp.NewTools(mcp.Deps{
		Gateway:   application.Gateway,
		Retriever: application.Retriever,
		Assembler: application.Assembler,
		Policy:    application.Policy,
		Ledger:    application.Ledger,
	}, principal, scope, *sourceName)

	// Logs to stderr, always. stdout belongs to the protocol, and one stray line on it
	// desynchronizes the client for the rest of the session.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.InfoContext(ctx, "mcp server ready",
		slog.String("graph_space_id", string(scope.GraphSpaceID)),
		slog.String("principal", string(principal.ID)),
		slog.Int("tools", len(tools.Definitions())))

	server := mcp.NewServer(tools, logger)
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
