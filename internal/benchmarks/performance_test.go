package benchmarks_test

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/benchmarks"
	"github.com/gimantha/strata/internal/domain"
)

// These are Go benchmarks rather than tests, so `go test ./...` does not run them and CI
// stays fast. Run them deliberately:
//
//	scripts/benchmark.sh
//	go test ./internal/benchmarks/ -bench . -benchtime 1x -run '^$' -v
//
// Every result prints the conditions section 39 requires alongside it.

// BenchmarkIngestAcceptance measures section 39's first target: durable ingest acceptance,
// tens to hundreds of events per second per node.
//
// The operation timed is Accept — archive the payload, commit the event and its work item,
// return. Deliberately not including processing: the API's promise is that a caller waits
// for durability and never for extraction, so timing the pipeline here would measure a
// latency no caller experiences.
func BenchmarkIngestAcceptance(b *testing.B) {
	corpus := benchmarks.DefaultCorpus()
	d := newDeployment(b, corpus)
	docs := corpus.Generate()

	var (
		latencies []time.Duration
		accepted  int
	)

	start := time.Now()
	for b.Loop() {
		doc := docs[accepted%len(docs)]
		// Unique keys, because an idempotent replay is a different and much cheaper
		// operation than a first acceptance.
		doc.ExternalID = fmt.Sprintf("%s-%d", doc.ExternalID, accepted/len(docs))

		at := time.Now()
		d.accept(b, doc)
		latencies = append(latencies, time.Since(at))
		accepted++
	}
	elapsed := time.Since(start)

	slices.Sort(latencies)
	perSecond := float64(accepted) / elapsed.Seconds()
	b.ReportMetric(perSecond, "events/sec")

	if !reportable(len(latencies)) {
		return
	}
	report(b, corpus, "durable ingest acceptance",
		fmt.Sprintf("%.0f events/sec sustained over %d events (%s)",
			perSecond, accepted, elapsed.Round(time.Millisecond)),
		fmt.Sprintf("p50 %s  p95 %s  p99 %s",
			percentile(latencies, 50).Round(time.Microsecond),
			percentile(latencies, 95).Round(time.Microsecond),
			percentile(latencies, 99).Round(time.Microsecond)),
		"target: tens to hundreds of events/sec per node (AGENTS.md section 39)")
}

// BenchmarkIngestAcceptanceConcurrent measures the same target with several callers, which
// is what "per node" means.
//
// A single-caller figure is a latency measurement wearing a throughput label: it reports how
// fast one request completes, not how much the node can absorb. Most of an acceptance is
// waiting on the database, so concurrency is where a node's real capacity shows.
func BenchmarkIngestAcceptanceConcurrent(b *testing.B) {
	corpus := benchmarks.DefaultCorpus()
	d := newDeployment(b, corpus)
	docs := corpus.Generate()

	var (
		mu        sync.Mutex
		latencies []time.Duration
		counter   atomic.Int64
	)

	start := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		var local []time.Duration
		for pb.Next() {
			n := counter.Add(1)
			doc := docs[int(n)%len(docs)]
			// Unique per iteration, so no caller is measuring an idempotent replay.
			doc.ExternalID = fmt.Sprintf("%s-c%d", doc.ExternalID, n)

			at := time.Now()
			d.accept(b, doc)
			local = append(local, time.Since(at))
		}
		mu.Lock()
		latencies = append(latencies, local...)
		mu.Unlock()
	})
	elapsed := time.Since(start)

	slices.Sort(latencies)
	accepted := counter.Load()
	perSecond := float64(accepted) / elapsed.Seconds()
	b.ReportMetric(perSecond, "events/sec")

	if !reportable(len(latencies)) {
		return
	}
	report(b, corpus, "durable ingest acceptance, concurrent callers",
		fmt.Sprintf("%.0f events/sec sustained over %d events across %d goroutines (%s)",
			perSecond, accepted, runtime.GOMAXPROCS(0), elapsed.Round(time.Millisecond)),
		fmt.Sprintf("p50 %s  p95 %s  p99 %s",
			percentile(latencies, 50).Round(time.Microsecond),
			percentile(latencies, 95).Round(time.Microsecond),
			percentile(latencies, 99).Round(time.Microsecond)),
		"target: tens to hundreds of events/sec per node (AGENTS.md section 39)")
}

