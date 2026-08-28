// Package opensearch stores the lexical projection in a dedicated search engine
// (AGENTS.md phase 15).
//
// The third implementation behind an index port, and the second one written to find out
// whether a port is real rather than because a measurement demanded it. PostgreSQL's
// full-text search meets every section 39 target at the scales tested; what a dedicated
// engine buys is the demonstration that index.Lexical can be satisfied by something sharing
// no code, no query language and no storage with the reference.
//
// One decision shapes the mapping. The port has two search modes in one method, because they
// are one index: stemmed full text for prose, and literal substring for the identifiers and
// codes that stemming destroys. PostgreSQL serves those with a tsvector and an ILIKE over
// the same column. Here they are two views of one field — an analyzed `content` for the
// first and a `content.exact` wildcard subfield for the second — so a document is written
// once and read either way.
//
// The wildcard subfield is what makes this backend viable at all. A prefix or typo-tolerant
// engine could serve the prose half and not the other, and weakening the contract to fit a
// backend is what phase 15 says not to do.
package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/index"
)

// Name identifies this backend on a startup log and in the recovery report.
const Name = "opensearch"

// namespace seeds the derived document ids. Fixed and never to be changed: every id is a
// function of it, so a new one orphans every document.
var namespace = uuid.MustParse("2b8a1c4e-9f13-4d77-8c21-7a5f0e3b9d42")

// Options configure the backend.
type Options struct {
	// Addresses are the cluster endpoints.
	Addresses []string
	// Username and Password authenticate, when the security plugin is enabled.
	Username string
	Password string
	// Index holds every workspace's records. Tenancy is a filter, as it is in the
	// reference: one index keeps the analyzers and the mapping in one place.
	Index string
}

func (o Options) withDefaults() Options {
	if len(o.Addresses) == 0 {
		o.Addresses = []string{"http://127.0.0.1:9200"}
	}
	if o.Index == "" {
		o.Index = "strata_lexical"
	}
	return o
}

// Store is the OpenSearch-backed lexical index.
type Store struct {
	api   *opensearchapi.Client
	index string
}

// Open connects and provisions the index.
func Open(ctx context.Context, opts Options) (*Store, error) {
	const op = "opensearch.Open"

	opts = opts.withDefaults()
	api, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearch.Config{
			Addresses: opts.Addresses,
			Username:  opts.Username,
			Password:  opts.Password,
		},
	})
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot connect to OpenSearch")
	}

	store := &Store{api: api, index: opts.Index}
	if err := store.ensureIndex(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// Name identifies the backend.
func (s *Store) Name() string { return Name }

// mapping is the index definition.
//
// Written out rather than left to dynamic mapping, because a field OpenSearch guesses at is
// a field whose filter behaviour is a guess: a date inferred as text does not answer a range
// query, and a keyword inferred as text is analyzed and stops matching exactly.
const mapping = `{
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "workspace_id":      {"type": "keyword"},
      "graph_space_id":    {"type": "keyword"},
      "collection_id":     {"type": "keyword"},
      "record_id":         {"type": "keyword"},
      "surface":           {"type": "keyword"},
      "status":            {"type": "keyword"},
      "classification":    {"type": "keyword"},
      "memory_kind":       {"type": "keyword"},
      "entity_type":       {"type": "keyword"},
      "predicate":         {"type": "keyword"},
      "source_id":         {"type": "keyword"},
      "source_event_id":   {"type": "keyword"},
      "content":           {
        "type": "text",
        "analyzer": "english",
        "fields": {"exact": {"type": "wildcard"}}
      },
      "valid_from":        {"type": "date"},
      "valid_to":          {"type": "date"},
      "active_from":       {"type": "date"},
      "active_until":      {"type": "date"},
      "expires_at":        {"type": "date"},
      "decay_starts_at":   {"type": "date"}
    }
  }
}`

func (s *Store) ensureIndex(ctx context.Context) error {
	const op = "opensearch.ensureIndex"

	exists, err := s.api.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{Indices: []string{s.index}})
	if err == nil && exists != nil && exists.StatusCode == http.StatusOK {
		return nil
	}

	if _, err := s.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: s.index,
		Body:  strings.NewReader(mapping),
	}); err != nil {
		// A concurrent creator is not a failure: two workers starting together both try.
		if strings.Contains(err.Error(), "resource_already_exists_exception") {
			return nil
		}
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot create the index")
	}
	return nil
}

// documentID derives a stable identifier from the projection's key.
//
// The same three columns as the reference's unique index. Derived rather than stored, so an
// upsert converges on replay and a workspace cannot address another's document.
func documentID(ws domain.WorkspaceID, surface domain.Surface, recordID string) string {
	name := fmt.Sprintf("%s\x1f%s\x1f%s", ws, surface, recordID)
	return uuid.NewSHA1(namespace, []byte(name)).String()
}

// stamp renders a timestamp, or nil so the field is absent.
//
// Absent rather than a sentinel, unlike the Qdrant adapter: OpenSearch has a first-class
// exists query, so "NULL means unbounded" is expressible directly and a sentinel date would
// be a value someone could stumble into.
func stamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *Store) documentOf(r domain.ProjectedRecord) map[string]any {
	return map[string]any{
		"workspace_id":    string(r.Scope.WorkspaceID),
		"graph_space_id":  string(r.Scope.GraphSpaceID),
		"collection_id":   string(r.Scope.CollectionID),
		"record_id":       r.RecordID,
		"surface":         string(r.Surface),
		"status":          r.Status,
		"classification":  string(r.Classification),
		"memory_kind":     string(r.MemoryKind),
		"entity_type":     r.EntityType,
		"predicate":       r.Predicate,
		"source_id":       string(r.SourceID),
		"source_event_id": string(r.SourceEventID),
		"content":         r.Content,
		"valid_from":      stamp(r.ValidFrom),
		"valid_to":        stamp(r.ValidTo),
		"active_from":     stamp(r.Lifecycle.ActiveFrom),
		"active_until":    stamp(r.Lifecycle.ActiveUntil),
		"expires_at":      stamp(r.Lifecycle.ExpiresAt),
		"decay_starts_at": stamp(r.Lifecycle.DecayStartsAt),
	}
}

