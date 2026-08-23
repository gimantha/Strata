# Ontology and schema modes

A graph space is either **open** — extraction invents entity types and predicate names, and
the registry normalizes them — or **guided**, where every claim is validated against a bound
schema version (AGENTS.md section 8).

Both exist because they answer different questions. A new corpus needs discovery; a
production pipeline needs a contract. The mode is set per graph space, so the same source can
be processed both ways and compared.

## Defining a version

```
POST /v1/graph-spaces/{graph_space_id}/ontology/versions
```

```json
{
  "name": "supply chain v1",
  "activate": true,
  "register_predicates": true,
  "entity_types": [
    {"name": "organization", "aliases": ["company", "org"]},
    {"name": "facility"}
  ],
  "predicates": [
    {
      "name": "SUPPLIES_TO",
      "subject_types": ["organization"],
      "object_types": ["facility"],
      "object_kinds": ["entity"]
    },
    {
      "name": "TIER",
      "subject_types": ["organization"],
      "object_kinds": ["symbol"],
      "allowed_values": ["PREMIUM", "STANDARD"],
      "functional": true
    }
  ]
}
```

Versions are **immutable and sequenced**. Assertions record the version that validated them,
so editing one in place would silently change what committed claims were checked against.
`activate` supersedes the other active versions; without it the version is a draft, which can
be validated against but not bound.

`register_predicates` writes the constraints into the predicate registry, so guided
declaration and open-mode discovery share one description of what a predicate means instead
of maintaining two that can disagree.

Unconstrained fields stay open: a predicate naming no subject types accepts any defined type.
An ontology that had to enumerate everything would be unusable for the loose predicates every
corpus has.

## Binding a graph space

```
PUT /v1/graph-spaces/{graph_space_id}/ontology
{"mode": "guided", "ontology_version_id": "01a0..."}
```

`GET` the same path to see the current binding.

**Binding is not retroactive.** Claims already committed keep the version they were validated
under, or none. Re-validating history against a new schema would rewrite what the system
believed at the time. Use `validate` first.

## What validation checks

| Violation | Meaning |
|---|---|
| `unknown_entity_type` | The subject or object type is not in the schema |
| `unknown_predicate` | The predicate is not in the schema |
| `subject_type_not_allowed` | The predicate does not take that subject type |
| `object_type_not_allowed` | The predicate does not take that object type |
| `object_kind_not_allowed` | Wrong typed-object column — a string where a symbol was declared |
| `value_not_allowed` | Outside a closed vocabulary |

Every violation is reported, not just the first: a candidate with a misspelled type and a bad
value has two problems, and fixing them one round trip at a time is nobody's idea of a good
afternoon.

Entity type aliases are honored. Sources disagree about whether a company is an
`organization`, an `org`, or a `company`, and refusing a claim over that is pedantry rather
than validation.

## What happens to an invalid claim

Never a silent commit. Which of the two outcomes depends on who can act next:

| Source | Outcome |
|---|---|
| A caller stating a claim through the API | **Rejected** with `ontology_violation` and the specific problem |
| A model proposing a claim during extraction | **Quarantined**: committed with status `quarantined`, carrying the reasons |

A caller can fix and resend, so the problem goes back to them. A model has no such loop, and
discarding its output would lose evidence of what the source said — so the claim is held,
visible, for a human to accept or drop.

**A quarantined claim is not belief.** It is not projected, so it cannot be retrieved, so it
cannot reach a context block. It is not reconciled either: a claim the schema refused must not
supersede one the schema accepted.

## Validating before you commit to a schema

```
POST /v1/graph-spaces/{graph_space_id}/ontology/versions/{id}/validate
```

```
$ cgctl ontology validate --graph-space $GS --version 01a0...

ontology version 2 (supply chain v2)
  1284 claim(s) checked, 1190 conforming, 94 violating

VIOLATION                 COUNT
unknown_predicate         71
value_not_allowed         23

  Acme Corporation MOOD optimistic
      unknown_predicate: predicate "MOOD" is not in the ontology
  ... and 93 more

Nothing was changed. This is a report, not a migration.
```

This is the migration tool. A schema change is cheap to declare and expensive to discover
wrong, so the useful operation is not "apply" but "tell me what this would refuse" — run on
real data, before binding, with the answer in hand. It never writes.

## Schema-guided extraction

In guided mode the allowed vocabulary is added to the extraction prompt and the system
message tells the model to omit facts that do not fit rather than invent names for them.

Prompting narrows what has to be rejected; it does not replace rejecting it. The same schema
is enforced again at commit, because a model told to use certain predicates will mostly
comply and will occasionally not. Anything that slips through is quarantined.

## Type-constrained retrieval

Queries can narrow to one kind of subject (AGENTS.md section 19.2):

```json
{"query": "assembles fasteners", "entity_types": ["facility"]}
```

The projections copy the subject's entity type, so the filter runs before ranking rather than
after.

## From the CLI

```bash
cgctl ontology define --graph-space $GS --file schema.json --activate
cgctl ontology ls --graph-space $GS
cgctl ontology validate --graph-space $GS --version $VERSION
cgctl ontology bind --graph-space $GS --mode guided --version $VERSION
cgctl ontology bind --graph-space $GS --mode open
```

The schema file is the JSON body above without the `activate` and `register_predicates`
flags — it is a contract, meant to be reviewed and committed to a repository like any other.

The reasoning behind the two modes and the two dispositions is in
[ADR 0014](../adr/0014-two-ontology-modes-and-two-dispositions.md).
