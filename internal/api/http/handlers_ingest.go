package http

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/store/ledger"
)

// ingestEventRequest is one submitted source event.
//
// Note what is absent: there is no workspace field. Tenancy comes from the
// authenticated principal and the graph space in the path, so a caller cannot direct an
// event into another tenant (AGENTS.md section 22.1).
type ingestEventRequest struct {
	SourceID       string `json:"source_id,omitempty"`
	SourceName     string `json:"source_name,omitempty"`
	CollectionID   string `json:"collection_id,omitempty"`
	ExternalID     string `json:"external_id,omitempty"`
	EventType      string `json:"event_type,omitempty"`
	Operation      string `json:"operation,omitempty"`
	MediaType      string `json:"media_type,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Classification string `json:"classification,omitempty"`

	// Content carries the payload. Exactly one of content or content_json is used;
	// content_json exists so a caller can submit structured data without escaping it.
	Content     string          `json:"content,omitempty"`
	ContentJSON json.RawMessage `json:"content_json,omitempty"`

	EventTime        *time.Time `json:"event_time,omitempty"`
	SourceTime       *time.Time `json:"source_time,omitempty"`
	SourceCommitTime *time.Time `json:"source_commit_time,omitempty"`
	SourceSequence   string     `json:"source_sequence,omitempty"`
	SourceVersion    string     `json:"source_version,omitempty"`

	Metadata map[string]any `json:"metadata,omitempty"`
}

type receiptResponse struct {
	SourceEventID  string `json:"source_event_id"`
	ArtifactID     string `json:"artifact_id"`
	IdempotencyKey string `json:"idempotency_key"`
	ContentHash    string `json:"content_hash"`
	Status         string `json:"status"`
	Duplicate      bool   `json:"duplicate"`
}

type batchItemResponse struct {
	Index   int              `json:"index"`
	Receipt *receiptResponse `json:"receipt,omitempty"`
	Error   *errorBody       `json:"error,omitempty"`
}

func toReceiptResponse(rec ingest.Receipt) receiptResponse {
	return receiptResponse{
		SourceEventID:  string(rec.SourceEventID),
		ArtifactID:     string(rec.ArtifactID),
		IdempotencyKey: rec.IdempotencyKey,
		ContentHash:    rec.ContentHash,
		Status:         string(rec.Status),
		Duplicate:      rec.Duplicate,
	}
}

// handleIngestEvents accepts one event or an array of them.
//
// Both forms run the identical single-event path, which is what keeps batch import
// semantically identical to streaming ingestion (AGENTS.md section 10.1).
func (s *Server) handleIngestEvents(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleWriter)
	if !ok {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleIngestEvents",
			"could not read request body: it may exceed the size limit"))
		return
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleIngestEvents",
			"request body is required"))
		return
	}

	principal := principalFrom(r.Context())
	headerKey := r.Header.Get("Idempotency-Key")

	// Batch form.
	if trimmed[0] == '[' {
		var items []ingestEventRequest
		if err := json.Unmarshal(trimmed, &items); err != nil {
			s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleIngestEvents",
				"malformed batch body: %s", err.Error()))
			return
		}
		if len(items) == 0 {
			s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleIngestEvents",
				"batch must contain at least one event"))
			return
		}

		requests := make([]ingest.Request, 0, len(items))
		prepared := make([]*errorBody, len(items))
		for i, item := range items {
			// A batch-level idempotency key would collapse distinct events into one, so
			// it is deliberately not applied to batch items.
			req, err := s.buildIngestRequest(scope, principal, item, "")
			if err != nil {
				code := domain.CodeOf(err)
				prepared[i] = &errorBody{Code: code, Message: clientMessage(err)}
				requests = append(requests, ingest.Request{})
				continue
			}
			requests = append(requests, req)
		}

		results := make([]batchItemResponse, len(items))
		accepted, failed := 0, 0
		for i := range items {
			if prepared[i] != nil {
				results[i] = batchItemResponse{Index: i, Error: prepared[i]}
				failed++
				continue
			}
			receipt, err := s.gateway.Accept(r.Context(), requests[i])
			if err != nil {
				results[i] = batchItemResponse{Index: i, Error: &errorBody{
					Code: domain.CodeOf(err), Message: clientMessage(err),
				}}
				failed++
				continue
			}
			rec := toReceiptResponse(receipt)
			results[i] = batchItemResponse{Index: i, Receipt: &rec}
			accepted++
		}

		// 207 when some items failed: reporting 200 would hide partial loss, and
		// reporting 400 would hide the items that were durably accepted.
		status := http.StatusOK
		if failed > 0 {
			status = http.StatusMultiStatus
		}
		s.writeJSON(w, r, status, map[string]any{
			"accepted": accepted,
			"failed":   failed,
			"results":  results,
		})
		return
	}

	var item ingestEventRequest
	if err := json.Unmarshal(trimmed, &item); err != nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleIngestEvents",
			"malformed request body: %s", err.Error()))
		return
	}

	req, err := s.buildIngestRequest(scope, principal, item, headerKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.accept(w, r, req)
}

// handleIngestDocument accepts a raw document body, so a caller can upload a file
// without wrapping it in JSON. It becomes an ordinary source event.
func (s *Server) handleIngestDocument(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleWriter)
	if !ok {
		return
	}

	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleIngestDocument",
			"could not read request body: it may exceed the size limit"))
		return
	}

	query := r.URL.Query()
	req := ingest.Request{
		Scope:          scope,
		Principal:      principalFrom(r.Context()).Ref(),
		SourceID:       domain.SourceID(query.Get("source_id")),
		SourceName:     query.Get("source_name"),
		ExternalID:     query.Get("external_id"),
		EventType:      query.Get("event_type"),
		MediaType:      r.Header.Get("Content-Type"),
		Payload:        payload,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		SourceVersion:  query.Get("source_version"),
		SourceSequence: query.Get("source_sequence"),
	}
	if op := query.Get("operation"); op != "" {
		operation, err := domain.ParseSourceOperation(op)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		req.Operation = operation
	}
	if class := query.Get("classification"); class != "" {
		classification, err := domain.ParseClassification(class)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		req.Classification = classification
	}
	if collection := query.Get("collection_id"); collection != "" {
		req.Scope.CollectionID = domain.CollectionID(collection)
	}
	s.accept(w, r, req)
}

// handleIngestEpisode accepts an already-segmented unit: one conversation turn, one
// tool result. It still becomes a source event, so provenance and replay work exactly
// as they do for anything else; only segmentation is skipped.
func (s *Server) handleIngestEpisode(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleWriter)
	if !ok {
		return
	}

	var item ingestEventRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &item); err != nil {
		s.writeError(w, r, err)
		return
	}

	req, err := s.buildIngestRequest(scope, principalFrom(r.Context()), item, r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	req.DirectEpisode = true
	if req.Operation == "" {
		req.Operation = domain.SourceOpAppend
	}
	s.accept(w, r, req)
}

// accept submits one request and renders its receipt.
func (s *Server) accept(w http.ResponseWriter, r *http.Request, req ingest.Request) {
	receipt, err := s.gateway.Accept(r.Context(), req)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// A duplicate is a successful replay, not a new resource: 200 rather than 201 tells
	// the caller its retry was recognized instead of duplicated.
	status := http.StatusAccepted
	if receipt.Duplicate {
		status = http.StatusOK
	}
	s.writeJSON(w, r, status, toReceiptResponse(receipt))
}

// buildIngestRequest converts the wire shape into a gateway request.
func (s *Server) buildIngestRequest(scope domain.Scope, principal domain.Principal, item ingestEventRequest, headerKey string) (ingest.Request, error) {
	const op = "http.buildIngestRequest"

	if item.Content != "" && len(item.ContentJSON) > 0 {
		return ingest.Request{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"set either content or content_json, not both")
	}

	payload := []byte(item.Content)
	mediaType := item.MediaType
	if len(item.ContentJSON) > 0 {
		payload = item.ContentJSON
		if mediaType == "" {
			mediaType = normalize.MediaTypeJSON
		}
	}
	if len(payload) == 0 {
		return ingest.Request{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"content or content_json is required")
	}

	req := ingest.Request{
		Scope:            scope,
		Principal:        principal.Ref(),
		SourceID:         domain.SourceID(item.SourceID),
		SourceName:       item.SourceName,
		ExternalID:       item.ExternalID,
		EventType:        item.EventType,
		MediaType:        mediaType,
		Payload:          payload,
		IdempotencyKey:   firstNonEmpty(item.IdempotencyKey, headerKey),
		EventTime:        item.EventTime,
		SourceTime:       item.SourceTime,
		SourceCommitTime: item.SourceCommitTime,
		SourceSequence:   item.SourceSequence,
		SourceVersion:    item.SourceVersion,
		Metadata:         item.Metadata,
	}
	if item.CollectionID != "" {
		req.Scope.CollectionID = domain.CollectionID(item.CollectionID)
	}
	if item.Operation != "" {
		operation, err := domain.ParseSourceOperation(item.Operation)
		if err != nil {
			return ingest.Request{}, err
		}
		req.Operation = operation
	}
	if item.Classification != "" {
		classification, err := domain.ParseClassification(item.Classification)
		if err != nil {
			return ingest.Request{}, err
		}
		req.Classification = classification
	}
	return req, nil
}

// eventStatusResponse reports processing progress, never whether content is true.
type eventStatusResponse struct {
	SourceEventID string             `json:"source_event_id"`
	WorkspaceID   string             `json:"workspace_id"`
	GraphSpaceID  string             `json:"graph_space_id"`
	Status        string             `json:"status"`
	Operation     string             `json:"operation"`
	ContentHash   string             `json:"content_hash"`
	RecordedAt    string             `json:"recorded_at"`
	Episodes      int                `json:"episodes"`
	Chunks        int                `json:"chunks"`
	Pipeline      *pipelineResponse  `json:"pipeline,omitempty"`
	Work          []workItemResponse `json:"work,omitempty"`
}

type pipelineResponse struct {
	Version int             `json:"version"`
	Status  string          `json:"status"`
	Stages  []stageResponse `json:"stages,omitempty"`
}

type stageResponse struct {
	Name       string          `json:"name"`
	Version    int             `json:"version"`
	Status     string          `json:"status"`
	Attempts   int             `json:"attempts"`
	Output     json.RawMessage `json:"output,omitempty"`
	ErrorClass string          `json:"error_class,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
}

