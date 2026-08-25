package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/domain"
)

// cmdRecoveryClassify prints which tables a backup must contain and which it may skip.
//
// Printed from the running binary rather than copied into a runbook, because a runbook that
// lists tables goes stale the first time somebody adds one, and the failure shows up during
// a restore (AGENTS.md section 40).
func cmdRecoveryClassify(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("recovery classify", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	classification, err := a.Ledger.ClassifyTables(ctx)
	if err != nil {
		return err
	}

	fmt.Println("CANONICAL — restore these; nothing regenerates them")
	for _, name := range classification.Canonical {
		fmt.Printf("  %s\n", name)
	}
	fmt.Println("\nDERIVED — rebuildable from the canonical tables; back up only for recovery speed")
	for _, name := range classification.Derived {
		fmt.Printf("  %s\n", name)
	}

	if !classification.Complete() {
		fmt.Printf("\nWARNING: %s\n", classification.Problem())
		return errors.New("the backup classification does not match this schema")
	}
	fmt.Println("\nEvery table in this schema is accounted for.")
	return nil
}

// cmdRecoveryDrill proves the derived half is genuinely rebuildable from the canonical half.
//
// Section 40 asks for restore and rebuild to be tested regularly. The restore half needs a
// backup and a spare database, so it lives in scripts/restore-drill.sh; this is the half that
// can be checked against any deployment, including the restored copy that script creates.
//
// Destructive by design: it deletes every derived record and rebuilds them. Safe on a
// restored copy, and refused without --confirm anywhere else, because "it is only an index"
// is exactly the reasoning that precedes an unplanned outage.
func cmdRecoveryDrill(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("recovery drill", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	confirm := fs.Bool("confirm", false,
		"required: this drops and rebuilds every derived index in the workspace")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*confirm {
		return errors.New("--confirm is required: the drill drops every derived index in the " +
			"workspace and rebuilds it, which makes retrieval unavailable while it runs")
	}

	_, ws, err := resolveScopeForWorkspace(ctx, a, *keyID, *workspace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	before, err := a.Ledger.CountProjected(ctx, ws)
	if err != nil {
		return err
	}
	fmt.Printf("before:  %s\n", formatCounts(before))
	if total(before) == 0 {
		return errors.New("the workspace has no derived records, so a rebuild would prove " +
			"nothing; run the drill against a workspace with data in it")
	}

	fmt.Println("dropping every derived record...")
	if err := a.Ledger.DropDerived(ctx, ws); err != nil {
		return err
	}
	emptied, err := a.Ledger.CountProjected(ctx, ws)
	if err != nil {
		return err
	}
	if total(emptied) != 0 {
		return fmt.Errorf("records survived the drop: %s", formatCounts(emptied))
	}

	fmt.Println("rebuilding from the canonical ledger alone...")
	stats, err := a.Projector.Rebuild(ctx, ws)
	if err != nil {
		return err
	}

	after, err := a.Ledger.CountProjected(ctx, ws)
	if err != nil {
		return err
	}
	fmt.Printf("after:   %s  (replayed %d event(s))\n", formatCounts(after), stats.Events)

	// Counts rather than content: a rebuild that produced the same number of records from
	// different material would be a deeper bug than this drill is shaped to find, and
	// scenario I's test already compares retrieval results record by record.
	var short []string
	for name, want := range before {
		if after[name] < want {
			short = append(short, fmt.Sprintf("%s %d of %d", name, after[name], want))
		}
	}
	if len(short) > 0 {
		sort.Strings(short)
		return fmt.Errorf("the rebuild did not restore every record (%v): this backup's "+
			"canonical tables are not sufficient to reconstruct its indexes", short)
	}

	fmt.Println("\nDrill passed. Every derived index was reconstructed from the canonical")
	fmt.Println("ledger, which is the property section 40's backup guidance depends on.")
	return nil
}

func total(counts map[string]int) int {
	sum := 0
	for _, count := range counts {
		sum += count
	}
	return sum
}

// formatCounts renders projection counts in a stable order, so two runs can be compared.
func formatCounts(counts map[string]int) string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)

	out := ""
	for i, name := range names {
		if i > 0 {
			out += "  "
		}
		out += fmt.Sprintf("%s %d", name, counts[name])
	}
	return out
}
