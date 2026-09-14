package ledger_test

import (
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

type purgeSeed struct {
	event     domain.SourceEventID
	assertion domain.AssertionID
	blobKey   string
}

func seedPurgeKnowledge(t *testing.T, f *pgtest.Fixture, tenant pgtest.Tenant, recorded time.Time, status domain.AssertionStatus) purgeSeed {
	t.Helper()
	ctx := t.Context()
	token := domain.NewUUIDString()
	artifact, event := domain.NewArtifactID(), domain.NewSourceEventID()
	episode, chunk := domain.NewEpisodeID(), domain.NewChunkID()
	entity, predicate, assertion := domain.NewEntityID(), domain.NewPredicateID(), domain.NewAssertionID()
	modelRun := domain.ModelRunID(domain.NewUUIDString())
	blobKey := "purge/" + token

	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO artifacts (id,workspace_id,content_hash,size_bytes,blob_key,storage,created_at)
		  VALUES ($1,$2,$3,10,$4,'fixture',$5)`, []any{artifact, tenant.Workspace.ID, token, blobKey, recorded}},
		{`INSERT INTO source_events
		  (id,workspace_id,graph_space_id,source_id,operation,content_hash,idempotency_key,
		   observed_at,recorded_at,raw_artifact_id,status,classification,created_at)
		  VALUES ($1,$2,$3,$4,'append',$5,$5,$6,$6,$7,'processed','internal',$6)`,
			[]any{event, tenant.Workspace.ID, tenant.GraphSpace.ID, tenant.Source.ID, token, recorded, artifact}},
		{`INSERT INTO episodes
		  (id,workspace_id,graph_space_id,source_event_id,artifact_id,sequence,content,
		   observed_at,recorded_at,classification,created_at)
		  VALUES ($1,$2,$3,$4,$5,1,'old source',$6,$6,'internal',$6)`,
			[]any{episode, tenant.Workspace.ID, tenant.GraphSpace.ID, event, artifact, recorded}},
		{`INSERT INTO chunks
		  (id,workspace_id,graph_space_id,source_event_id,episode_id,artifact_id,sequence,
		   content,classification,created_at)
		  VALUES ($1,$2,$3,$4,$5,$6,1,'old source','internal',$7)`,
			[]any{chunk, tenant.Workspace.ID, tenant.GraphSpace.ID, event, episode, artifact, recorded}},
		{`INSERT INTO predicates
		  (id,workspace_id,name,temporal_policy,conflict_policy,default_memory_kind,sensitivity,status)
		  VALUES ($1,$2,$3,'interval','coexist','semantic','internal','active')`,
			[]any{predicate, tenant.Workspace.ID, "PURGE_" + token}},
		{`INSERT INTO entities
		  (id,workspace_id,graph_space_id,canonical_name,entity_type,created_at)
		  VALUES ($1,$2,$3,$4,'fixture',$5)`,
			[]any{entity, tenant.Workspace.ID, tenant.GraphSpace.ID, "Entity " + token, recorded}},
		{`INSERT INTO assertions
		  (id,workspace_id,graph_space_id,subject_id,predicate_id,predicate_name,predicate_version,
		   object_kind,object_text,object_key,memory_kind,observed_at,recorded_at,status,
		   provenance_mode,source_event_id,fingerprint,classification,retracted_at)
		  VALUES ($1,$2,$3,$4,$5,$6,1,'string','old','old','semantic',$7::timestamptz,$7::timestamptz,$8::text,
		   'extracted',$9,$10,'internal',CASE WHEN $8::text='retracted' THEN $7::timestamptz ELSE NULL END)`,
			[]any{assertion, tenant.Workspace.ID, tenant.GraphSpace.ID, entity, predicate,
				"PURGE_" + token, recorded, status, event, token}},
		{`INSERT INTO model_runs
		  (id,workspace_id,graph_space_id,provider,model,request_hash,status,source_event_id,created_at)
		  VALUES ($1,$2,$3,'fixture','fixture',$4,'succeeded',$5,$6)`,
			[]any{modelRun, tenant.Workspace.ID, tenant.GraphSpace.ID, token, event, recorded}},
		{`INSERT INTO evidence
		  (id,workspace_id,assertion_id,episode_id,chunk_id,artifact_id,source_event_id,
		   extracted_text,model_run_id,created_at)
		  VALUES ($1,$2,$3,$4,$5,$6,$7,'old source',$8,$9)`,
			[]any{domain.NewEvidenceID(), tenant.Workspace.ID, assertion, episode, chunk, artifact, event, modelRun, recorded}},
		{`INSERT INTO resolution_decisions
		  (id,workspace_id,graph_space_id,mention_text,method,chosen_entity_id,confidence,
		   resolver_version,source_event_id,created_at)
		  VALUES ($1,$2,$3,'old mention','new_entity',$4,1,1,$5,$6)`,
			[]any{domain.NewUUIDString(), tenant.Workspace.ID, tenant.GraphSpace.ID, entity, event, recorded}},
		{`INSERT INTO lexical_records
		  (id,workspace_id,graph_space_id,surface,record_id,content,classification,source_event_id)
		  VALUES ($1,$2,$3,'chunk',$4,'old source','internal',$5)`,
			[]any{domain.NewUUIDString(), tenant.Workspace.ID, tenant.GraphSpace.ID, chunk, event}},
	}
	for i, statement := range statements {
		if _, err := f.Store.Pool().Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed purge knowledge statement %d: %v", i, err)
		}
	}
	return purgeSeed{event: event, assertion: assertion, blobKey: blobKey}
}

func TestIntegrationKnowledgePurgeProtectsLiveHeldAndReferencedKnowledge(t *testing.T) {
	f := pgtest.NewFixture(t)
	other := f.NewTenant(t, "purge-other")
	now := time.Now().UTC()
	old, cutoff := now.Add(-120*24*time.Hour), now.Add(-60*24*time.Hour)
	first := seedPurgeKnowledge(t, f, f.Primary, old, domain.AssertionRetracted)
	second := seedPurgeKnowledge(t, f, f.Primary, old.Add(time.Hour), domain.AssertionRetracted)
	live := seedPurgeKnowledge(t, f, f.Primary, old, domain.AssertionActive)
	_ = seedPurgeKnowledge(t, f, other, old, domain.AssertionRetracted)
	referenced := seedPurgeKnowledge(t, f, f.Primary, old, domain.AssertionSuperseded)
	newer := seedPurgeKnowledge(t, f, f.Primary, now.Add(-24*time.Hour), domain.AssertionActive)
	if _, err := f.Store.Pool().Exec(t.Context(), `UPDATE assertions SET supersedes_id=$1
		WHERE workspace_id=$2 AND id=$3`, referenced.assertion, f.Primary.Workspace.ID, newer.assertion); err != nil {
		t.Fatalf("link retained supersession: %v", err)
	}

	hold, err := f.Store.CreateKnowledgeHold(t.Context(), domain.KnowledgeHold{
		Scope: f.Primary.Scope(), SourceEventID: first.event, Reason: "litigation",
		CreatedBy: f.Primary.Principal.Ref(),
	})
	if err != nil {
		t.Fatalf("create hold: %v", err)
	}
	request := domain.KnowledgePurgeRequest{
		Scope: f.Primary.Scope(), Before: cutoff, RequestedAt: now,
		Principal: f.Primary.Principal.Ref(), Reason: "expired retention window", DryRun: true,
	}
	plan, err := f.Store.PlanKnowledgePurge(t.Context(), request)
	if err != nil {
		t.Fatalf("plan held purge: %v", err)
	}
	if plan.Eligible.Events != 1 {
		t.Fatalf("held/live/referenced/foreign events must be protected, eligible=%+v", plan.Eligible)
	}
	if err := f.Store.ReleaseKnowledgeHold(t.Context(), f.Primary.Scope(), hold.ID, f.Primary.Principal.Ref()); err != nil {
		t.Fatalf("release hold: %v", err)
	}
	request.DryRun = false
	plan, err = f.Store.PlanKnowledgePurge(t.Context(), request)
	if err != nil {
		t.Fatalf("plan executable purge: %v", err)
	}
	if plan.Eligible.Events != 2 || plan.Eligible.Assertions != 2 || plan.Eligible.Blobs != 2 {
		t.Fatalf("unexpected executable selection: %+v", plan.Eligible)
	}
	run, keys, err := f.Store.ExecuteKnowledgePurge(t.Context(), f.Primary.Workspace.ID, plan.ID)
	if err != nil {
		t.Fatalf("execute purge: %v", err)
	}
	if run.Status != domain.PurgeCanonicalDeleted || run.Deleted.Events != 2 || len(keys) != 2 {
		t.Fatalf("unexpected purge result: run=%+v keys=%v", run, keys)
	}
	for _, seed := range []purgeSeed{first, second} {
		var exists bool
		if err := f.Store.Pool().QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM source_events WHERE id=$1)`, seed.event).Scan(&exists); err != nil || exists {
			t.Fatalf("purged event %s remains (exists=%v err=%v)", seed.event, exists, err)
		}
	}
	for label, seed := range map[string]purgeSeed{"live": live, "referenced": referenced, "foreign": {event: otherEvent(t, f, other)}} {
		var exists bool
		if err := f.Store.Pool().QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM source_events WHERE id=$1)`, seed.event).Scan(&exists); err != nil || !exists {
			t.Fatalf("%s event was not protected (exists=%v err=%v)", label, exists, err)
		}
	}
	if count, err := f.Store.CountAuditEvents(t.Context(), f.Primary.Workspace.ID, "knowledge.purge.execute"); err != nil || count != 1 {
		t.Fatalf("purge execution audit count=%d err=%v", count, err)
	}
	if err := f.Store.FinishKnowledgePurge(t.Context(), f.Primary.Workspace.ID, plan.ID,
		domain.PurgeFailed, "projection unavailable", 0, now); err != nil {
		t.Fatalf("record partial cleanup: %v", err)
	}
	retried, retryKeys, err := f.Store.ExecuteKnowledgePurge(t.Context(), f.Primary.Workspace.ID, plan.ID)
	if err != nil {
		t.Fatalf("resume canonical-deleted purge: %v", err)
	}
	if retried.Deleted.Events != 2 || len(retryKeys) != len(keys) {
		t.Fatalf("retry lost durable cleanup state: run=%+v keys=%v", retried, retryKeys)
	}
	if err := f.Store.FinishKnowledgePurge(t.Context(), f.Primary.Workspace.ID, plan.ID,
		domain.PurgeCompleted, "", int64(len(keys)), now.Add(time.Minute)); err != nil {
		t.Fatalf("finish retry: %v", err)
	}
}

func otherEvent(t *testing.T, f *pgtest.Fixture, tenant pgtest.Tenant) domain.SourceEventID {
	t.Helper()
	var id domain.SourceEventID
	if err := f.Store.Pool().QueryRow(t.Context(), `SELECT id FROM source_events
		WHERE workspace_id=$1 ORDER BY recorded_at LIMIT 1`, tenant.Workspace.ID).Scan(&id); err != nil {
		t.Fatalf("load foreign event: %v", err)
	}
	return id
}