// BenchmarkRetrievalLatency measures section 39's second target: query retrieval p95 below
// one second without external reranking.
//
// The mix is reported per shape rather than pooled. A pooled p95 over entity lookups and
// semantic questions describes neither, and hides the case where one shape is slow because
// it is missing an index.
func BenchmarkRetrievalLatency(b *testing.B) {
	corpus := benchmarks.DefaultCorpus()
	d := newDeployment(b, corpus)
	ingested, processed := d.load(b)
	b.Logf("corpus loaded: ingest %s, processing %s",
		ingested.Round(time.Millisecond), processed.Round(time.Millisecond))

	for _, mix := range benchmarks.StandardMix(corpus.Vocabulary()) {
		b.Run(mix.Name, func(b *testing.B) {
			var latencies []time.Duration
			issued := 0

			for b.Loop() {
				query := mix.Queries[issued%len(mix.Queries)]
				issued++

				at := time.Now()
				result, err := d.app.Retriever.Query(context.Background(), domain.QueryRequest{
					Scope:     d.scope,
					Query:     query,
					Principal: d.principal,
					Limit:     20,
				})
				if err != nil {
					b.Fatalf("query %q: %v", query, err)
				}
				latencies = append(latencies, time.Since(at))
				if len(result.Items) == 0 && mix.Name != "rare-term" {
					b.Fatalf("query %q returned nothing; the benchmark is measuring an "+
						"empty index rather than retrieval", query)
				}
			}

			slices.Sort(latencies)
			p95 := percentile(latencies, 95)
			b.ReportMetric(float64(p95.Microseconds())/1000, "p95_ms")

			if !reportable(len(latencies)) {
				return
			}
			report(b, corpus, "retrieval latency: "+mix.Name,
				fmt.Sprintf("queries: %v", mix.Queries),
				fmt.Sprintf("p50 %s  p95 %s  p99 %s over %d queries",
					percentile(latencies, 50).Round(time.Microsecond), p95.Round(time.Microsecond),
					percentile(latencies, 99).Round(time.Microsecond), len(latencies)),
				"target: p95 under 1s without external reranking (AGENTS.md section 39)")

			if limit := target("CG_BENCH_RETRIEVAL_P95", time.Second); p95 > limit {
				b.Errorf("p95 of %s exceeds the %s target", p95.Round(time.Millisecond), limit)
			}
		})
	}
}

// BenchmarkContextAssembly measures the end-to-end path an agent actually uses: retrieve,
// select under a token budget, render with citations.
//
// Worth measuring separately from retrieval because assembly hydrates canonical records for
// every citation, and that is a different query pattern from ranking.
func BenchmarkContextAssembly(b *testing.B) {
	corpus := benchmarks.DefaultCorpus()
	d := newDeployment(b, corpus)
	d.load(b)

	queries := benchmarks.StandardMix(corpus.Vocabulary())[1].Queries
	var latencies []time.Duration
	issued := 0

	for b.Loop() {
		at := time.Now()
		block, err := d.app.Assembler.Assemble(context.Background(), domain.ContextRequest{
			Scope:       d.scope,
			Query:       queries[issued%len(queries)],
			Principal:   d.principal,
			TokenBudget: 2000,
		})
		if err != nil {
			b.Fatalf("assemble: %v", err)
		}
		if block.Budget.Used > 2000 {
			b.Fatalf("assembly exceeded its budget: %d tokens", block.Budget.Used)
		}
		latencies = append(latencies, time.Since(at))
		issued++
	}

	slices.Sort(latencies)
	p95 := percentile(latencies, 95)
	b.ReportMetric(float64(p95.Microseconds())/1000, "p95_ms")

	if !reportable(len(latencies)) {
		return
	}
	report(b, corpus, "context assembly (2000-token budget)",
		fmt.Sprintf("queries: %v", queries),
		fmt.Sprintf("p50 %s  p95 %s over %d assemblies",
			percentile(latencies, 50).Round(time.Microsecond), p95.Round(time.Microsecond),
			len(latencies)))
}

// BenchmarkProjectionLag measures section 39's third target: incremental projection lag,
// normally seconds.
//
// Lag here is the interval from accepting a document to its content being retrievable —
// what a caller experiences as "I just added this and cannot find it". Measured against a
// warm corpus rather than an empty database, because lag on an empty index is not the
// number anyone needs.
func BenchmarkProjectionLag(b *testing.B) {
	corpus := benchmarks.DefaultCorpus()
	d := newDeployment(b, corpus)
	d.load(b)

	var lags []time.Duration
	issued := 0

	for b.Loop() {
		marker := fmt.Sprintf("Zephyrine%d", issued)
		doc := benchmarks.Document{
			ExternalID: fmt.Sprintf("lag-%d", issued),
			Content:    "Late arrival " + marker + " joined Kelvin Analytics this week.",
		}
		issued++

		at := time.Now()
		eventID := d.accept(b, doc)
		if _, err := d.app.Runner.Process(context.Background(),
			d.scope.WorkspaceID, eventID, false); err != nil {
			b.Fatalf("process: %v", err)
		}

		result, err := d.app.Retriever.Query(context.Background(), domain.QueryRequest{
			Scope: d.scope, Query: marker, Principal: d.principal, Limit: 5,
		})
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if len(result.Items) == 0 {
			b.Fatalf("a processed document was not retrievable; lag is unbounded, not slow")
		}
		lags = append(lags, time.Since(at))
	}

	slices.Sort(lags)
	p95 := percentile(lags, 95)
	b.ReportMetric(float64(p95.Milliseconds()), "p95_ms")

	if !reportable(len(lags)) {
		return
	}
	report(b, corpus, "incremental projection lag (accept to retrievable)",
		fmt.Sprintf("p50 %s  p95 %s over %d documents",
			percentile(lags, 50).Round(time.Millisecond), p95.Round(time.Millisecond), len(lags)),
		"target: normally seconds (AGENTS.md section 39)")

	if limit := target("CG_BENCH_PROJECTION_LAG_P95", 5*time.Second); p95 > limit {
		b.Errorf("p95 lag of %s exceeds the %s target", p95.Round(time.Millisecond), limit)
	}
}