type workItemResponse struct {
	ID         string `json:"id"`
	EventType  string `json:"event_type"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	VisibleAt  string `json:"visible_at"`
	ErrorClass string `json:"error_class,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// handleEventStatus reports where an event is in the pipeline.
//
// The caller supplies only the event id; the workspace is resolved from the principal's
// grants, so an id belonging to another tenant is reported as absent.
func (s *Server) handleEventStatus(w http.ResponseWriter, r *http.Request) {
	const op = "http.handleEventStatus"

	principal := principalFrom(r.Context())
	rawID := r.PathValue("event_id")
	if !domain.ValidUUID(domain.SourceEventID(rawID)) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, op, "malformed event id"))
		return
	}
	eventID := domain.SourceEventID(rawID)

	allowed := make([]domain.WorkspaceID, 0, len(principal.Grants))
	for _, grant := range principal.Grants {
		allowed = append(allowed, grant.WorkspaceID)
	}

	ws, err := s.ledger.ResolveSourceEventWorkspace(r.Context(), eventID, allowed)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.identity.AuthorizeWorkspace(r.Context(), principal, ws, domain.RoleReader); err != nil {
		s.writeError(w, r, domain.Errorf(domain.CodeNotFound, op, "source event not found"))
		return
	}

	status, err := s.ledger.SourceEventStatus(r.Context(), ws, eventID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toEventStatusResponse(status))
}

