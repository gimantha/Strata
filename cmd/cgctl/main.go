// Command cgctl is the administration and development CLI.
//
// It talks to PostgreSQL directly and goes through the same services the API uses, so
// an operator action is subject to the same authorization and audit rules as a request.
// It selects a configured principal with --key-id rather than bypassing policy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/identity"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/store/ledger"
)

const usage = `cgctl - Strata administration CLI

Usage:
  cgctl <command> [flags]

Commands:
  migrate                  Apply pending schema migrations
  keygen                   Mint an API key and print its file entry
  workspace create|ls      Manage workspaces
  graph-space create|ls    Manage graph spaces
  source register|ls       Manage sources
  grant                    Grant a principal access to a workspace
  ingest file              Ingest a file as a source event
  event status             Show an event's processing status
  process                  Run the pipeline for an event now
  outbox ls|retry          Inspect and revive durable work
  predicate define|ls      Manage the predicate registry
  entity ls                List entities in a graph space
  assert                   Record a claim against a source event
  ask                      Query knowledge, optionally as of a past instant
  provenance               Walk a claim back to its source
  conflicts                List recorded disagreements
  version                  Print the schema version this binary expects

Run "cgctl <command> -h" for command flags.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "cgctl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	command := args[0]
	rest := args[1:]

	switch command {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "keygen":
		return cmdKeygen(rest)
	case "version":
		return cmdVersion()
	case "migrate":
		return withApp(rest, cmdMigrate)
	case "workspace":
		return withSubcommand(rest, map[string]appCommand{"create": cmdWorkspaceCreate, "ls": cmdWorkspaceList})
	case "graph-space":
		return withSubcommand(rest, map[string]appCommand{"create": cmdGraphSpaceCreate, "ls": cmdGraphSpaceList})
	case "source":
		return withSubcommand(rest, map[string]appCommand{"register": cmdSourceRegister, "ls": cmdSourceList})
	case "grant":
		return withApp(rest, cmdGrant)
	case "ingest":
		return withSubcommand(rest, map[string]appCommand{"file": cmdIngestFile})
	case "event":
		return withSubcommand(rest, map[string]appCommand{"status": cmdEventStatus})
	case "process":
		return withApp(rest, cmdProcess)
	case "outbox":
		return withSubcommand(rest, map[string]appCommand{"ls": cmdOutboxList, "retry": cmdOutboxRetry})
	case "predicate":
		return withSubcommand(rest, map[string]appCommand{"define": cmdPredicateDefine, "ls": cmdPredicateList})
	case "entity":
		return withSubcommand(rest, map[string]appCommand{"ls": cmdEntityList})
	case "assert":
		return withApp(rest, cmdAssert)
	case "ask":
		return withApp(rest, cmdAsk)
	case "provenance":
		return withApp(rest, cmdProvenance)
	case "conflicts":
		return withApp(rest, cmdConflicts)
	default:
		return fmt.Errorf("unknown command %q; run \"cgctl help\"", command)
	}
}

type appCommand func(ctx context.Context, a *app.App, args []string) error

func withSubcommand(args []string, commands map[string]appCommand) error {
	if len(args) == 0 {
		names := make([]string, 0, len(commands))
		for name := range commands {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("expected one of: %s", strings.Join(names, ", "))
	}
	cmd, ok := commands[args[0]]
	if !ok {
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
	return withApp(args[1:], cmd)
}

// withApp builds the application, runs a command, and shuts down cleanly.
func withApp(args []string, fn appCommand) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The CLI never starts a background worker, and it applies migrations only when
	// explicitly asked to.
	cfg.EmbeddedWorker = false
	cfg.AutoMigrate = false

	application, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = application.Close(ctx) }()

	return fn(ctx, application, args)
}

func cmdVersion() error {
	head, err := ledger.EmbeddedHead()
	if err != nil {
		return err
	}
	fmt.Printf("expected schema version: %d\n", head)
	return nil
}

func cmdMigrate(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	applied, err := a.Ledger.Migrate(ctx, a.Logger)
	if err != nil {
		return err
	}
	version, err := a.Ledger.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("applied %d migration(s); schema is at version %d\n", applied, version)
	return nil
}

// cmdKeygen mints a credential. The secret is printed once and only its hash is stored,
// so the key file can be committed to a secret manager without holding the secret
// itself.
func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	principalID := fs.String("principal", "", "principal id (required)")
	displayName := fs.String("name", "", "display name")
	kind := fs.String("kind", string(domain.PrincipalUser), "principal kind: user, agent, service")
	role := fs.String("system-role", string(domain.RoleReader), "system role: reader, writer, admin, owner")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *principalID == "" {
		return errors.New("--principal is required")
	}

	principalKind, err := domain.ParsePrincipalKind(*kind)
	if err != nil {
		return err
	}
	systemRole, err := domain.ParseRole(*role)
	if err != nil {
		return err
	}

	name := *displayName
	if name == "" {
		name = *principalID
	}

	keyID, secret, entry, err := identity.GenerateKey(*principalID, name, principalKind, systemRole)
	if err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(map[string]any{
		"version": 1,
		"keys":    []any{entry},
	}, "", "  ")
	if err != nil {
		return err
	}

	fmt.Printf("credential (send as \"Authorization: Bearer %s\"):\n  %s\n\n",
		identity.FormatCredential(keyID, secret), identity.FormatCredential(keyID, secret))
	fmt.Printf("store this in your API key file; the secret above is not recoverable from it:\n%s\n", encoded)
	return nil
}

func cmdWorkspaceCreate(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("workspace create", flag.ContinueOnError)
	slug := fs.String("slug", "", "workspace slug (required)")
	name := fs.String("name", "", "display name")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeSystem(principal, domain.RoleAdmin); err != nil {
		return err
	}

	ws, err := a.Ledger.CreateWorkspace(ctx, domain.Workspace{
		Slug: *slug, Name: orDefault(*name, *slug),
	}, principal.ID)
	if err != nil {
		return err
	}
	fmt.Printf("workspace %s created (slug %s), owned by %s\n", ws.ID, ws.Slug, principal.ID)
	return nil
}

func cmdWorkspaceList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("workspace ls", flag.ContinueOnError)
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	list, err := a.Ledger.ListWorkspaces(ctx, principal.ID)
	if err != nil {
		return err
	}

	tw := newTable("ID", "SLUG", "NAME", "CREATED")
	for _, ws := range list {
		row(tw, string(ws.ID), ws.Slug, ws.Name, ws.CreatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

func cmdGraphSpaceCreate(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("graph-space create", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	slug := fs.String("slug", "", "graph space slug (required)")
	name := fs.String("name", "", "display name")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, a, *workspace)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleAdmin); err != nil {
		return err
	}

	gs, err := a.Ledger.CreateGraphSpace(ctx, domain.GraphSpace{
		WorkspaceID: ws, Slug: *slug, Name: orDefault(*name, *slug),
	}, principal.ID)
	if err != nil {
		return err
	}
	fmt.Printf("graph space %s created (slug %s) in workspace %s\n", gs.ID, gs.Slug, ws)
	return nil
}

func cmdGraphSpaceList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("graph-space ls", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, a, *workspace)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleReader); err != nil {
		return err
	}

	list, err := a.Ledger.ListGraphSpaces(ctx, ws)
	if err != nil {
		return err
	}
	tw := newTable("ID", "SLUG", "NAME", "CREATED")
	for _, gs := range list {
		row(tw, string(gs.ID), gs.Slug, gs.Name, gs.CreatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

func cmdSourceRegister(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("source register", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	kind := fs.String("kind", string(domain.SourceKindFile), "source kind")
	name := fs.String("name", "", "source name, unique per workspace (required)")
	uri := fs.String("uri", "", "source URI")
	trust := fs.String("trust", "", "trust level: untrusted, low, standard, high, authoritative")
	classification := fs.String("classification", "", "classification: public, internal, confidential, restricted, secret")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, a, *workspace)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleAdmin); err != nil {
		return err
	}

	sourceKind, err := domain.ParseSourceKind(*kind)
	if err != nil {
		return err
	}
	src := domain.Source{WorkspaceID: ws, Kind: sourceKind, Name: *name, URI: *uri}
	if *trust != "" {
		level, err := domain.ParseTrustLevel(*trust)
		if err != nil {
			return err
		}
		src.TrustLevel = level
	}
	if *classification != "" {
		class, err := domain.ParseClassification(*classification)
		if err != nil {
			return err
		}
		src.Classification = class
	}

	created, err := a.Ledger.CreateSource(ctx, src, principal.ID)
	if err != nil {
		return err
	}
	fmt.Printf("source %s registered (%s, trust %s, %s)\n",
		created.ID, created.Name, created.TrustLevel, created.Classification)
	return nil
}

func cmdSourceList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("source ls", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, a, *workspace)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleReader); err != nil {
		return err
	}

	list, err := a.Ledger.ListSources(ctx, ws)
	if err != nil {
		return err
	}
	tw := newTable("ID", "NAME", "KIND", "TRUST", "CLASSIFICATION")
	for _, src := range list {
		row(tw, string(src.ID), src.Name, string(src.Kind), string(src.TrustLevel), string(src.Classification))
	}
	return tw.Flush()
}

func cmdGrant(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("grant", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	target := fs.String("principal", "", "principal id to grant (required)")
	role := fs.String("role", string(domain.RoleWriter), "role: reader, writer, admin, owner")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, a, *workspace)
	if err != nil {
		return err
	}
	// Only an owner may widen access to a workspace.
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleOwner); err != nil {
		return err
	}
	granted, err := domain.ParseRole(*role)
	if err != nil {
		return err
	}

	if err := a.Ledger.Grant(ctx, domain.PrincipalID(*target), ws, granted, principal.ID); err != nil {
		return err
	}
	fmt.Printf("granted %s the %s role on workspace %s\n", *target, granted, ws)
	return nil
}

func cmdIngestFile(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("ingest file", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	source := fs.String("source", "", "source name or id (required)")
	path := fs.String("file", "", "path to the file to ingest (required)")
	externalID := fs.String("external-id", "", "upstream identifier")
	sourceVersion := fs.String("source-version", "", "upstream version")
	idempotencyKey := fs.String("idempotency-key", "", "caller-supplied idempotency key")
	mediaType := fs.String("media-type", "", "media type; inferred from the extension when omitted")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--file is required")
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	scope, err := a.Identity.ResolveGraphSpace(ctx, principal, domain.GraphSpaceID(*graphSpace), domain.RoleWriter)
	if err != nil {
		return err
	}

	payload, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("read %s: %w", *path, err)
	}

	req := ingest.Request{
		Scope:          scope,
		Principal:      principal.Ref(),
		SourceName:     *source,
		ExternalID:     *externalID,
		SourceVersion:  *sourceVersion,
		MediaType:      orDefault(*mediaType, mediaTypeForPath(*path)),
		Payload:        payload,
		IdempotencyKey: *idempotencyKey,
		EventType:      "file.upload",
		Metadata:       map[string]any{"filename": filepath.Base(*path)},
	}
	if domain.ValidUUID(domain.SourceID(*source)) {
		req.SourceID = domain.SourceID(*source)
		req.SourceName = ""
	}

	receipt, err := a.Gateway.Accept(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("source event %s (%s)\n  duplicate: %t\n  content hash: %s\n  idempotency key: %s\n",
		receipt.SourceEventID, receipt.Status, receipt.Duplicate, receipt.ContentHash, receipt.IdempotencyKey)
	return nil
}

func cmdEventStatus(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("event status", flag.ContinueOnError)
	eventID := fs.String("id", "", "source event id (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *eventID == "" && fs.NArg() > 0 {
		*eventID = fs.Arg(0)
	}
	if *eventID == "" {
		return errors.New("--id is required")
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	allowed := make([]domain.WorkspaceID, 0, len(principal.Grants))
	for _, grant := range principal.Grants {
		allowed = append(allowed, grant.WorkspaceID)
	}
	ws, err := a.Ledger.ResolveSourceEventWorkspace(ctx, domain.SourceEventID(*eventID), allowed)
	if err != nil {
		return err
	}

	status, err := a.Ledger.SourceEventStatus(ctx, ws, domain.SourceEventID(*eventID))
	if err != nil {
		return err
	}

	fmt.Printf("source event %s\n  status: %s\n  operation: %s\n  episodes: %d\n  chunks: %d\n",
		status.Event.ID, status.Event.Status, status.Event.Operation, status.Episodes, status.Chunks)
	if status.Run != nil {
		fmt.Printf("  pipeline v%d: %s\n", status.Run.PipelineVersion, status.Run.Status)
		for _, stage := range status.Stages {
			detail := ""
			if stage.LastError != "" {
				detail = fmt.Sprintf(" (%s: %s)", stage.ErrorClass, stage.LastError)
			}
			fmt.Printf("    %-10s v%d %-9s attempts=%d %s%s\n",
				stage.StageName, stage.StageVersion, stage.Status, stage.Attempts,
				strings.TrimSpace(string(stage.OutputRef)), detail)
		}
	}
	for _, item := range status.Work {
		fmt.Printf("  work %s %s attempts=%d visible_at=%s %s\n",
			item.ID, item.Status, item.Attempts, item.VisibleAt.Format(time.RFC3339), item.LastError)
	}
	return nil
}

// cmdProcess runs the pipeline synchronously, which is how an operator reprocesses an
// event without waiting for a worker to pick it up.
func cmdProcess(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("process", flag.ContinueOnError)
	eventID := fs.String("event", "", "source event id (required)")
	force := fs.Bool("force", false, "re-run stages that already succeeded")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *eventID == "" {
		return errors.New("--event is required")
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	allowed := make([]domain.WorkspaceID, 0, len(principal.Grants))
	for _, grant := range principal.Grants {
		allowed = append(allowed, grant.WorkspaceID)
	}
	ws, err := a.Ledger.ResolveSourceEventWorkspace(ctx, domain.SourceEventID(*eventID), allowed)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleWriter); err != nil {
		return err
	}

	result, err := a.Runner.Process(ctx, ws, domain.SourceEventID(*eventID), *force)
	if err != nil {
		return err
	}
	fmt.Printf("processed event %s: %d stage(s) run, %d skipped as already complete\n",
		*eventID, result.StagesRun, result.StagesSkipped)
	return nil
}

func cmdOutboxList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("outbox ls", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	status := fs.String("status", "", "filter by status: pending, claimed, succeeded, dead, cancelled")
	limit := fs.Int("limit", 50, "maximum rows")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, a, *workspace)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleReader); err != nil {
		return err
	}

	var filter domain.OutboxStatus
	if *status != "" {
		parsed, err := domain.ParseOutboxStatus(*status)
		if err != nil {
			return err
		}
		filter = parsed
	}

	items, err := a.Ledger.ListOutbox(ctx, ws, filter, *limit)
	if err != nil {
		return err
	}
	tw := newTable("ID", "EVENT TYPE", "STATUS", "ATTEMPTS", "VISIBLE AT", "ERROR")
	for _, item := range items {
		row(tw, string(item.ID), item.EventType, string(item.Status),
			fmt.Sprint(item.Attempts), item.VisibleAt.Format(time.RFC3339), truncate(item.LastError, 60))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	depth, err := a.Ledger.OutboxDepth(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nqueue depth: %v\noldest claimable item: %.1fs\n", depth.ByStatus, depth.OldestPendingAge)
	return nil
}

// cmdOutboxRetry revives dead-lettered work after its cause has been fixed. Dead items
// are never discarded, so this is always available (AGENTS.md section 28.4).
func cmdOutboxRetry(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("outbox retry", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	id := fs.String("id", "", "specific outbox id; all dead items when omitted")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, a, *workspace)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleWriter); err != nil {
		return err
	}

	var ids []domain.OutboxEventID
	if *id != "" {
		ids = append(ids, domain.OutboxEventID(*id))
	}
	revived, err := a.Ledger.ReviveDeadLetters(ctx, ws, ids)
	if err != nil {
		return err
	}
	fmt.Printf("returned %d dead-lettered item(s) to the queue\n", revived)
	return nil
}

// resolvePrincipal selects the principal to act as.
//
// It never fabricates one: policy checks are the same here as in the API, so an
// operator without a configured key cannot act (AGENTS.md section 44.8).
func resolvePrincipal(ctx context.Context, a *app.App, keyID string) (domain.Principal, error) {
	available := a.Identity.KeyIDs()
	if len(available) == 0 {
		return domain.Principal{}, fmt.Errorf(
			"no API keys configured at %s; run \"cgctl keygen --principal <id> --system-role admin\" first",
			a.Config.APIKeysFile)
	}
	if keyID == "" {
		if len(available) > 1 {
			sort.Strings(available)
			return domain.Principal{}, fmt.Errorf(
				"several keys are configured; choose one with --key-id (%s)", strings.Join(available, ", "))
		}
		keyID = available[0]
	}
	return a.Identity.PrincipalForKeyID(ctx, keyID)
}

// resolveWorkspace accepts an id or a slug, so operators can use readable names.
func resolveWorkspace(ctx context.Context, a *app.App, value string) (domain.WorkspaceID, error) {
	if value == "" {
		return "", errors.New("--workspace is required")
	}
	if domain.ValidUUID(domain.WorkspaceID(value)) {
		return domain.WorkspaceID(value), nil
	}
	ws, err := a.Ledger.GetWorkspaceBySlug(ctx, value)
	if err != nil {
		return "", err
	}
	return ws.ID, nil
}

func mediaTypeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return normalize.MediaTypeJSON
	case ".md", ".markdown":
		return normalize.MediaTypeMarkdown
	default:
		return normalize.MediaTypePlain
	}
}

func newTable(headers ...string) *tabwriter.Writer {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	return tw
}

func row(tw *tabwriter.Writer, cells ...string) {
	fmt.Fprintln(tw, strings.Join(cells, "\t"))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
