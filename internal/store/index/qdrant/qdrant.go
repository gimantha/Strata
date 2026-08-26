// Package qdrant stores the vector projection in a dedicated vector database
// (AGENTS.md phase 15).
//
// The second implementation of index.Vectors, and the reason the port exists. PostgreSQL
// with pgvector remains the reference and is not deprecated by this — the measurements in
// docs/api/performance.md say it is not the bottleneck at the scales tested. What this buys
// is the demonstration that the port is real: a backend that shares no code, no query
// language, and no storage engine with the reference, held to the same behaviour by the same
// suite.
//
// Three decisions shape everything here, and each is a place a naive port goes wrong.
//
// Point identity is derived, not stored. Qdrant point ids are UUIDs or integers, while the
// projection's key is a five-part tuple, so the id is a UUIDv5 over that tuple. That is what
// makes an upsert converge on replay, and it makes tenancy structural: another workspace
// asking for the same record computes a different id and finds nothing.
//
// Absent timestamps are written as sentinels rather than omitted. PostgreSQL's temporal
// clauses read "NULL means unbounded", which needs two branches per column in SQL and none
// at all if the unbounded case is a real value. It is also the only encoding under which a
// metadata refresh can clear a timestamp, because Qdrant's set_payload merges keys and cannot
// remove one it is not told about.
//
// Writes wait. Qdrant acknowledges a write before it is searchable unless asked otherwise,
// and the port requires a purge to be visible to the next read — a rebuild purges and
// immediately starts writing. That is correctness rather than tuning, so it is not
// configurable.
package qdrant

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	client "github.com/qdrant/go-client/qdrant"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/index"
)

// Name identifies this backend on a startup log and in the recovery report.
const Name = "qdrant"

// Dimensions is the embedding width the projection uses everywhere else.
const Dimensions = 1536

// namespace seeds the derived point ids.
//
// A fixed constant, checked in, and never to be changed: every id is a function of it, so a
// new namespace orphans every point in every deployment and forces a full re-embed.
var namespace = uuid.MustParse("6f3c1f1e-6f77-4e6a-9a2a-0f7c2c9c1a11")

// Sentinels standing for an absent timestamp, in microseconds since the epoch.
//
// Chosen outside any representable instant and inside float64's exact-integer range, so a
// range comparison against them is exact and can never collide with a real timestamp:
// -8e15µs is around 251,000 BCE and +8e15µs around 255,000 CE.
const (
	unboundedBelow = -8_000_000_000_000_000
	unboundedAbove = 8_000_000_000_000_000
)

// Options configure the backend.
type Options struct {
	// Host and Port address the gRPC API, which is 6334 rather than the REST 6333.
	Host string
	Port int
	// APIKey authenticates, and UseTLS is required by managed deployments.
	APIKey string
	UseTLS bool
	// Collection holds every workspace's vectors. Tenancy is a payload filter with
	// workspace_id marked as the tenant key, which is Qdrant's own multitenancy shape and
	// gives each tenant its own region of the index.
	Collection string
}

func (o Options) withDefaults() Options {
	if o.Host == "" {
		o.Host = "localhost"
	}
	if o.Port == 0 {
		o.Port = 6334
	}
	if o.Collection == "" {
		o.Collection = "strata_vectors"
	}
	return o
}

// Store is the Qdrant-backed vector index.
type Store struct {
	client *client.Client
	opts   Options
}