// Upsert writes records, replacing whatever was at the same derived id.
func (s *Store) Upsert(ctx context.Context, records []domain.ProjectedRecord) error {
	const op = "opensearch.Upsert"

	if len(records) == 0 {
		return nil
	}

	var body bytes.Buffer
	for _, r := range records {
		id := documentID(r.Scope.WorkspaceID, r.Surface, r.RecordID)
		action := map[string]any{"index": map[string]any{"_index": s.index, "_id": id}}
		if err := json.NewEncoder(&body).Encode(action); err != nil {
			return domain.Wrap(err, domain.CodeInternal, op, "cannot encode a bulk action")
		}
		if err := json.NewEncoder(&body).Encode(s.documentOf(r)); err != nil {
			return domain.Wrap(err, domain.CodeInternal, op, "cannot encode a record")
		}
	}

	// Refreshed, not merely acknowledged. OpenSearch is near-real-time by default and the
	// port requires a write to be visible to the next read — a rebuild purges and
	// immediately starts writing, and the projector reads back what it just wrote.
	response, err := s.api.Bulk(ctx, opensearchapi.BulkReq{
		Body:   bytes.NewReader(body.Bytes()),
		Params: opensearchapi.BulkParams{Refresh: "true"},
	})
	if err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot write records")
	}
	if response != nil && response.Errors {
		return domain.Errorf(domain.CodeProviderUnavailable, op,
			"the search engine rejected part of the batch")
	}
	return nil
}

// Search returns matching records, with every filter applied before ranking.
func (s *Store) Search(ctx context.Context, q domain.LexicalQuery) ([]domain.Hit, error) {
	const op = "opensearch.Search"

	if strings.TrimSpace(q.Text) == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "query text is required")
	}
	if domain.IsZero(q.Scope.WorkspaceID) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "workspace scope is required")
	}

	limit := q.Limit
	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = 20
	}

	must, empty := matchClause(q)
	if empty {
		return nil, nil
	}
	filters, impossible := s.filters(q)
	if impossible {
		return nil, nil
	}

	body, err := json.Marshal(map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{"must": must, "filter": filters},
		},
		// Everything the hit needs and nothing else: content is returned because a lexical
		// hit carries its text, and the decay clock because ranking uses it.
		"_source": []string{"surface", "record_id", "content", "decay_starts_at"},
	})
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeInternal, op, "cannot encode the query")
	}

	response, err := s.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{s.index},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot search")
	}

	mode := "lexical"
	if q.Exact {
		mode = "lexical_exact"
	}
	now := time.Now().UTC()
	if q.ActiveAt != nil {
		now = q.ActiveAt.UTC()
	}

	out := make([]domain.Hit, 0, len(response.Hits.Hits))
	for _, raw := range response.Hits.Hits {
		var source struct {
			Surface       string  `json:"surface"`
			RecordID      string  `json:"record_id"`
			Content       string  `json:"content"`
			DecayStartsAt *string `json:"decay_starts_at"`
		}
		if err := json.Unmarshal(raw.Source, &source); err != nil {
			return nil, domain.Wrap(err, domain.CodeInternal, op, "cannot decode a hit")
		}

		hit := domain.Hit{
			Surface:  domain.Surface(source.Surface),
			RecordID: source.RecordID,
			Content:  source.Content,
			Score:    float64(raw.Score),
		}
		hit.Decay = domain.Lifecycle{DecayStartsAt: parseStamp(source.DecayStartsAt)}.
			DecayWeight(now, domain.DecayHalfLife)
		hit.Detail = map[string]any{"retriever": mode, "rank": hit.Score, "decay": hit.Decay}
		out = append(out, hit)
	}
	return out, nil
}

func parseStamp(value *string) *time.Time {
	if value == nil || *value == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return nil
	}
	at = at.UTC()
	return &at
}

// Purge removes every record in a workspace.
func (s *Store) Purge(ctx context.Context, ws domain.WorkspaceID) error {
	const op = "opensearch.Purge"

	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"workspace_id": string(ws)}},
	})
	if err != nil {
		return domain.Wrap(err, domain.CodeInternal, op, "cannot encode the purge")
	}

	refresh := true
	if _, err := s.api.Document.DeleteByQuery(ctx, opensearchapi.DocumentDeleteByQueryReq{
		Indices: []string{s.index},
		Body:    bytes.NewReader(body),
		Params:  opensearchapi.DocumentDeleteByQueryParams{Refresh: &refresh},
	}); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot purge the projection")
	}
	return nil
}

// Count reports how many records a workspace holds.
func (s *Store) Count(ctx context.Context, ws domain.WorkspaceID) (int, error) {
	const op = "opensearch.Count"

	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"workspace_id": string(ws)}},
	})
	if err != nil {
		return 0, domain.Wrap(err, domain.CodeInternal, op, "cannot encode the count")
	}

	response, err := s.api.Indices.Count(ctx, &opensearchapi.IndicesCountReq{
		Indices: []string{s.index},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		return 0, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot count projected records")
	}
	return response.Count, nil
}

// Ensure the adapter satisfies the port.
var _ index.Lexical = (*Store)(nil)
