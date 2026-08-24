package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/portable"
)

// cmdPackageExport writes a portable context package.
func cmdPackageExport(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("package export", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	out := fs.String("out", "", "file to write, or - for stdout (required)")
	includeSuperseded := fs.Bool("include-superseded", false, "carry history as well as current belief")
	includeChunks := fs.Bool("include-chunks", false, "copy the source passages too")
	notes := fs.String("notes", "", "free-text note recorded in the package header")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	decision, err := a.Policy.AuthorizeRead(ctx, principal, scope, "package-export")
	if err != nil {
		return err
	}

	writer := os.Stdout
	if *out != "-" {
		file, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer file.Close()
		writer = file
	}

	result, err := a.Exporter.Export(ctx, portable.ExportRequest{
		Scope: scope, Principal: principal.Ref(), Policy: decision.Filters,
		IncludeSuperseded: *includeSuperseded, IncludeChunks: *includeChunks,
		Notes: *notes,
	}, writer)
	if err != nil {
		return err
	}

	if *out == "-" {
		return nil
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, result.Bytes)
	for _, kind := range domain.PackageRecordOrder {
		if count := result.Manifest.Counts[kind]; count > 0 {
			fmt.Printf("  %-10s %d\n", kind, count)
		}
	}
	fmt.Printf("  digest     %s\n", result.Manifest.Digest)
	if result.Header.Policy.Filtered {
		// Stated as a fact rather than an alarm. A ceiling is in force on every read, so a
		// warning shaped like a problem would be ignored within a week; what a reader
		// needs to know is that this package cannot be treated as a backup.
		fmt.Printf("\nExported with a classification ceiling of %s: anything above it is "+
			"not in this package.\n", result.Header.Policy.MaxClassification)
		if result.Header.Policy.Excluded > 0 {
			fmt.Printf("%d claim(s) were dropped by policy during the export.\n",
				result.Header.Policy.Excluded)
		}
	}
	return nil
}

// cmdPackageImport reads a portable context package.
func cmdPackageImport(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("package import", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	in := fs.String("file", "", "package to read, or - for stdin (required)")
	sourceName := fs.String("source", "", "source to attribute imported knowledge to")
	dryRun := fs.Bool("dry-run", false, "verify and report without writing")
	acceptPredicates := fs.Bool("accept-predicates", false,
		"adopt the predicate semantics the package declares")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		return errors.New("--file is required")
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	reader := os.Stdin
	if *in != "-" {
		file, err := os.Open(*in)
		if err != nil {
			return err
		}
		defer file.Close()
		reader = file
	}

	summary, err := a.Importer.Import(ctx, portable.ImportRequest{
		Scope: scope, Principal: principal.Ref(), SourceName: *sourceName,
		DryRun: *dryRun, AcceptPredicates: *acceptPredicates,
	}, reader)
	if err != nil {
		return err
	}

	fmt.Printf("package from %s/%s", summary.Header.Source.WorkspaceSlug,
		summary.Header.Source.GraphSpaceSlug)
	if summary.Header.Source.Instance != "" {
		fmt.Printf(" (%s)", summary.Header.Source.Instance)
	}
	fmt.Println()
	if summary.Header.Policy.Filtered {
		fmt.Printf("  filtered at export to %s\n", summary.Header.Policy.MaxClassification)
	}
	fmt.Printf("  %s\n", summary.Describe())

	for _, rejection := range summary.Rejected {
		fmt.Printf("  rejected %s: %s\n", rejection.Kind, rejection.Reason)
	}
	if *dryRun {
		fmt.Println("\nVerified. Nothing was written.")
	}
	return nil
}

// cmdPackageVerify checks a package's integrity without a database.
func cmdPackageVerify(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("package verify", flag.ContinueOnError)
	in := fs.String("file", "", "package to verify, or - for stdin (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		return errors.New("--file is required")
	}

	reader := os.Stdin
	if *in != "-" {
		file, err := os.Open(*in)
		if err != nil {
			return err
		}
		defer file.Close()
		reader = file
	}

	header, manifest, err := portable.Verify(context.Background(), reader)
	if err != nil {
		return err
	}

	fmt.Printf("valid context package, version %d\n", header.Version)
	fmt.Printf("  from      %s/%s\n", header.Source.WorkspaceSlug, header.Source.GraphSpaceSlug)
	fmt.Printf("  created   %s\n", header.CreatedAt.Format("2006-01-02 15:04:05 MST"))
	for _, kind := range domain.PackageRecordOrder {
		if count := manifest.Counts[kind]; count > 0 {
			fmt.Printf("  %-10s %d\n", kind, count)
		}
	}
	fmt.Printf("  digest    %s\n", manifest.Digest)
	if header.Policy.Filtered {
		fmt.Printf("\nExported with a classification ceiling of %s: anything above it is "+
			"not in this package.\n", header.Policy.MaxClassification)
	}
	return nil
}
