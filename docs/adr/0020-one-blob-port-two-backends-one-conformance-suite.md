# ADR 0020 — One blob port, two backends, one conformance suite

Status: accepted
Date: 2026-08-25
Phase: 15 (advanced storage adapters)

## Context

Every ingested payload is archived before its event is committed, because an assertion must
always be traceable back to the bytes it came from (sections 2.2, 6.4). Until now the only
backend was the local filesystem.

That is correct and it has one operational limitation that section 40 makes concrete: a
filesystem offers no versioning and no replication of its own. Protecting the artifact store
therefore falls entirely on the deployment, and the consequence of getting it wrong is
specific and bad — a `source_events` row whose artifact is missing is a claim that can no
longer be walked back to its source. The provenance chain ends in a 404, which is worse than
the claim being absent, because the claim still looks answerable.

Phase 15 asks for an S3-compatible blob backend. It also says: *do not change canonical
semantics to suit one backend.*

## Decision

**One port, two backends, and one behavioural suite that both must pass.**

`blob.Store` already existed with six methods and was written small deliberately. The S3
adapter implements it without adding to it, and `blob.RunConformance` — exported from the
non-test file — is run by both backends' tests.

That suite is the substance of this decision. Two implementations compiling against one
interface prove nothing about behaviour; the compiler checks signatures and says nothing
about what the methods do. The suite checks the parts that genuinely differ between a
filesystem and an object store:

- **Absence is `ErrNotFound`** from both `Get` and `Stat`. Object stores report absence
  three different ways — `GetObject` returns a typed `NoSuchKey`, `HeadObject` has no body to
  carry one and returns a bare 404, and some compatible implementations use a `NotFound` API
  code — so an adapter can easily translate one path and not the other.
- **`Put` is idempotent.** Keys are content addresses, so an ingestion replay writes identical
  bytes to the same key. A backend treating that as a conflict turns a normal retry into a
  failed ingest.
- **`Delete` is idempotent.** A caller cleaning up should not have to distinguish "gone now"
  from "gone already".
- **Empty content round trips.** A zero-byte document is legal, and a backend that stored
  nothing would make it indistinguishable from a missing artifact — a provenance chain that
  ends in a lie rather than a gap.

**Tested against MinIO, not AWS.** The adapter claims S3 *compatibility*, and a compatibility
claim tested only against S3 is untested. MinIO runs in CI and in `scripts/dev-minio.sh`,
alongside the real PostgreSQL and real NATS the other suites use, for the same reason: the
behaviour under test is how a real implementation responds, and a fake reproduces whatever
its author already believed.

**The SDK, not a hand-rolled client.** The MCP server was implemented directly rather than
through an SDK, on the grounds that a protocol shim is a hundred lines and a dependency would
put a third party between this system and its own contract. That reasoning does not carry
here. SigV4 request signing is intricate, and getting it wrong is a security defect rather
than a bug. `aws-sdk-go-v2` handles it, works against any S3-compatible endpoint through an
endpoint override, and stays behind the port — nothing outside `internal/store/blob/s3`
imports it.

**`Name()` returns `"s3"` for every provider.** Artifact rows record which *kind* of store
holds the bytes so a later migration can find them. Recording the vendor would make the value
change when a deployment moves between S3-compatible providers, without the bytes moving at
all.

## Consequences

A deployment chooses with `CG_BLOB_BACKEND`. Nothing downstream knows which is running:
`app.Blobs` is typed as the port rather than the implementation, which is what makes that
true rather than merely intended.

Existing artifacts stay where they are. `Artifact.Storage` has recorded the backend on every
row since phase 1, so filesystem-era and object-store-era artifacts coexist and a migration
tool can tell them apart. Switching the backend does not move anything; that is a separate
operation nobody has needed yet.

**The bucket is not created.** Bucket lifecycle, versioning, and replication are the
deployment's to configure — they are the reason to use an object store at all. An adapter
that quietly created buckets would make a typo in configuration look like a working
deployment, with the bytes going somewhere nobody backs up.

`Healthy` uses `HeadBucket` rather than a test write. Readiness is asked often, and a probe
that wrote would fill a bucket with health checks and would fail a deployment that is
deliberately read-only during a recovery.

Credentials may be omitted entirely, falling back to the ambient chain, so a deployment on
AWS holds no credentials in its configuration. The redacted config log prints the bucket and
endpoint and never the secret.

## Alternatives considered

**Extending the port with object-store concepts** — presigned URLs, multipart upload, storage
classes. Rejected for now: none of them is needed by ingestion, and a port that grows to fit
one backend stops being a port. Multipart matters only above 5GB, and a single archived
payload that large is a different problem.

**A hand-rolled S3 client**, for strict provider independence. Rejected: see above. The
independence that matters is that nothing outside the adapter package imports the SDK, and
that is enforced by `scripts/check-domain-deps.sh` for the domain and by the port everywhere
else.

**Keeping the filesystem as the only backend and telling operators to replicate the disk.**
Rejected because it makes the durability of the provenance chain depend on infrastructure
this project cannot see, test, or describe in a runbook.
