// Package config loads runtime configuration from the environment.
//
// Configuration is environment-first: every key has a CG_ prefix and a documented
// default (see configs/dev.env). Secrets are never committed and never logged
// (AGENTS.md sections 22.5, 30.3).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// Config is the resolved configuration for every Strata process. One struct serves
// the server, the worker, and the CLI so a deployment cannot configure them
// inconsistently.
type Config struct {
	Env         string
	ServiceName string

	HTTPAddr        string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	MaxBodyBytes    int64

	DatabaseURL string
	DBMaxConns  int32
	DBMinConns  int32
	// AutoMigrate applies pending migrations at startup. Safe with several instances
	// because the runner holds an advisory lock, and it keeps single-node development
	// to one command. Disable it where migrations are a separate deployment step.
	AutoMigrate bool

	BlobDir     string
	APIKeysFile string

	LogLevel     string
	LogFormat    string
	OTLPEndpoint string

	// EmbeddedWorker runs the outbox consumer inside contextgraphd. Convenient for
	// single-node development; production runs cgworker separately (section 14).
	EmbeddedWorker     bool
	WorkerConcurrency  int
	WorkerBatchSize    int
	WorkerLease        time.Duration
	WorkerPollInterval time.Duration
	WorkerMaxAttempts  int
	BackoffBase        time.Duration
	BackoffMax         time.Duration

	PipelineVersion    int
	ChunkMaxTokens     int
	ChunkOverlapTokens int
}