// Open connects and provisions the collection.
func Open(ctx context.Context, opts Options) (*Store, error) {
	const op = "qdrant.Open"

	opts = opts.withDefaults()
	conn, err := client.NewClient(&client.Config{
		Host:   opts.Host,
		Port:   opts.Port,
		APIKey: opts.APIKey,
		UseTLS: opts.UseTLS,
		// The client reads its own version from the build info, which is empty under
		// `go test`, so the check reports a spurious mismatch on every test run.
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot connect to Qdrant")
	}

	store := &Store{client: conn, opts: opts}
	if err := store.ensureCollection(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the connection.
func (s *Store) Close() error { return s.client.Close() }

// Name identifies the backend.
func (s *Store) Name() string { return Name }

// payloadIndexes are the fields a filter touches, with the type each needs.
//
// Every one of them is required. Without an index Qdrant plans a filtered search from
// estimated cardinality and can return fewer points than match, which surfaces as a recall
// problem and gets blamed on the embedding model.
var payloadIndexes = []struct {
	field string
	kind  client.FieldType
}{
	{"workspace_id", client.FieldType_FieldTypeUuid},
	{"graph_space_id", client.FieldType_FieldTypeKeyword},
	{"collection_id", client.FieldType_FieldTypeKeyword},
	{"record_id", client.FieldType_FieldTypeKeyword},
	{"surface", client.FieldType_FieldTypeKeyword},
	{"embedding_model", client.FieldType_FieldTypeKeyword},
	{"embedding_version", client.FieldType_FieldTypeInteger},
	{"status", client.FieldType_FieldTypeKeyword},
	{"classification", client.FieldType_FieldTypeKeyword},
	{"memory_kind", client.FieldType_FieldTypeKeyword},
	{"entity_type", client.FieldType_FieldTypeKeyword},
	{"predicate", client.FieldType_FieldTypeKeyword},
	{"source_id", client.FieldType_FieldTypeKeyword},
	{"valid_from", client.FieldType_FieldTypeInteger},
	{"valid_to", client.FieldType_FieldTypeInteger},
	{"active_from", client.FieldType_FieldTypeInteger},
	{"active_until", client.FieldType_FieldTypeInteger},
	{"expires_at", client.FieldType_FieldTypeInteger},
}

// ensureCollection creates the collection and its payload indexes if they are absent.
//
// Idempotent, and deliberately not lazy. Payload indexes only give the HNSW graph
// filter-aware edges for indexes that existed when the graph was built, so provisioning has
// to happen before the first write rather than on it.
func (s *Store) ensureCollection(ctx context.Context) error {
	const op = "qdrant.ensureCollection"

	exists, err := s.client.CollectionExists(ctx, s.opts.Collection)
	if err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot inspect the collection")
	}
	if !exists {
		if err := s.client.CreateCollection(ctx, &client.CreateCollection{
			CollectionName: s.opts.Collection,
			VectorsConfig: client.NewVectorsConfig(&client.VectorParams{
				Size: Dimensions,
				// Cosine, matching the reference's `1 - (embedding <=> $1)`. Qdrant
				// normalizes on write and returns similarity, so scores are directly
				// comparable and need no rescaling.
				Distance: client.Distance_Cosine,
			}),
		}); err != nil {
			return domain.Wrap(err, domain.CodeProviderUnavailable, op,
				"cannot create the collection")
		}
	}

	wait := true
	isTenant := true
	for _, idx := range payloadIndexes {
		request := &client.CreateFieldIndexCollection{
			CollectionName: s.opts.Collection,
			FieldName:      idx.field,
			FieldType:      idx.kind.Enum(),
			Wait:           &wait,
		}
		if idx.field == "workspace_id" {
			// The tenant key. Qdrant groups a tenant's points together on disk and gives
			// them their own graph edges, which is both the recall fix for a strict
			// workspace filter and the isolation the port promises.
			request.FieldIndexParams = client.NewPayloadIndexParamsUUID(
				&client.UuidIndexParams{IsTenant: &isTenant})
		}
		if _, err := s.client.CreateFieldIndex(ctx, request); err != nil {
			return domain.Wrap(err, domain.CodeProviderUnavailable, op,
				"cannot create the payload index for "+idx.field)
		}
	}
	return nil
}

// pointID derives a stable identifier from the projection's key.
//
// The same five columns as the reference's unique index, and deliberately not graph_space_id:
// a record re-derived into another graph space must land on the same point and update it,
// which is what "a projection reflects the ledger" means. The separator is a byte no
// component can contain, so two different tuples cannot render to one name.
func pointID(ws domain.WorkspaceID, surface domain.Surface, recordID, model string, version int) string {
	name := fmt.Sprintf("%s\x1f%s\x1f%s\x1f%s\x1f%d", ws, surface, recordID, model, version)
	return uuid.NewSHA1(namespace, []byte(name)).String()
}

// stamp renders a timestamp, or the sentinel standing for its absence.
func stamp(t *time.Time, absent int64) int64 {
	if t == nil {
		return absent
	}
	return t.UTC().UnixMicro()
}

// unstamp reverses stamp, turning a sentinel back into nil.
//
// Needed because decay is computed from decay_starts_at, and a sentinel reaching
// DecayWeight would produce a confident and wrong ranking weight rather than an error.
func unstamp(micros int64) *time.Time {
	if micros <= unboundedBelow || micros >= unboundedAbove {
		return nil
	}
	at := time.UnixMicro(micros).UTC()
	return &at
}

// payloadOf renders the filterable fields of a record.
//
// Every key is always present, including the ones standing for NULL. That is what lets a
// filter be a plain range comparison rather than a two-branch test, and it is what lets a
// metadata refresh clear a timestamp — set_payload merges, so a key that is not sent keeps
// whatever it had.
func payloadOf(r domain.ProjectedRecord, model string, version int) map[string]any {
	return map[string]any{
		"workspace_id":      string(r.Scope.WorkspaceID),
		"graph_space_id":    string(r.Scope.GraphSpaceID),
		"collection_id":     string(r.Scope.CollectionID),
		"record_id":         r.RecordID,
		"surface":           string(r.Surface),
		"embedding_model":   model,
		"embedding_version": int64(version),
		"status":            r.Status,
		"classification":    string(r.Classification),
		"memory_kind":       string(r.MemoryKind),
		"entity_type":       r.EntityType,
		"predicate":         r.Predicate,
		// The empty string rather than a missing key, matching the reference's nullable
		// source_id: a must_not condition then behaves the same for a source-less record
		// in both engines without a special case.
		"source_id":       string(r.SourceID),
		"source_event_id": string(r.SourceEventID),
		"valid_from":      stamp(r.ValidFrom, unboundedBelow),
		"valid_to":        stamp(r.ValidTo, unboundedAbove),
		"active_from":     stamp(r.Lifecycle.ActiveFrom, unboundedBelow),
		"active_until":    stamp(r.Lifecycle.ActiveUntil, unboundedAbove),
		"expires_at":      stamp(r.Lifecycle.ExpiresAt, unboundedAbove),
		"decay_starts_at": stamp(r.Lifecycle.DecayStartsAt, unboundedAbove),
	}
}

// Upsert writes vectors, replacing whatever was at the same derived id.
func (s *Store) Upsert(ctx context.Context, records []domain.VectorRecord) error {
	const op = "qdrant.Upsert"

	if len(records) == 0 {
		return nil
	}

	points := make([]*client.PointStruct, 0, len(records))
	for _, r := range records {
		payload := payloadOf(r.ProjectedRecord, r.Model, r.Version)
		payload["content_hash"] = r.ContentHash

		points = append(points, &client.PointStruct{
			Id: client.NewIDUUID(pointID(r.Scope.WorkspaceID, r.Surface, r.RecordID,
				r.Model, r.Version)),
			Vectors: client.NewVectorsDense(r.Embedding),
			Payload: client.NewValueMap(payload),
		})
	}

	wait := true
	if _, err := s.client.Upsert(ctx, &client.UpsertPoints{
		CollectionName: s.opts.Collection, Wait: &wait, Points: points,
	}); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot write vectors")
	}
	return nil
}

// RefreshMetadata updates the filter payload without touching the vector.
func (s *Store) RefreshMetadata(ctx context.Context, model string, version int,
	records []domain.ProjectedRecord) error {
	const op = "qdrant.RefreshMetadata"

	if len(records) == 0 {
		return nil
	}

	wait := true
	operations := make([]*client.PointsUpdateOperation, 0, len(records))
	for _, r := range records {
		// Selected by filter rather than by id. A set_payload addressed to an id that does
		// not exist fails the whole batch with NotFound and does not roll back the
		// operations that already ran; a filter matching nothing is simply a no-op, which
		// is the right behaviour for a refresh of something not yet embedded.
		operations = append(operations, client.NewPointsUpdateSetPayload(
			&client.PointsUpdateOperation_SetPayload{
				PointsSelector: client.NewPointsSelectorFilter(&client.Filter{
					Must: identityConditions(r.Scope.WorkspaceID, r.Surface, r.RecordID,
						model, version),
				}),
				Payload: client.NewValueMap(payloadOf(r, model, version)),
			}))
	}

	if _, err := s.client.UpdateBatch(ctx, &client.UpdateBatchPoints{
		CollectionName: s.opts.Collection, Wait: &wait, Operations: operations,
	}); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot refresh vector metadata")
	}
	return nil
}

