package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// A work item this consumer cannot understand must fail as invalid input rather than as a
// transient error, so it dead-letters immediately instead of retrying forever against a
// consumer that will never understand it (AGENTS.md sections 28.4, 34).
func TestHandleWorkRejectsUnprocessableItemsTerminally(t *testing.T) {
	app := &App{}
	ctx := context.Background()

	payload, err := json.Marshal(domain.SourceEventAcceptedPayload{PipelineVersion: 1})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}

	cases := []struct {
		name  string
		event domain.OutboxEvent
	}{
		{
			name: "unknown event type",
			event: domain.OutboxEvent{
				EventType:     "knowledge.teleported",
				SchemaVersion: domain.OutboxSchemaVersion,
				Payload:       payload,
			},
		},
		{
			name: "future schema version",
			event: domain.OutboxEvent{
				EventType:     domain.EventTypeSourceEventAccepted,
				SchemaVersion: domain.OutboxSchemaVersion + 1,
				Payload:       payload,
			},
		},
		{
			name: "malformed payload",
			event: domain.OutboxEvent{
				EventType:     domain.EventTypeSourceEventAccepted,
				SchemaVersion: domain.OutboxSchemaVersion,
				Payload:       json.RawMessage(`{"pipeline_version": "one"}`),
			},
		},
		{
			name: "no source event",
			event: domain.OutboxEvent{
				EventType:     domain.EventTypeSourceEventAccepted,
				SchemaVersion: domain.OutboxSchemaVersion,
				Payload:       payload,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := app.HandleWork(ctx, tc.event)
			if err == nil {
				t.Fatal("expected the item to be rejected")
			}
			if domain.ClassifyError(err).Retryable() {
				t.Fatalf("expected a terminal failure, got the retryable class %s",
					domain.ClassifyError(err))
			}
		})
	}
}

func TestSubscriptionMirrorsConfiguration(t *testing.T) {
	app := &App{}
	app.Config.WorkerConcurrency = 7
	app.Config.WorkerBatchSize = 11
	app.Config.PipelineVersion = 3

	spec := app.Subscription()
	if spec.Concurrency != 7 || spec.BatchSize != 11 {
		t.Fatalf("subscription must reflect configuration: %+v", spec)
	}
	if len(spec.Topics) != 1 || spec.Topics[0] != domain.TopicIngestPipeline {
		t.Fatalf("the worker must subscribe to the ingest topic, got %v", spec.Topics)
	}
}
