package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/policy"
)

// policyFile is the on-disk form of a policy set: reviewed and committed like any contract.
type policyFile struct {
	Name             string                `json:"name"`
	Notes            string                `json:"notes,omitempty"`
	DefaultClearance domain.Classification `json:"default_clearance,omitempty"`
	Rules            []domain.PolicyRule   `json:"rules"`
}

// cmdPolicyDefine appends an immutable policy version from a JSON file.
func cmdPolicyDefine(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("policy define", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	path := fs.String("file", "", "path to a policy JSON file (required)")
	activate := fs.Bool("activate", false, "make this the policy in force")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--file is required")
	}

	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var file policyFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("cannot parse %s: %w", *path, err)
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	set, err := a.Policy.Define(ctx, policy.DefineRequest{
		Scope: scope, Principal: principal.Ref(),
		Name: file.Name, Notes: file.Notes,
		DefaultClearance: file.DefaultClearance, Rules: file.Rules,
		Activate: *activate,
	})
	if err != nil {
		return err
	}

	fmt.Printf("policy version %d created (%s)\n", set.Version, set.ID)
	fmt.Printf("  %d rule(s), default clearance %s\n", len(set.Rules), set.DefaultClearance)
	if !*activate {
		fmt.Println("\nNot in force. Check what it would decide, then activate it:")
		fmt.Printf("  cgctl policy explain --graph-space %s --principal <id>\n", *graphSpace)
	}
	return nil
}

// cmdPolicyList shows the policy history and which version is in force.
func cmdPolicyList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("policy ls", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	sets, err := a.Policy.List(ctx, scope.WorkspaceID, 50)
	if err != nil {
		return err
	}

	tw := newTable("ID", "VERSION", "ACTIVE", "CLEARANCE", "RULES", "NAME")
	for _, set := range sets {
		active := ""
		if set.Active {
			active = "yes"
		}
		row(tw, string(set.ID), fmt.Sprintf("%d", set.Version), active,
			string(set.DefaultClearance), fmt.Sprintf("%d", len(set.Rules)),
			truncate(set.Name, 28))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(sets) == 0 {
		// Absence has a describable meaning rather than being a gap somebody has to
		// interpret.
		fmt.Println("no policy is configured; role-based access with an internal ceiling applies")
	}
	return nil
}

// cmdPolicyExplain answers what a principal would be allowed to see.
func cmdPolicyExplain(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("policy explain", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	subject := fs.String("principal", "", "principal to evaluate (defaults to the caller)")
	action := fs.String("action", "read", "read, write, export, or admin")
	purpose := fs.String("purpose", "", "stated purpose, when policy requires one")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}
	parsedAction, err := domain.ParsePolicyAction(*action)
	if err != nil {
		return err
	}

	evaluated := principal
	if *subject != "" {
		evaluated, err = a.Identity.PrincipalForID(ctx, domain.PrincipalID(*subject))
		if err != nil {
			return err
		}
	}

	decision, set, err := a.Policy.Explain(ctx, domain.AccessRequest{
		Principal: evaluated, Action: parsedAction, Scope: scope, Purpose: *purpose,
	})
	if err != nil {
		return err
	}

	verdict := "denied"
	if decision.Allowed {
		verdict = "allowed"
	}
	fmt.Printf("%s may %s: %s\n", evaluated.ID, parsedAction, verdict)
	fmt.Printf("  reason: %s\n", decision.Reason)
	fmt.Printf("  policy version %d\n", set.Version)
	if !decision.Allowed {
		return nil
	}

	fmt.Printf("  may see: ")
	levels := decision.Filters.PermittedClassifications()
	for i, level := range levels {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(level)
	}
	fmt.Println()

	if len(decision.Filters.DeniedSources) > 0 {
		fmt.Printf("  excluded sources: %v\n", decision.Filters.DeniedSources)
	}
	if len(decision.Filters.DeniedPredicates) > 0 {
		fmt.Printf("  excluded predicates: %v\n", decision.Filters.DeniedPredicates)
	}
	if len(decision.Filters.AllowedSources) > 0 {
		fmt.Printf("  restricted to sources: %v\n", decision.Filters.AllowedSources)
	}
	return nil
}

// cmdPolicyClearance sets a principal's ceiling inside one workspace.
func cmdPolicyClearance(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("policy clearance", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	subject := fs.String("principal", "", "principal to set (required)")
	level := fs.String("max-classification", "", "ceiling, or empty to clear it")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *subject == "" {
		return errors.New("--principal is required")
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	var clearance domain.Classification
	if *level != "" {
		clearance, err = domain.ParseClassification(*level)
		if err != nil {
			return err
		}
	}
	if err := a.Policy.SetClearance(ctx, scope, principal.Ref(),
		domain.PrincipalID(*subject), clearance); err != nil {
		return err
	}

	if clearance == "" {
		fmt.Printf("clearance cleared for %s; the policy default applies\n", *subject)
		return nil
	}
	// A clearance only ever narrows, so saying so here avoids the obvious misreading.
	fmt.Printf("%s may now see up to %s in this workspace, or less if policy is tighter\n",
		*subject, clearance)
	return nil
}

// cmdTraces lists recent retrieval traces.
func cmdTraces(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("traces", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	limit := fs.Int("limit", 20, "maximum traces to show")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	traces, err := a.Ledger.ListTraces(ctx, scope, *limit)
	if err != nil {
		return err
	}

	tw := newTable("ID", "WHEN", "PRINCIPAL", "RESULTS", "MS", "QUERY")
	for _, trace := range traces {
		text := trace.QueryText
		if trace.Redacted {
			text = "(redacted) " + trace.QueryHash[:12]
		}
		row(tw, string(trace.ID), trace.QueryTime.Format("15:04:05"),
			truncate(string(trace.Principal.ID), 16),
			fmt.Sprintf("%d", len(trace.SelectedRefs)),
			fmt.Sprintf("%d", trace.Latency.Milliseconds()),
			truncate(text, 40))
	}
	return tw.Flush()
}