// identityConditions matches exactly one record by the projection's key.
func identityConditions(ws domain.WorkspaceID, surface domain.Surface, recordID, model string,
	version int) []*client.Condition {
	return []*client.Condition{
		client.NewMatchKeyword("workspace_id", string(ws)),
		client.NewMatchKeyword("surface", string(surface)),
		client.NewMatchKeyword("record_id", recordID),
		client.NewMatchKeyword("embedding_model", model),
		client.NewMatchInt("embedding_version", int64(version)),
	}
}

// ExistingHashes reports the content hash stored with each named record.
func (s *Store) ExistingHashes(ctx context.Context, ws domain.WorkspaceID, model string,
	version int, surface domain.Surface, recordIDs []string) (map[string]string, error) {
	const op = "qdrant.ExistingHashes"

	out := map[string]string{}
	if len(recordIDs) == 0 {
		return out, nil
	}

	ids := make([]*client.PointId, 0, len(recordIDs))
	for _, recordID := range recordIDs {
		ids = append(ids, client.NewIDUUID(pointID(ws, surface, recordID, model, version)))
	}

	points, err := s.client.Get(ctx, &client.GetPoints{
		CollectionName: s.opts.Collection,
		Ids:            ids,
		WithPayload:    client.NewWithPayloadInclude("record_id", "content_hash", "workspace_id"),
		// 1536 floats per point, for a lookup that reads two strings.
		WithVectors: client.NewWithVectorsEnable(false),
	})
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot read vector hashes")
	}

	for _, point := range points {
		payload := point.GetPayload()
		// Tenancy is already structural — the id is derived from the workspace — but the
		// payload is checked anyway, because a read that leaks across tenants is not the
		// place to rely on one mechanism.
		if payload["workspace_id"].GetStringValue() != string(ws) {
			continue
		}
		out[payload["record_id"].GetStringValue()] = payload["content_hash"].GetStringValue()
	}
	return out, nil
}

