# ADR 0016: A policy decision is a filter, not a verdict

Status: accepted
Date: 2026-08-24

## Context

AGENTS.md section 22.4 states the requirement plainly: apply policy constraints in each
retriever's query where technically possible, and never retrieve unauthorized data into
application memory and merely hide it afterwards.

Most access-control layers return a boolean. That shape works for "may this principal call
this endpoint" and fails completely for "which of these ten thousand claims may they see" —
because the only way to answer with a boolean is to fetch everything and drop rows on the way
out. Which is the pattern section 22.4 exists to forbid, and which fails in three ways at
once: restricted rows sit in this process's memory, they compete for slots in a ranked result
set, and a filtered query silently returns fewer results than asked for.

## Decision

**Evaluation returns a decision *and* the narrowing it implies.** `Decision.Filters` carries
a classification ceiling, excluded and permitted sources, predicates, entity types, and
memory kinds. Every field is designed to become a SQL clause, and the read paths push them
into their `WHERE` clauses rather than filtering results.

**A deny that names resources narrows; a deny that names none refuses.** "Readers may not see
HR sources" does not fail a reader's query — it removes those sources from it. A rule that
made every query fail would be routed around within a week, and a system people route around
protects nothing. A deny naming no resources is a blanket refusal, which is the only case
where refusing is the right answer.

**Deny wins, then explicit allow, then the role baseline.** Deliberately boring. Priority
numbers, most-specific-match, and first-match ordering are where policy bugs live, because two
people reading the same rules reach different conclusions and both are defensible. Here, if
any blanket deny matches, the answer is no.

**A grant is still the gate.** Policy narrows access inside a workspace a principal already
holds; it never grants access to one they do not, however permissive a rule looks. Multi-tenant
isolation is not a policy question and must not become one.

**A clearance can only lower a ceiling.** Two limits on what someone may see combine to the
tighter one. If a grant's clearance could raise the policy ceiling, granting somebody access
to a workspace would quietly hand them everything in it and the policy would be advice.

**"No policy configured" is a named state, not an absence.** `DefaultPolicySet` is role-based
access with an internal clearance ceiling, and it appears in audit records as version 0. An
absence is something each reader interprets; a named default is something they can look up.

**Every decision is audited, refusals included, and audit rows never quote content.** A log
that reproduced the material it was guarding would be a second copy of the protected data, in
a table people grant broad read access to precisely because it is "just metadata".

## Consequences

Two filter columns were added to the projections — `source_id` and `predicate` — for the same
reason validity and classification were already there: a rule restricting a principal to
certain sources has to narrow the query, and joining back to the ledger per candidate would
make a filtered query slower than an unfiltered one, which is exactly how "filter afterwards"
becomes the tempting shortcut.

Graph traversal applies policy inside the walk. Traversal leaks by reaching: an entity found
only through a restricted edge is disclosed by its presence even when the edge's own claim is
filtered from the answer, and filtering afterwards would also corrupt depth, since a hidden
edge would still have counted as a hop. The clause is written as static SQL with nullable
array parameters rather than assembled per query, because a recursive CTE with two lateral
branches must apply exactly the same condition in both and a concatenated string is one edit
away from applying it in only one.

`PolicyFilters.Allows` re-checks a record in hand. The filters are enforced in SQL; this is
the second gate for paths that hold a canonical record already — hydrating a citation,
streaming an export row. Belt and braces, on the paths where a missed filter hands over data
rather than merely showing too much.

Retrieval traces became implementable in this phase rather than in phase 8, where they were
deferred: section 6.12 marks query text "subject to policy/redaction", and there was nothing
to redact against. A trace now stores the query hash always and the text only when the
deployment permits, so "which queries run often" stays answerable where the words themselves
may not be kept.

The rule language is deliberately small. There are no priorities, no rule references, no
expressions, and no arithmetic. Everything it can express is checkable by reading it, and
everything it cannot express is a reason to write code rather than a reason to grow the
language.

## Alternatives considered

**Boolean decisions with post-filtering.** The common implementation and the one section 22.4
names as forbidden. It puts restricted rows in memory, lets them displace permitted ones in a
ranked list, and turns a filtered query into a short one.

**Row-level security in PostgreSQL.** Genuinely attractive: enforcement in the database is
enforcement nobody can forget. Rejected for now because the policy attributes include the
caller's stated purpose and the request's residency, which are not properties of a database
session, and because expressing this rule set as RLS policies would move the logic somewhere
it cannot be unit-tested. Worth revisiting for the classification ceiling alone.

**A general expression language.** Every policy engine grows one eventually. It also grows a
class of bugs where a rule does something nobody predicted, at a layer where surprises are
expensive. When the fixed attributes stop being enough, the answer is a new attribute, not a
new syntax.
