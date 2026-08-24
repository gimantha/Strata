package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/memory"
)

// cmdConsolidate turns repeated observation into stable facts.
func cmdConsolidate(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("consolidate", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	minObservations := fs.Int("min-observations", 0, "how many times a claim must be seen")
	minSources := fs.Int("min-sources", 0, "how many distinct sources must corroborate it")
	limit := fs.Int("limit", 0, "maximum observations to examine")
	dryRun := fs.Bool("dry-run", false, "report what would be derived without writing")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleWriter)
	if err != nil {
		return err
	}

	result, err := a.Memory.Consolidate(ctx, memory.ConsolidateRequest{
		Scope:     scope,
		Principal: principal.Ref(),
		Rule: domain.ConsolidationRule{
			MinObservations:    *minObservations,
			MinDistinctSources: *minSources,
		},
		Limit:  *limit,
		DryRun: *dryRun,
	})
	if err != nil {
		return err
	}

	fmt.Printf("%d observation(s) examined in %d group(s)\n", result.Examined, result.Groups)
	if len(result.Qualified) > 0 {
		tw := newTable("PREDICATE", "SEEN", "SOURCES", "CONFIDENCE", "SUMMARY")
		for _, group := range result.Qualified {
			row(tw, group.Predicate, fmt.Sprintf("%d", len(group.Members)),
				fmt.Sprintf("%d", len(group.Sources)),
				fmt.Sprintf("%.2f", group.Confidence()), group.Summary())
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if *dryRun {
		// Saying so explicitly: a command that reports what it would do and then does
		// nothing is easy to misread as a command that did it.
		fmt.Printf("\n%d group(s) would be consolidated. Nothing was written.\n", len(result.Qualified))
		return nil
	}

	fmt.Printf("\n%d fact(s) derived", len(result.Derived))
	if result.Existing > 0 {
		fmt.Printf(", %d already known", result.Existing)
	}
	fmt.Println()
	return nil
}

// cmdForget takes a claim out of active context.
func cmdForget(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	assertionID := fs.String("assertion", "", "assertion id (required)")
	kind := fs.String("kind", "deactivate", "deactivate, retract, retention, or erasure")
	reason := fs.String("reason", "", "why (required for deactivation)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *assertionID == "" {
		return errors.New("--assertion is required")
	}

	parsed, err := domain.ParseForgetKind(*kind)
	if err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := a.Ledger.ResolveAssertionWorkspace(ctx, domain.AssertionID(*assertionID),
		workspacesOf(principal))
	if err != nil {
		return err
	}

	assertion, err := a.Memory.Forget(ctx, memory.ForgetRequest{
		Scope:       domain.Scope{WorkspaceID: ws},
		Actor:       principal.Ref(),
		AssertionID: domain.AssertionID(*assertionID),
		Kind:        parsed,
		Reason:      *reason,
	})
	if err != nil {
		return err
	}

	fmt.Printf("assertion %s deactivated\n", assertion.ID)
	fmt.Printf("  status: %s (unchanged)\n", assertion.Status)
	// The distinction is the point of the command, so it is stated rather than implied.
	fmt.Println("  the claim is still true, still cited, and still answerable as of an earlier instant")
	fmt.Printf("  undo with: cgctl reactivate --assertion %s\n", assertion.ID)
	return nil
}

// cmdReactivate puts deactivated knowledge back in scope.
func cmdReactivate(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("reactivate", flag.ContinueOnError)
	assertionID := fs.String("assertion", "", "assertion id (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *assertionID == "" {
		return errors.New("--assertion is required")
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := a.Ledger.ResolveAssertionWorkspace(ctx, domain.AssertionID(*assertionID),
		workspacesOf(principal))
	if err != nil {
		return err
	}

	assertion, err := a.Memory.Reactivate(ctx, domain.Scope{WorkspaceID: ws},
		principal.Ref(), domain.AssertionID(*assertionID))
	if err != nil {
		return err
	}

	fmt.Printf("assertion %s is active again\n", assertion.ID)
	return nil
}

func workspacesOf(p domain.Principal) []domain.WorkspaceID {
	out := make([]domain.WorkspaceID, 0, len(p.Grants))
	for _, grant := range p.Grants {
		out = append(out, grant.WorkspaceID)
	}
	return out
}
