package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/domain"
)

func cmdPurgePlan(ctx context.Context, a *app.App, args []string) error {
	return cmdPurge(ctx, a, args, true)
}

func cmdPurgeRun(ctx context.Context, a *app.App, args []string) error {
	return cmdPurge(ctx, a, args, false)
}

func cmdPurge(ctx context.Context, a *app.App, args []string, dryRun bool) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	before := fs.String("before", "", "delete eligible knowledge older than RFC3339/date (required)")
	reason := fs.String("reason", "", "retention-policy reason (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cutoff, err := parsePurgeTime(*before)
	if err != nil {
		return err
	}
	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}
	run, err := a.Purge.Run(ctx, domain.KnowledgePurgeRequest{
		Scope: scope, Before: cutoff, Principal: principal.Ref(), Reason: *reason, DryRun: dryRun,
	})
	if err != nil {
		return err
	}
	verb := "eligible"
	if !dryRun {
		verb = "deleted"
	}
	counts := run.Eligible
	if !dryRun {
		counts = run.Deleted
	}
	fmt.Printf("purge %s: %s\n", run.ID, run.Status)
	fmt.Printf("  %s: %d events, %d assertions, %d evidence, %d artifacts, %d blobs\n",
		verb, counts.Events, counts.Assertions, counts.Evidence, counts.Artifacts, counts.Blobs)
	if dryRun {
		fmt.Println("  dry run only; use `purge run` with the same cutoff and reason to execute")
	}
	return nil
}

func cmdPurgeHold(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("purge hold", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	event := fs.String("event", "", "optional source event id; empty protects the graph space")
	reason := fs.String("reason", "", "why this knowledge must be retained (required)")
	expires := fs.String("expires-at", "", "optional RFC3339 expiry")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}
	var expiresAt *time.Time
	if *expires != "" {
		parsed, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			return fmt.Errorf("--expires-at must be RFC3339: %w", err)
		}
		expiresAt = &parsed
	}
	hold, err := a.Purge.Hold(ctx, domain.KnowledgeHold{
		Scope: scope, SourceEventID: domain.SourceEventID(*event), Reason: *reason,
		CreatedBy: principal.Ref(), ExpiresAt: expiresAt,
	})
	if err != nil {
		return err
	}
	fmt.Printf("knowledge hold %s created\n", hold.ID)
	return nil
}

func cmdPurgeRelease(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("purge release", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	hold := fs.String("hold", "", "knowledge hold id (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hold == "" {
		return errors.New("--hold is required")
	}
	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}
	if err := a.Purge.Release(ctx, scope, domain.KnowledgeHoldID(*hold), principal.Ref()); err != nil {
		return err
	}
	fmt.Printf("knowledge hold %s released\n", *hold)
	return nil
}

func cmdPurgeRetry(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("purge retry", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	runID := fs.String("run", "", "failed knowledge purge run id (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runID == "" {
		return errors.New("--run is required")
	}
	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}
	run, err := a.Purge.Retry(ctx, scope, domain.KnowledgePurgeID(*runID))
	if err != nil {
		return err
	}
	fmt.Printf("purge %s: %s\n", run.ID, run.Status)
	return nil
}

func parsePurgeTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, errors.New("--before is required")
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse(domain.DateLayout, raw); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, errors.New("--before must be RFC3339 or YYYY-MM-DD")
}
