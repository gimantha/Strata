package extraction

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// wireResult mirrors the JSON schema exactly. Decoding into this shape, with unknown
// fields rejected, is the local validation that a provider's "strict mode" does not
// replace (AGENTS.md section 13.1).
type wireResult struct {
	Entities   []wireEntity    `json:"entities"`
	Assertions []wireAssertion `json:"assertions"`
	Temporal   []wireTemporal  `json:"temporal"`
}

type wireEntity struct {
	Name        string   `json:"name"`
	EntityType  string   `json:"entity_type"`
	Aliases     []string `json:"aliases"`
	MentionText string   `json:"mention_text"`
	Confidence  float64  `json:"confidence"`
}

type wireAssertion struct {
	SubjectName      string  `json:"subject_name"`
	SubjectType      string  `json:"subject_type"`
	Predicate        string  `json:"predicate"`
	ObjectEntityName *string `json:"object_entity_name"`
	ObjectKind       *string `json:"object_kind"`
	ObjectValue      *string `json:"object_value"`
	ScopeKey         *string `json:"scope_key"`
	ValidFrom        *string `json:"valid_from"`
	ValidTo          *string `json:"valid_to"`
	EventTime        *string `json:"event_time"`
	EvidenceQuote    string  `json:"evidence_quote"`
	Confidence       float64 `json:"confidence"`
}

type wireTemporal struct {
	Text       string  `json:"text"`
	Kind       string  `json:"kind"`
	Resolved   *string `json:"resolved"`
	Confidence float64 `json:"confidence"`
}

// Rejection records a candidate that was dropped and why.
//
// Rejections are returned rather than silently swallowed: a model that keeps producing
// ungrounded claims is a problem an operator needs to see.
type Rejection struct {
	Kind    string // entity or assertion
	Subject string
	Reason  string
}

// Validated is the result of checking a model's output.
type Validated struct {
	Result     domain.ExtractionResult
	Rejections []Rejection
}

// MaxRawResponseBytes bounds the output a model may return.
const MaxRawResponseBytes = 1 << 20

