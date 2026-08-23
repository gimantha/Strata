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
	"github.com/gimantha/strata/internal/ontology"
)

// schemaFile is the on-disk form of an ontology version.
//
// A schema is edited, reviewed, and committed to a repository like any other contract, so
// the CLI takes a file rather than a pile of flags.
type schemaFile struct {
	Name        string                       `json:"name"`
	Notes       string                       `json:"notes,omitempty"`
	EntityTypes []domain.EntityTypeDef       `json:"entity_types"`
	Predicates  []domain.PredicateConstraint `json:"predicates"`
}

// cmdOntologyDefine appends an immutable version from a JSON file.
func cmdOntologyDefine(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("ontology define", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	path := fs.String("file", "", "path to a schema JSON file (required)")
	activate := fs.Bool("activate", false, "activate this version and supersede the others")
	register := fs.Bool("register-predicates", true, "write the constraints into the predicate registry")
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
	var schema schemaFile
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("cannot parse %s: %w", *path, err)
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	version, err := a.Ontology.Define(ctx, ontology.DefineRequest{
		Scope:              scope,
		Principal:          principal.Ref(),
		Name:               schema.Name,
		Notes:              schema.Notes,
		EntityTypes:        schema.EntityTypes,
		Predicates:         schema.Predicates,
		Activate:           *activate,
		RegisterPredicates: *register,
	})
	if err != nil {
		return err
	}

	fmt.Printf("ontology version %d created (%s), status %s\n",
		version.Version, version.ID, version.Status)
	fmt.Printf("  %d entity type(s): %v\n", len(version.EntityTypes), version.EntityTypeNames())
	fmt.Printf("  %d predicate(s): %v\n", len(version.Predicates), version.PredicateNames())
	if !*activate {
		fmt.Println("\nThis is a draft. Validate it against existing knowledge, then activate and bind it:")
		fmt.Printf("  cgctl ontology validate --graph-space %s --version %s\n", *graphSpace, version.ID)
	}
	return nil
}

// cmdOntologyList shows the schema history.
func cmdOntologyList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("ontology ls", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	limit := fs.Int("limit", 20, "maximum versions to show")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}

	versions, err := a.Ontology.List(ctx, scope.WorkspaceID, *limit)
	if err != nil {
		return err
	}

	// Id first, like every other listing: a name can contain spaces, and putting it before
	// the id makes the output awkward to pipe into awk.
	tw := newTable("ID", "VERSION", "STATUS", "TYPES", "PREDICATES", "NAME")
	for _, version := range versions {
		row(tw, string(version.ID), fmt.Sprintf("%d", version.Version), string(version.Status),
			fmt.Sprintf("%d", len(version.EntityTypes)), fmt.Sprintf("%d", len(version.Predicates)),
			truncate(version.Name, 28))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	binding, err := a.Ontology.Binding(ctx, scope)
	if err != nil {
		return err
	}
	fmt.Printf("\nthis graph space is in %s mode", binding.Mode)
	if binding.Version != nil {
		fmt.Printf(", bound to version %d", binding.Version.Version)
	}
	fmt.Println()
	return nil
}

// cmdOntologyBind switches a graph space between open and guided mode.
func cmdOntologyBind(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("ontology bind", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	mode := fs.String("mode", "guided", "open or guided")
	versionID := fs.String("version", "", "ontology version id, required for guided mode")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	parsedMode, err := domain.ParseOntologyMode(*mode)
	if err != nil {
		return err
	}
	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	var version *domain.OntologyVersionID
	if *versionID != "" {
		id := domain.OntologyVersionID(*versionID)
		version = &id
	}
	if err := a.Ontology.Bind(ctx, scope, parsedMode, version); err != nil {
		return err
	}

	fmt.Printf("graph space %s is now in %s mode\n", scope.GraphSpaceID, parsedMode)
	if parsedMode == domain.OntologyGuided {
		// Binding changes what happens next, not what happened before.
		fmt.Println("Claims committed before this keep whatever they were validated against.")
	}
	return nil
}

// cmdOntologyValidate reports what a version would refuse, without changing anything.
func cmdOntologyValidate(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("ontology validate", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	versionID := fs.String("version", "", "ontology version id (defaults to the latest)")
	limit := fs.Int("limit", 0, "maximum assertions to check")
	show := fs.Int("show", 10, "how many violating claims to list")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}

	id := domain.OntologyVersionID(*versionID)
	if *versionID == "" {
		latest, err := a.Ontology.Latest(ctx, scope.WorkspaceID)
		if err != nil {
			return err
		}
		id = latest.ID
	}

	report, err := a.Ontology.Validate(ctx, scope, id, *limit)
	if err != nil {
		return err
	}

	fmt.Printf("ontology version %d (%s)\n", report.Version.Version, report.Version.Name)
	fmt.Printf("  %d claim(s) checked, %d conforming, %d violating\n",
		report.Checked, report.Conforming, len(report.Violations))

	if len(report.ByCode) > 0 {
		fmt.Println()
		tw := newTable("VIOLATION", "COUNT")
		for code, count := range report.ByCode {
			row(tw, string(code), fmt.Sprintf("%d", count))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(report.Violations) > 0 && *show > 0 {
		fmt.Println()
		shown := report.Violations
		if len(shown) > *show {
			shown = shown[:*show]
		}
		for _, violation := range shown {
			fmt.Printf("  %s %s %s\n", violation.Subject, violation.Predicate, violation.Object)
			for _, reason := range violation.Violations {
				fmt.Printf("      %s\n", reason)
			}
		}
		if len(report.Violations) > len(shown) {
			fmt.Printf("  ... and %d more\n", len(report.Violations)-len(shown))
		}
	}

	// Nothing was written. Saying so matters: a command named "validate" that quietly
	// quarantined things would be a bad surprise.
	fmt.Println("\nNothing was changed. This is a report, not a migration.")
	return nil
}