func toEventStatusResponse(status ledger.EventStatus) eventStatusResponse {
	out := eventStatusResponse{
		SourceEventID: string(status.Event.ID),
		WorkspaceID:   string(status.Event.WorkspaceID),
		GraphSpaceID:  string(status.Event.GraphSpaceID),
		Status:        string(status.Event.Status),
		Operation:     string(status.Event.Operation),
		ContentHash:   status.Event.ContentHash,
		RecordedAt:    status.Event.RecordedAt.UTC().Format(timeFormat),
		Episodes:      status.Episodes,
		Chunks:        status.Chunks,
	}

	if status.Run != nil {
		pipeline := &pipelineResponse{
			Version: status.Run.PipelineVersion,
			Status:  string(status.Run.Status),
		}
		for _, stage := range status.Stages {
			pipeline.Stages = append(pipeline.Stages, stageResponse{
				Name:       stage.StageName,
				Version:    stage.StageVersion,
				Status:     string(stage.Status),
				Attempts:   stage.Attempts,
				Output:     stage.OutputRef,
				ErrorClass: string(stage.ErrorClass),
				LastError:  stage.LastError,
			})
		}
		out.Pipeline = pipeline
	}

	for _, item := range status.Work {
		out.Work = append(out.Work, workItemResponse{
			ID:         string(item.ID),
			EventType:  item.EventType,
			Status:     string(item.Status),
			Attempts:   item.Attempts,
			VisibleAt:  item.VisibleAt.UTC().Format(timeFormat),
			ErrorClass: string(item.ErrorClass),
			LastError:  item.LastError,
		})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
