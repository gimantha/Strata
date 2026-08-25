// Package pgtest provides a real PostgreSQL database for integration tests.
//
// Integration tests run against real PostgreSQL, never a mock, because the behavior
// under test is transactional and index-dependent (AGENTS.md section 32.3).
//
// A database is resolved in this order:
//
//  1. TEST_DATABASE_URL, if set (CI service container, or a shared dev cluster).
//  2. An ephemeral cluster booted with initdb/pg_ctl, if those binaries exist.
//     This keeps "go test ./..." working on a developer machine with no Docker.
//  3. A server already listening on one of this project's own ports, which is how a
//     machine with Docker but no PostgreSQL installation gets a database. See
//     localCandidates for why only those ports are tried.
//  4. Otherwise the test skips - unless CG_REQUIRE_PG=1, which turns the skip into a
//     failure so CI cannot silently lose integration coverage.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/store/ledger"
)

// templateDB is per process: Go runs one test binary per package, often in parallel,
// and a shared template name would make them race to create it.
var templateDB = fmt.Sprintf("strata_test_tmpl_%d", os.Getpid())

var (
	baseOnce sync.Once
	baseDSN  string
	baseErr  error
	cluster  *ephemeralCluster

	templateOnce sync.Once
	templateErr  error

	dbCounter atomic.Int64
)

// Main runs a package's tests and then tears down any ephemeral cluster. Integration
// test packages need:
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }
func Main(m *testing.M) {
	code := m.Run()

	// Drop this process's template so a shared cluster does not accumulate databases
	// across runs. The ephemeral cluster is discarded wholesale instead.
	if cluster == nil && baseDSN != "" && templateErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = adminExec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, templateDB))
		cancel()
	}
	if cluster != nil {
		cluster.stop()
	}
	os.Exit(code)
}

// Available reports whether a database can be provided, without failing a test.
func Available() bool {
	resolveBase()
	return baseErr == nil
}

// DSN returns a migrated, isolated database for this test and drops it afterwards.
//
// testing.TB rather than *testing.T so benchmarks can use it too: the performance targets
// in AGENTS.md section 39 have to be measured against a real database for the same reason
// the integration tests are.
func DSN(t testing.TB) string {
	t.Helper()

	resolveBase()
	if baseErr != nil {
		if os.Getenv("CG_REQUIRE_PG") == "1" {
			t.Fatalf("CG_REQUIRE_PG=1 but no PostgreSQL is available: %v", baseErr)
		}
		t.Skipf("skipping integration test: no PostgreSQL available (%v)", baseErr)
	}

	ensureTemplate(t)

	name := fmt.Sprintf("strata_test_%d_%d", os.Getpid(), dbCounter.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Cloning the migrated template is much cheaper than re-running migrations for
	// every test, and gives each test a private database.
	if err := adminExec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, name, templateDB)); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		_ = adminExec(dropCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})

	return withDatabase(baseDSN, name)
}

// Pool returns a connection pool to a migrated, isolated database.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()

	dsn := DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Store returns a ledger store backed by a migrated, isolated database.
func Store(t testing.TB) *ledger.Store {
	t.Helper()
	return ledger.NewStore(Pool(t))
}

// Config returns a configuration pointed at a migrated, isolated database, with
// blob storage in a temporary directory.
func Config(t testing.TB) config.Config {
	t.Helper()

	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "CG_DATABASE_URL":
			return DSN(t)
		case "CG_BLOB_DIR":
			return filepath.Join(t.TempDir(), "blobs")
		case "CG_API_KEYS_FILE":
			return filepath.Join(t.TempDir(), "api-keys.json")
		case "CG_LOG_LEVEL":
			return "error"
		case "CG_ENV":
			return "test"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("build test config: %v", err)
	}
	return cfg
}

// ensureTemplate migrates one template database that every test clones.
func ensureTemplate(t testing.TB) {
	t.Helper()

	templateOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		if err := adminExec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, templateDB)); err != nil {
			templateErr = err
			return
		}
		if err := adminExec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, templateDB)); err != nil {
			templateErr = err
			return
		}

		pool, err := pgxpool.New(ctx, withDatabase(baseDSN, templateDB))
		if err != nil {
			templateErr = err
			return
		}
		defer pool.Close()

		if _, err := ledger.NewStore(pool).Migrate(ctx, nil); err != nil {
			templateErr = err
			return
		}
	})

	if templateErr != nil {
		t.Fatalf("prepare migrated template database: %v", templateErr)
	}
}