// Search returns the nearest records, with every filter applied before ranking.
func (s *Store) Search(ctx context.Context, q domain.VectorQuery) ([]domain.Hit, error) {
	const op = "qdrant.Search"

	if len(q.Embedding) == 0 {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "a query embedding is required")
	}
	if domain.IsZero(q.Scope.WorkspaceID) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "workspace scope is required")
	}

	must, mustNot, empty := s.conditions(q)
	if empty {
		// A permitted set with nothing in it means nothing qualifies. Sending an empty
		// match to Qdrant is unspecified, so the answer is produced here instead.
		return nil, nil
	}

	limit := uint64(q.Limit)
	if limit == 0 || limit > uint64(domain.MaxAssertionLimit) {
		limit = 20
	}

	scored, err := s.client.Query(ctx, &client.QueryPoints{
		CollectionName: s.opts.Collection,
		Query:          client.NewQueryDense(q.Embedding),
		Filter:         &client.Filter{Must: must, MustNot: mustNot},
		Limit:          &limit,
		WithPayload:    client.NewWithPayloadInclude("surface", "record_id", "decay_starts_at"),
		WithVectors:    client.NewWithVectorsEnable(false),
	})
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot search vectors")
	}

	now := time.Now().UTC()
	if q.ActiveAt != nil {
		now = q.ActiveAt.UTC()
	}

	out := make([]domain.Hit, 0, len(scored))
	for _, point := range scored {
		payload := point.GetPayload()
		score := float64(point.GetScore())
		// The floor is applied here rather than through Qdrant's score_threshold, which
		// compares strictly. The reference drops a hit when score < MinScore, so a hit
		// exactly at the floor survives, and a MinScore of zero really does remove
		// negative similarities rather than meaning "no floor".
		if score < q.MinScore {
			continue
		}

		hit := domain.Hit{
			Surface:  domain.Surface(payload["surface"].GetStringValue()),
			RecordID: payload["record_id"].GetStringValue(),
			Score:    score,
			Detail:   map[string]any{"retriever": "vector", "cosine_similarity": score},
		}
		hit.Decay = domain.Lifecycle{
			DecayStartsAt: unstamp(payload["decay_starts_at"].GetIntegerValue()),
		}.DecayWeight(now, domain.DecayHalfLife)
		out = append(out, hit)
	}

	// Qdrant does not promise an order among equal scores, and the reference sorts by
	// score then record id so that repeated queries and successive pages agree.
	slices.SortFunc(out, func(a, b domain.Hit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		default:
			return cmpString(a.RecordID, b.RecordID)
		}
	})
	return out, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Purge removes every record in a workspace.
func (s *Store) Purge(ctx context.Context, ws domain.WorkspaceID) error {
	const op = "qdrant.Purge"

	wait := true
	if _, err := s.client.Delete(ctx, &client.DeletePoints{
		CollectionName: s.opts.Collection, Wait: &wait,
		Points: client.NewPointsSelectorFilter(&client.Filter{
			Must: []*client.Condition{client.NewMatchKeyword("workspace_id", string(ws))},
		}),
	}); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot purge the projection")
	}
	return nil
}

// Count reports how many records a workspace holds.
func (s *Store) Count(ctx context.Context, ws domain.WorkspaceID) (int, error) {
	const op = "qdrant.Count"

	// Exact, because Qdrant's default is an estimate and the recovery drill's whole claim
	// is that a rebuild restored exactly what was dropped. At small scale an estimate
	// agrees, so the default would pass every test and mislead in production.
	exact := true
	count, err := s.client.Count(ctx, &client.CountPoints{
		CollectionName: s.opts.Collection, Exact: &exact,
		Filter: &client.Filter{
			Must: []*client.Condition{client.NewMatchKeyword("workspace_id", string(ws))},
		},
	})
	if err != nil {
		return 0, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot count projected records")
	}
	if count > math.MaxInt {
		return math.MaxInt, nil
	}
	return int(count), nil
}

// Ensure the adapter satisfies the port.
var _ index.Vectors = (*Store)(nil)