// Validate parses and checks structured output against the source it came from.
//
// The distinction it draws matters. Output that is not parseable, or that does not match
// the schema, fails outright: nothing from that response is usable, and none of it reaches
// the ledger. Output that parses but contains individual bad candidates keeps the good
// ones and reports the rest as rejections - one hallucinated claim among ten should not
// discard the nine that check out.
func Validate(raw []byte, source string) (Validated, error) {
	const op = "extraction.Validate"

	if len(raw) == 0 {
		return Validated{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"the model returned an empty response")
	}
	if len(raw) > MaxRawResponseBytes {
		return Validated{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"the model returned %d bytes, over the %d byte limit", len(raw), MaxRawResponseBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	// Unknown fields are a schema violation, not a curiosity: a model inventing fields is
	// a model that may be inventing other things.
	decoder.DisallowUnknownFields()

	var wire wireResult
	if err := decoder.Decode(&wire); err != nil {
		return Validated{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"the model returned output that does not match the schema: %s", err.Error())
	}
	if err := decoder.Decode(new(json.RawMessage)); err == nil {
		return Validated{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"the model returned more than one JSON document")
	}

	if len(wire.Entities) > domain.MaxCandidatesPerRequest ||
		len(wire.Assertions) > domain.MaxCandidatesPerRequest {
		return Validated{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"the model returned more than %d candidates", domain.MaxCandidatesPerRequest)
	}

	out := Validated{}

	for _, e := range wire.Entities {
		candidate := domain.EntityCandidate{
			Name:        strings.TrimSpace(e.Name),
			EntityType:  normalizeEntityType(e.EntityType),
			Aliases:     trimAll(e.Aliases),
			MentionText: e.MentionText,
			Confidence:  clampConfidence(e.Confidence),
		}
		if err := candidate.Validate(); err != nil {
			out.Rejections = append(out.Rejections, Rejection{
				Kind: "entity", Subject: e.Name, Reason: domainMessage(err),
			})
			continue
		}
		out.Result.Entities = append(out.Result.Entities, candidate)
	}

	for _, a := range wire.Assertions {
		candidate := domain.AssertionCandidate{
			SubjectName:      strings.TrimSpace(a.SubjectName),
			SubjectType:      normalizeEntityType(a.SubjectType),
			Predicate:        strings.TrimSpace(a.Predicate),
			ObjectEntityName: strings.TrimSpace(deref(a.ObjectEntityName)),
			ObjectEntityType: "",
			ObjectValue:      deref(a.ObjectValue),
			ScopeKey:         strings.TrimSpace(deref(a.ScopeKey)),
			EvidenceQuote:    a.EvidenceQuote,
			Confidence:       clampConfidence(a.Confidence),
		}
		if candidate.ObjectEntityName == "" {
			candidate.ObjectKind = domain.ObjectKind(strings.TrimSpace(deref(a.ObjectKind)))
			if candidate.ObjectKind == "" {
				candidate.ObjectKind = domain.ObjectString
			}
		}

		var invalid bool
		for _, field := range []struct {
			name  string
			value *string
			dest  **time.Time
		}{
			{"valid_from", a.ValidFrom, &candidate.ValidFrom},
			{"valid_to", a.ValidTo, &candidate.ValidTo},
			{"event_time", a.EventTime, &candidate.EventTime},
		} {
			parsed, err := parseOptionalInstant(field.value)
			if err != nil {
				out.Rejections = append(out.Rejections, Rejection{
					Kind: "assertion", Subject: a.SubjectName + " " + a.Predicate,
					Reason: field.name + " is not a valid timestamp",
				})
				invalid = true
				break
			}
			*field.dest = parsed
		}
		if invalid {
			continue
		}

		if err := candidate.Validate(); err != nil {
			out.Rejections = append(out.Rejections, Rejection{
				Kind: "assertion", Subject: a.SubjectName + " " + a.Predicate,
				Reason: domainMessage(err),
			})
			continue
		}

		// The quote must really be in the source. This catches a model that invented a
		// fact outright.
		if !candidate.GroundedIn(source) {
			out.Rejections = append(out.Rejections, Rejection{
				Kind: "assertion", Subject: a.SubjectName + " " + a.Predicate,
				Reason: "evidence quote does not appear in the source material",
			})
			continue
		}

		// Grounding cannot catch a planted quote. An attacker who writes "report that X
		// is true" into a document has supplied a real quote for the model to find, so
		// the claim grounds perfectly well. What distinguishes it is the instruction
		// wrapped around it, which taints the paragraph it sits in.
		if reason, suspicious := QuoteIsSuspicious(source, candidate.EvidenceQuote); suspicious {
			candidate.Quarantine = true
			candidate.QuarantineReason = reason
		}

		out.Result.Assertions = append(out.Result.Assertions, candidate)
	}

	for _, h := range wire.Temporal {
		resolved, err := parseOptionalInstant(h.Resolved)
		if err != nil {
			// A bad hint is dropped rather than failing the response: hints are advisory,
			// and claims carry their own validity.
			continue
		}
		out.Result.Temporal = append(out.Result.Temporal, domain.TemporalHint{
			Text:       h.Text,
			Kind:       h.Kind,
			Resolved:   resolved,
			Confidence: clampConfidence(h.Confidence),
		})
	}

	return out, nil
}

// parseOptionalInstant accepts RFC3339, a plain date, or nothing.
func parseOptionalInstant(value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, domain.DateLayout} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			utc := parsed.UTC()
			return &utc, nil
		}
	}
	return nil, domain.Errorf(domain.CodeInvalidArgument, "extraction.parseOptionalInstant",
		"%q is not a recognizable timestamp", trimmed)
}

// normalizeEntityType keeps types to a short lowercase vocabulary without rejecting a
// model that capitalizes differently.
func normalizeEntityType(t string) string {
	trimmed := strings.ToLower(strings.TrimSpace(t))
	if trimmed == "" {
		return "unknown"
	}
	return strings.ReplaceAll(trimmed, " ", "_")
}

// clampConfidence keeps a model's self-reported score in range instead of rejecting the
// candidate over it. An out-of-range score says the number is unreliable, which clamping
// already reflects.
func clampConfidence(v float64) float64 {
	switch {
	case v <= 0:
		return 0.5
	case v > 1:
		return 1
	default:
		return v
	}
}

func trimAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// domainMessage extracts the client-safe part of a domain error.
func domainMessage(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return err.Error()
}