// adminExec runs a statement (CREATE/DROP DATABASE) outside any transaction.
func adminExec(ctx context.Context, sql string) error {
	pool, err := pgxpool.New(ctx, withDatabase(baseDSN, "postgres"))
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%s: %w", strings.SplitN(sql, " ", 3)[0], err)
	}
	return nil
}

// withDatabase rewrites the database name in a postgres URL.
func withDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

func resolveBase() {
	baseOnce.Do(func() {
		if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
			baseDSN = dsn
			return
		}
		c, err := startEphemeral()
		if err == nil {
			cluster = c
			baseDSN = c.dsn
			return
		}

		// No server binaries. A developer on macOS typically has Docker instead, and
		// this project's own tooling puts a container on a known port.
		if dsn, found := discoverLocal(); found {
			fmt.Fprintf(os.Stderr, "pgtest: using PostgreSQL discovered at %s "+
				"(set TEST_DATABASE_URL to override)\n", displayHost(dsn))
			baseDSN = dsn
			return
		}
		baseErr = fmt.Errorf("%w; and no server with pgvector is listening on "+
			"127.0.0.1:55432 or 127.0.0.1:55433", err)
	})
}

// localCandidates are the endpoints this project's own tooling creates: scripts/dev-postgres.sh
// and the container command in the README. They are deliberately non-default ports.
//
// The default 5432 is not probed and should not be added. A developer's real database very
// often listens there, and a test harness that creates and drops databases in whatever it
// finds on the default port is a bad surprise waiting to happen. A listener on 55432 or 55433
// is one this repository told someone to start.
var localCandidates = []string{
	"postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable",
	"postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable",
	"postgres://postgres:postgres@127.0.0.1:55433/postgres?sslmode=disable",
	"postgres://postgres@127.0.0.1:55433/postgres?sslmode=disable",
}

// discoverLocal returns the first reachable candidate that can actually run the migrations.
//
// Reachability alone is not enough: the projections migration needs pgvector, and a plain
// postgres:16 container answers connections perfectly well right up to the point where every
// integration test fails on a missing extension. Checking here turns that into a skip with a
// clear reason instead.
func discoverLocal() (string, bool) {
	for _, dsn := range localCandidates {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ok := probe(ctx, dsn)
		cancel()
		if ok {
			return dsn, true
		}
	}
	return "", false
}