// Load reads configuration from the process environment.
func Load() (Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration using an arbitrary lookup, which keeps the loader
// testable without mutating process state.
func LoadFrom(getenv func(string) string) (Config, error) {
	l := &loader{getenv: getenv}

	cfg := Config{
		Env:         l.str("ENV", "dev"),
		ServiceName: l.str("SERVICE_NAME", "strata"),

		HTTPAddr:        l.str("HTTP_ADDR", ":8080"),
		RequestTimeout:  l.duration("REQUEST_TIMEOUT", 30*time.Second),
		ShutdownTimeout: l.duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		MaxBodyBytes:    int64(l.intVal("MAX_BODY_BYTES", 8<<20)),

		DatabaseURL: l.str("DATABASE_URL", ""),
		AutoMigrate: l.boolVal("AUTO_MIGRATE", true),
		DBMaxConns:  int32(l.intVal("DB_MAX_CONNS", 10)),
		DBMinConns:  int32(l.intVal("DB_MIN_CONNS", 0)),

		BlobDir:     l.str("BLOB_DIR", "./.data/blobs"),
		APIKeysFile: l.str("API_KEYS_FILE", "./configs/api-keys.json"),

		LogLevel:     strings.ToLower(l.str("LOG_LEVEL", "info")),
		LogFormat:    strings.ToLower(l.str("LOG_FORMAT", "json")),
		OTLPEndpoint: l.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),

		EmbeddedWorker:     l.boolVal("EMBEDDED_WORKER", false),
		WorkerConcurrency:  l.intVal("WORKER_CONCURRENCY", 4),
		WorkerBatchSize:    l.intVal("WORKER_BATCH_SIZE", 16),
		WorkerLease:        l.duration("WORKER_LEASE", 60*time.Second),
		WorkerPollInterval: l.duration("WORKER_POLL_INTERVAL", 500*time.Millisecond),
		WorkerMaxAttempts:  l.intVal("WORKER_MAX_ATTEMPTS", 8),
		BackoffBase:        l.duration("BACKOFF_BASE", time.Second),
		BackoffMax:         l.duration("BACKOFF_MAX", 5*time.Minute),

		PipelineVersion:    l.intVal("PIPELINE_VERSION", 1),
		ChunkMaxTokens:     l.intVal("CHUNK_MAX_TOKENS", 320),
		ChunkOverlapTokens: l.intVal("CHUNK_OVERLAP_TOKENS", 48),
	}

	if len(l.errs) > 0 {
		return Config{}, domain.Errorf(domain.CodeInvalidArgument, "config.Load",
			"invalid configuration: %s", strings.Join(l.errs, "; "))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects configurations that would fail later in a harder-to-diagnose
// place, and reports every problem at once rather than one per restart.
func (c Config) Validate() error {
	const op = "config.Validate"
	var problems []string

	if c.DatabaseURL == "" {
		problems = append(problems, "CG_DATABASE_URL is required")
	}
	if c.HTTPAddr == "" {
		problems = append(problems, "CG_HTTP_ADDR is required")
	}
	if c.BlobDir == "" {
		problems = append(problems, "CG_BLOB_DIR is required")
	}
	if c.DBMaxConns < 1 {
		problems = append(problems, "CG_DB_MAX_CONNS must be at least 1")
	}
	if c.DBMinConns < 0 || c.DBMinConns > c.DBMaxConns {
		problems = append(problems, "CG_DB_MIN_CONNS must be between 0 and CG_DB_MAX_CONNS")
	}
	if c.WorkerConcurrency < 1 {
		problems = append(problems, "CG_WORKER_CONCURRENCY must be at least 1")
	}
	if c.WorkerBatchSize < 1 {
		problems = append(problems, "CG_WORKER_BATCH_SIZE must be at least 1")
	}
	if c.WorkerMaxAttempts < 1 {
		problems = append(problems, "CG_WORKER_MAX_ATTEMPTS must be at least 1")
	}
	// A lease shorter than the poll interval would expire claims faster than the
	// worker can renew them, turning healthy work into repeated redelivery.
	if c.WorkerLease <= c.WorkerPollInterval {
		problems = append(problems, "CG_WORKER_LEASE must exceed CG_WORKER_POLL_INTERVAL")
	}
	if c.BackoffBase <= 0 || c.BackoffMax < c.BackoffBase {
		problems = append(problems, "CG_BACKOFF_MAX must be at least CG_BACKOFF_BASE, and both positive")
	}
	if c.PipelineVersion < 1 {
		problems = append(problems, "CG_PIPELINE_VERSION must be at least 1")
	}
	if c.ChunkMaxTokens < 16 {
		problems = append(problems, "CG_CHUNK_MAX_TOKENS must be at least 16")
	}
	if c.ChunkOverlapTokens < 0 || c.ChunkOverlapTokens >= c.ChunkMaxTokens {
		problems = append(problems, "CG_CHUNK_OVERLAP_TOKENS must be between 0 and CG_CHUNK_MAX_TOKENS-1")
	}
	if c.MaxBodyBytes < 1024 {
		problems = append(problems, "CG_MAX_BODY_BYTES must be at least 1024")
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		problems = append(problems, "CG_LOG_FORMAT must be json or text")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, "CG_LOG_LEVEL must be debug, info, warn, or error")
	}

	if len(problems) > 0 {
		return domain.Errorf(domain.CodeInvalidArgument, op,
			"invalid configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Redacted renders configuration for startup logs with the database URL's
// credentials removed. Configuration is logged; secrets are not.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"env":                c.Env,
		"service_name":       c.ServiceName,
		"http_addr":          c.HTTPAddr,
		"database":           redactDSN(c.DatabaseURL),
		"blob_dir":           c.BlobDir,
		"log_level":          c.LogLevel,
		"otlp_enabled":       c.OTLPEndpoint != "",
		"embedded_worker":    c.EmbeddedWorker,
		"worker_concurrency": c.WorkerConcurrency,
		"pipeline_version":   c.PipelineVersion,
	}
}

// redactDSN strips userinfo from a connection string so a password can never reach
// a log sink.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return "[redacted]"
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = "[redacted]@" + rest[at+1:]
	}
	return scheme + "://" + rest
}

const prefix = "CG_"

type loader struct {
	getenv func(string) string
	errs   []string
}

func (l *loader) raw(key string) (string, bool) {
	v := l.getenv(prefix + key)
	if v == "" {
		return "", false
	}
	return v, true
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) intVal(key string, def int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s%s must be an integer, got %q", prefix, key, v))
		return def
	}
	return n
}

func (l *loader) boolVal(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s%s must be a boolean, got %q", prefix, key, v))
		return def
	}
	return b
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s%s must be a duration such as 30s, got %q", prefix, key, v))
		return def
	}
	return d
}
