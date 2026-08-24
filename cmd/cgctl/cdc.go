package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/connector/cdc"
	"github.com/gimantha/strata/internal/domain"
)

// cmdStreamRegister binds a stream to the mapping that interprets its rows.
func cmdStreamRegister(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("stream register", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	sourceName := fs.String("source", "", "registered source name (required)")
	stream := fs.String("stream", "", "stream name, such as public.customers (required)")
	path := fs.String("mapping", "", "path to a mapping JSON file (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stream == "" || *path == "" {
		return errors.New("--stream and --mapping are required")
	}

	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var mapping domain.ChangeMapping
	if err := json.Unmarshal(raw, &mapping); err != nil {
		return fmt.Errorf("cannot parse %s: %w", *path, err)
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}
	source, err := a.Ledger.GetSourceByName(ctx, scope.WorkspaceID, *sourceName)
	if err != nil {
		return err
	}

	registered, err := a.Ledger.UpsertCDCStream(ctx, domain.CDCStream{
		WorkspaceID: scope.WorkspaceID,
		SourceID:    source.ID,
		Stream:      *stream,
		Mapping:     mapping,
	}, principal.ID)
	if err != nil {
		return err
	}

	fmt.Printf("stream %s registered for source %s\n", registered.Stream, source.Name)
	fmt.Printf("  subject type: %s\n", registered.Mapping.SubjectType)
	fmt.Printf("  %d column(s) mapped\n", len(registered.Mapping.Columns))
	if registered.Checkpoint.LastOffset != "" {
		// Re-registering a mapping must not rewind a running connector.
		fmt.Printf("  checkpoint left at offset %s\n", registered.Checkpoint.LastOffset)
	}
	return nil
}

// cmdStreamList shows registered streams and how far each has consumed.
func cmdStreamList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("stream ls", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}
	streams, err := a.Ledger.ListCDCStreams(ctx, scope.WorkspaceID)
	if err != nil {
		return err
	}

	tw := newTable("STREAM", "SUBJECT TYPE", "COLUMNS", "OFFSET", "CONSUMED")
	for _, stream := range streams {
		row(tw, stream.Stream, stream.Mapping.SubjectType,
			fmt.Sprintf("%d", len(stream.Mapping.Columns)),
			orDash(stream.Checkpoint.LastOffset),
			fmt.Sprintf("%d", stream.Checkpoint.EventsConsumed))
	}
	return tw.Flush()
}

// cmdStreamReplay consumes a change log through the connector.
func cmdStreamReplay(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("stream replay", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	sourceName := fs.String("source", "", "registered source name (required)")
	stream := fs.String("stream", "", "stream to consume; empty accepts every stream in the log")
	path := fs.String("file", "", "path to a JSONL change log (required)")
	resume := fs.Bool("resume", true, "skip events at or before the stored checkpoint")
	limit := fs.Int("limit", 0, "stop after this many events")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--file is required")
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleWriter)
	if err != nil {
		return err
	}
	source, err := a.Ledger.GetSourceByName(ctx, scope.WorkspaceID, *sourceName)
	if err != nil {
		return err
	}

	log, err := cdc.OpenReplayFile(*path)
	if err != nil {
		return err
	}
	defer log.Close()

	result, err := a.Connector.Run(ctx, cdc.Request{
		Scope:                scope,
		Principal:            principal.Ref(),
		SourceID:             source.ID,
		Stream:               *stream,
		Limit:                *limit,
		ResumeFromCheckpoint: *resume,
	}, log)
	if err != nil {
		return err
	}

	fmt.Printf("%d change(s) read: %d accepted, %d already ingested, %d skipped\n",
		result.Consumed, result.Accepted, result.Duplicates, result.Skipped)
	if result.LastOffset != "" {
		fmt.Printf("checkpoint at offset %s\n", result.LastOffset)
	}
	if result.Accepted > 0 {
		// The events are durable; turning them into knowledge is the worker's job.
		fmt.Println("\nRun the worker to process them into knowledge.")
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
