// Package extraction turns source material into candidate knowledge using a model.
//
// Three rules shape everything here (AGENTS.md sections 13, 24):
//
//  1. Source content is untrusted data. Prompts delimit it explicitly and instruct the
//     model never to act on instructions found inside it.
//  2. Output is schema-constrained and validated locally. A provider's promise that it
//     conformed is not a substitute for checking.
//  3. A model proposes; it never decides. Scope, classification, knowledge time, and
//     status come from the system, and every claim must quote the source verbatim.
package extraction

import "encoding/json"

// SchemaName identifies the structured-output schema to providers.
const SchemaName = "strata_extraction_result"

// PromptVersion is recorded on every model run, so a change in prompting can be correlated
// with a change in extraction quality.
const PromptVersion = 1

// PromptTemplate names the prompt in model-run records.
const PromptTemplate = "extract_candidates"

// ResultSchema is the JSON schema the model must conform to.
//
// Every property is required and additionalProperties is false throughout, which is what
// strict structured-output modes demand and what keeps the model's answer inside a shape
// we fully control. Optional values are expressed as nullable rather than omitted.
var ResultSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["entities", "assertions", "temporal"],
  "properties": {
    "entities": {
      "type": "array",
      "description": "Distinct real-world identities named in the source.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "entity_type", "aliases", "mention_text", "confidence"],
        "properties": {
          "name": {"type": "string", "description": "Canonical name exactly as written in the source."},
          "entity_type": {"type": "string", "description": "person, organization, place, product, or another short lowercase type."},
          "aliases": {"type": "array", "items": {"type": "string"}},
          "mention_text": {"type": "string", "description": "The exact span of the source naming this entity."},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1}
        }
      }
    },
    "assertions": {
      "type": "array",
      "description": "Factual claims the source states about those identities.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["subject_name", "subject_type", "predicate", "object_entity_name",
                     "object_kind", "object_value", "scope_key", "valid_from", "valid_to",
                     "event_time", "evidence_quote", "confidence"],
        "properties": {
          "subject_name": {"type": "string"},
          "subject_type": {"type": "string"},
          "predicate": {"type": "string", "description": "A relationship name such as WORKS_AT or FOUNDED_ON."},
          "object_entity_name": {
            "type": ["string", "null"],
            "description": "Set when the object is another identity; otherwise null."
          },
          "object_kind": {
            "type": ["string", "null"],
            "enum": ["string", "integer", "decimal", "boolean", "timestamp", "date",
                     "duration", "uri", "symbol", null],
            "description": "Type of a literal object; null when object_entity_name is set."
          },
          "object_value": {
            "type": ["string", "null"],
            "description": "Literal value as text; null when object_entity_name is set."
          },
          "scope_key": {
            "type": ["string", "null"],
            "description": "Context that narrows the claim, such as a role or project."
          },
          "valid_from": {"type": ["string", "null"], "description": "RFC3339 instant the fact became true."},
          "valid_to": {"type": ["string", "null"], "description": "RFC3339 instant the fact stopped being true."},
          "event_time": {"type": ["string", "null"], "description": "RFC3339 instant the described event occurred."},
          "evidence_quote": {
            "type": "string",
            "description": "A verbatim span copied from the source that states this fact. Required."
          },
          "confidence": {"type": "number", "minimum": 0, "maximum": 1}
        }
      }
    },
    "temporal": {
      "type": "array",
      "description": "Time expressions found in the source.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["text", "kind", "resolved", "confidence"],
        "properties": {
          "text": {"type": "string"},
          "kind": {"type": "string", "enum": ["valid_from", "valid_to", "event_time", "mentioned"]},
          "resolved": {"type": ["string", "null"], "description": "RFC3339 instant, or null if unresolvable."},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1}
        }
      }
    }
  }
}`)