func probe(ctx context.Context, dsn string) bool {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return false
	}
	defer pool.Close()

	for _, extension := range []string{"vector", "pg_trgm"} {
		var present bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = $1)`,
			extension).Scan(&present); err != nil || !present {
			return false
		}
	}
	return true
}

// displayHost renders a DSN without its credentials, for a log line.
func displayHost(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "the configured server"
	}
	return u.Host
}

// ephemeralCluster is a throwaway PostgreSQL instance listening on a unix socket in
// a temporary directory.
type ephemeralCluster struct {
	binDir  string
	dataDir string
	sockDir string
	runAs   string
	dsn     string
}

func startEphemeral() (*ephemeralCluster, error) {
	binDir, err := findBinDir()
	if err != nil {
		return nil, err
	}

	root, err := os.MkdirTemp("", "strata-pgtest-")
	if err != nil {
		return nil, err
	}
	c := &ephemeralCluster{
		binDir:  binDir,
		dataDir: filepath.Join(root, "data"),
		sockDir: filepath.Join(root, "sock"),
	}
	if err := os.MkdirAll(c.sockDir, 0o777); err != nil {
		return nil, err
	}

	// PostgreSQL refuses to run as root. When the test process is root (containers
	// often are), drop to a service account and hand it the directories.
	if os.Geteuid() == 0 {
		c.runAs = os.Getenv("PGTEST_RUN_AS_USER")
		if c.runAs == "" {
			c.runAs = "postgres"
		}
		if _, err := exec.LookPath("su"); err != nil {
			return nil, fmt.Errorf("running as root and su is unavailable, so PostgreSQL cannot be started: %w", err)
		}
		if out, err := exec.Command("chown", "-R", c.runAs, root).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("chown %s to %s: %v: %s", root, c.runAs, err, out)
		}
		if err := os.Chmod(root, 0o777); err != nil {
			return nil, err
		}
	}

	if out, err := c.run(filepath.Join(binDir, "initdb"),
		"-D", c.dataDir, "-A", "trust", "-U", "postgres", "--no-sync"); err != nil {
		return nil, fmt.Errorf("initdb: %v: %s", err, out)
	}

	// fsync off: this cluster is discarded when the test binary exits.
	opts := fmt.Sprintf("-h '' -k %s -c fsync=off -c full_page_writes=off -c synchronous_commit=off", c.sockDir)
	if out, err := c.run(filepath.Join(binDir, "pg_ctl"),
		"-D", c.dataDir, "-l", filepath.Join(root, "server.log"), "-o", opts, "-w", "start"); err != nil {
		return nil, fmt.Errorf("pg_ctl start: %v: %s", err, out)
	}

	c.dsn = fmt.Sprintf("postgres://postgres@/postgres?host=%s", url.QueryEscape(c.sockDir))
	return c, nil
}

func (c *ephemeralCluster) stop() {
	out, err := c.run(filepath.Join(c.binDir, "pg_ctl"), "-D", c.dataDir, "-m", "immediate", "-w", "stop")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: could not stop ephemeral cluster: %v: %s\n", err, out)
	}
	_ = os.RemoveAll(filepath.Dir(c.dataDir))
}

// run executes a cluster command, dropping privileges when the process is root.
func (c *ephemeralCluster) run(bin string, args ...string) (string, error) {
	var cmd *exec.Cmd
	if c.runAs != "" {
		quoted := make([]string, 0, len(args)+1)
		quoted = append(quoted, shellQuote(bin))
		for _, a := range args {
			quoted = append(quoted, shellQuote(a))
		}
		cmd = exec.Command("su", c.runAs, "-c", strings.Join(quoted, " "))
	} else {
		cmd = exec.Command(bin, args...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// serverBinaryRoots are the directory patterns that hold PostgreSQL server binaries on
// the platforms this project is developed on. Each match is expected to contain a bin
// subdirectory; the highest major version wins.
var serverBinaryRoots = []string{
	"/usr/lib/postgresql/*",                          // Debian and Ubuntu packages
	"/opt/homebrew/opt/postgresql@*",                 // Homebrew, Apple silicon
	"/usr/local/opt/postgresql@*",                    // Homebrew, Intel macOS
	"/opt/homebrew/Cellar/postgresql@*/*",            // Homebrew cellar, unlinked
	"/usr/local/Cellar/postgresql@*/*",               // Homebrew cellar, unlinked
	"/Applications/Postgres.app/Contents/Versions/*", // Postgres.app
}

// findBinDir locates PostgreSQL server binaries, preferring an explicit override.
func findBinDir() (string, error) {
	if dir := os.Getenv("PG_BINDIR"); dir != "" {
		if ok, _ := hasServerBinaries(dir); ok {
			return dir, nil
		}
		return "", fmt.Errorf("PG_BINDIR=%s does not contain initdb and pg_ctl", dir)
	}

	if p, err := exec.LookPath("initdb"); err == nil {
		dir := filepath.Dir(p)
		if ok, _ := hasServerBinaries(dir); ok {
			return dir, nil
		}
	}

	// Packaged installations keep server binaries out of PATH, one directory per
	// version, in a location that differs per platform.
	best, bestVersion := "", -1
	for _, pattern := range serverBinaryRoots {
		roots, _ := filepath.Glob(pattern)
		for _, root := range roots {
			dir := filepath.Join(root, "bin")
			if ok, _ := hasServerBinaries(dir); !ok {
				continue
			}
			if v := majorVersion(root); v > bestVersion {
				best, bestVersion = dir, v
			}
		}
	}
	if best != "" {
		return best, nil
	}

	// The image matters: the projections migration needs pgvector, so plain postgres:16
	// connects fine and then fails every integration test on a missing extension.
	return "", fmt.Errorf("no PostgreSQL server binaries found; run scripts/dev-postgres.sh start " +
		"(it falls back to Docker), or set TEST_DATABASE_URL, " +
		"or point PG_BINDIR at a directory containing initdb and pg_ctl")
}

// majorVersion extracts a major version from an installation directory name, handling
// the plain "16" of a Debian package and the "postgresql@16" of a Homebrew formula.
// Unparseable names sort last rather than causing an error, since a wrong guess about
// version ordering should not stop a usable installation from being found.
func majorVersion(root string) int {
	name := filepath.Base(root)
	if _, after, found := strings.Cut(name, "@"); found {
		name = after
	}
	digits := 0
	for digits < len(name) && name[digits] >= '0' && name[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0
	}
	v, err := strconv.Atoi(name[:digits])
	if err != nil {
		return 0
	}
	return v
}

func hasServerBinaries(dir string) (bool, error) {
	for _, bin := range []string{"initdb", "pg_ctl"} {
		if _, err := os.Stat(filepath.Join(dir, bin)); err != nil {
			return false, err
		}
	}
	return true, nil
}
