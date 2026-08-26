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

	// BlobBackend selects where raw source material is archived: fs or s3.
	//
	// The filesystem is correct and has no versioning or replication of its own, which
	// makes protecting the bytes every claim is cited to the deployment's problem. s3
	// hands that to an object store (AGENTS.md section 40.2).
	BlobBackend string
	BlobDir     string

	// S3 settings, used when CG_BLOB_BACKEND=s3. The endpoint makes this work with any
	// S3-compatible store, not only AWS.
	S3Bucket   string
	S3Prefix   string
	S3Endpoint string
	S3Region   string
	// S3AccessKey and S3Secret are optional: leaving both empty uses the ambient
	// credential chain, which is how a deployment on AWS avoids holding credentials at
	// all. The secret is never logged.
	S3AccessKey string
	S3Secret    string
	// S3PathStyle addresses buckets as endpoint/bucket/key. Required by most self-hosted
	// implementations, which have no wildcard DNS for virtual-host addressing.
	S3PathStyle bool
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
	// WorkerMaxEventsPerSecond caps how fast one worker takes on new work, 0 for
	// uncapped. A fleet that can outrun a downstream model or a database is a fleet
	// that turns a spike into an outage; this is the throttle that keeps a scaled-out
	// deployment inside what its dependencies can serve (AGENTS.md section 27.5).
	WorkerMaxEventsPerSecond float64
	// WorkerIdleBackoffMax is how far the poll interval stretches when there is no work.
	// Without it, fifty idle workers poll the ledger at the base interval forever, which
	// is a load pattern proportional to fleet size rather than to work.
	WorkerIdleBackoffMax time.Duration

	// VectorBackend selects where the vector projection lives: postgres or qdrant.
	//
	// The other two projections stay on PostgreSQL. Only this one has a second
	// implementation, and the measurements in docs/api/performance.md say PostgreSQL is
	// not the bottleneck at the scales tested — so this exists because the port is real,
	// not because the default is wrong.
	VectorBackend string
	// Qdrant settings, used when CG_VECTOR_BACKEND=qdrant. The port is the gRPC one.
	QdrantHost       string
	QdrantPort       int
	QdrantAPIKey     string
	QdrantUseTLS     bool
	QdrantCollection string

	// NATSURL enables push delivery through JetStream. Empty keeps the deployment on
	// pure ledger polling, which is correct and simply slower to react.
	//
	// The ledger stays the durability boundary either way: the bus carries a copy of
	// work that is already committed, so losing the bus costs latency, not work.
	NATSURL string
	// NATSStream names the JetStream stream. Two deployments sharing one NATS cluster
	// need distinct names: a work-queue stream may not overlap another's subjects.
	NATSStream string
	// NATSDurable names the consumer group. Workers sharing it share the queue, which
	// is how a fleet scales without processing anything twice.
	NATSDurable string
	// NATSAckWait is how long a delivery may go unacknowledged before redelivery. It is
	// the bus's lease and should be reasoned about like CG_WORKER_LEASE.
	NATSAckWait time.Duration
	// NATSMaxAckPending bounds unacknowledged deliveries per consumer: the backpressure
	// that makes a slow worker slow its own intake instead of hoarding a backlog.
	NATSMaxAckPending int

	PipelineVersion    int
	ChunkMaxTokens     int
	ChunkOverlapTokens int

	// LLMProvider selects the extraction model provider: none, mock, or openai.
	//
	// It defaults to none, so a deployment with no model configured still ingests,
	// segments, and chunks. Extraction is added when a provider is chosen rather than
	// being a requirement for the system to run.
	LLMProvider   string
	LLMBaseURL    string
	LLMModel      string
	LLMAPIKey     string
	LLMTimeout    time.Duration
	LLMMaxRetries int

	// RedactQueryText stores the hash of a query without its words. For deployments where
	// what people asked is itself sensitive (AGENTS.md section 6.12).
	RedactQueryText bool

	// EmbeddingProvider selects the embedder for the vector projection: none, mock,
	// hashing, or openai. Without one, lexical and graph retrieval still work.
	//
	// "hashing" is feature hashing computed locally: real cosine structure, no API key,
	// and no generalization across synonyms. It is what makes vector retrieval work
	// offline; "mock" is a deterministic hash with no structure at all and is only a test
	// double.
	EmbeddingProvider string
	EmbeddingBaseURL  string
	EmbeddingModel    string
	EmbeddingAPIKey   string
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

		BlobBackend: strings.ToLower(l.str("BLOB_BACKEND", "fs")),
		BlobDir:     l.str("BLOB_DIR", "./.data/blobs"),

		S3Bucket:    l.str("S3_BUCKET", ""),
		S3Prefix:    l.str("S3_PREFIX", ""),
		S3Endpoint:  l.str("S3_ENDPOINT", ""),
		S3Region:    l.str("S3_REGION", "us-east-1"),
		S3AccessKey: l.str("S3_ACCESS_KEY_ID", ""),
		S3Secret:    l.str("S3_SECRET_ACCESS_KEY", ""),
		S3PathStyle: l.boolVal("S3_PATH_STYLE", false),
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

		WorkerMaxEventsPerSecond: l.floatVal("WORKER_MAX_EVENTS_PER_SECOND", 0),
		WorkerIdleBackoffMax:     l.duration("WORKER_IDLE_BACKOFF_MAX", 5*time.Second),

		VectorBackend:    strings.ToLower(l.str("VECTOR_BACKEND", "postgres")),
		QdrantHost:       l.str("QDRANT_HOST", "127.0.0.1"),
		QdrantPort:       l.intVal("QDRANT_PORT", 6334),
		QdrantAPIKey:     l.str("QDRANT_API_KEY", ""),
		QdrantUseTLS:     l.boolVal("QDRANT_USE_TLS", false),
		QdrantCollection: l.str("QDRANT_COLLECTION", "strata_vectors"),

		NATSURL: l.str("NATS_URL", ""),
		// The literal rather than natsbus.StreamName: configuration should not have to
		// import a provider adapter to know its own default.
		NATSStream:        l.str("NATS_STREAM", "STRATA_WORK"),
		NATSDurable:       l.str("NATS_DURABLE", "strata-workers"),
		NATSAckWait:       l.duration("NATS_ACK_WAIT", 60*time.Second),
		NATSMaxAckPending: l.intVal("NATS_MAX_ACK_PENDING", 256),

		LLMProvider:   strings.ToLower(l.str("LLM_PROVIDER", "none")),
		LLMBaseURL:    l.str("LLM_BASE_URL", ""),
		LLMModel:      l.str("LLM_MODEL", ""),
		LLMAPIKey:     l.str("LLM_API_KEY", ""),
		LLMTimeout:    l.duration("LLM_TIMEOUT", 60*time.Second),
		LLMMaxRetries: l.intVal("LLM_MAX_RETRIES", 2),

		RedactQueryText:   l.boolVal("REDACT_QUERY_TEXT", false),
		EmbeddingProvider: strings.ToLower(l.str("EMBEDDING_PROVIDER", "none")),
		EmbeddingBaseURL:  l.str("EMBEDDING_BASE_URL", ""),
		EmbeddingModel:    l.str("EMBEDDING_MODEL", ""),
		EmbeddingAPIKey:   l.str("EMBEDDING_API_KEY", ""),

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
	switch c.BlobBackend {
	case "fs":
		if c.BlobDir == "" {
			problems = append(problems, "CG_BLOB_DIR is required for the fs backend")
		}
	case "s3":
		if c.S3Bucket == "" {
			problems = append(problems, "CG_S3_BUCKET is required for the s3 backend")
		}
	default:
		problems = append(problems, "CG_BLOB_BACKEND must be fs or s3")
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
	switch c.VectorBackend {
	case "postgres", "qdrant":
	default:
		problems = append(problems, "CG_VECTOR_BACKEND must be postgres or qdrant")
	}
	if c.VectorBackend == "qdrant" && c.QdrantHost == "" {
		problems = append(problems, "CG_QDRANT_HOST is required for the qdrant backend")
	}
	if c.WorkerMaxEventsPerSecond < 0 {
		problems = append(problems, "CG_WORKER_MAX_EVENTS_PER_SECOND cannot be negative; use 0 for uncapped")
	}
	// Backoff that never exceeds the base is no backoff at all, and a fleet of idle
	// workers would keep polling at full rate.
	if c.WorkerIdleBackoffMax != 0 && c.WorkerIdleBackoffMax < c.WorkerPollInterval {
		problems = append(problems, "CG_WORKER_IDLE_BACKOFF_MAX must be at least CG_WORKER_POLL_INTERVAL")
	}
	if c.NATSURL != "" && c.NATSAckWait <= 0 {
		problems = append(problems, "CG_NATS_ACK_WAIT must be positive")
	}
	if c.NATSURL != "" && c.NATSMaxAckPending < 1 {
		problems = append(problems, "CG_NATS_MAX_ACK_PENDING must be at least 1")
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
	switch c.LLMProvider {
	case "none", "mock", "hashing":
	case "openai":
		// Fail at startup rather than at the first extraction: a worker that starts
		// happily and then dead-letters every event is worse than one that will not start.
		if c.LLMModel == "" {
			problems = append(problems, "CG_LLM_MODEL is required when CG_LLM_PROVIDER is openai")
		}
	default:
		problems = append(problems, "CG_LLM_PROVIDER must be none, mock, or openai")
	}
	if c.LLMMaxRetries < 0 {
		problems = append(problems, "CG_LLM_MAX_RETRIES must not be negative")
	}
	switch c.EmbeddingProvider {
	case "none", "mock", "hashing":
	case "openai":
		if c.EmbeddingModel == "" {
			problems = append(problems,
				"CG_EMBEDDING_MODEL is required when CG_EMBEDDING_PROVIDER is openai")
		}
	default:
		problems = append(problems,
			"CG_EMBEDDING_PROVIDER must be none, mock, hashing, or openai")
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
		"env":          c.Env,
		"service_name": c.ServiceName,
		"http_addr":    c.HTTPAddr,
		"database":     redactDSN(c.DatabaseURL),
		"blob_backend": c.BlobBackend,
		// The bucket and endpoint, never the credentials.
		"blob_location":      c.blobLocation(),
		"log_level":          c.LogLevel,
		"otlp_enabled":       c.OTLPEndpoint != "",
		"embedded_worker":    c.EmbeddedWorker,
		"worker_concurrency": c.WorkerConcurrency,
		"pipeline_version":   c.PipelineVersion,
		// The URL, never credentials: a NATS DSN can carry a token in its userinfo.
		"nats_enabled": c.NATSURL != "",
		"nats_stream":  c.NATSStream,
		"nats_durable": c.NATSDurable,
	}
}

// blobLocation describes where artifacts live, without disclosing how to reach them.
func (c Config) blobLocation() string {
	if c.BlobBackend == "s3" {
		location := c.S3Bucket
		if c.S3Prefix != "" {
			location += "/" + c.S3Prefix
		}
		if c.S3Endpoint != "" {
			location += " @ " + c.S3Endpoint
		}
		return location
	}
	return c.BlobDir
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

func (l *loader) floatVal(key string, def float64) float64 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s%s must be a number, got %q", prefix, key, v))
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
