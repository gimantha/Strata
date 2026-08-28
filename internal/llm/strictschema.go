package llm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// unsupportedKeywords are rejected by strict structured-output modes rather than ignored.
//
// They are the tempting ones: a schema author reaches for maxItems to bound an array and
// maxLength to bound a string, and both turn a working schema into a 400.
var unsupportedKeywords = []string{
	"minItems", "maxItems", "minLength", "maxLength",
	"pattern", "format", "minProperties", "maxProperties",
	"uniqueItems", "oneOf", "allOf", "not", "if", "then", "else",
}

// RunStrictSchemaConformance holds a schema to what strict structured output demands:
// every property listed in "required", additionalProperties false on every object, and no
// keyword the strict validators reject.
//
// It exists because breaking these rules fails in the worst available direction. The
// provider rejects the request, the caller treats an unavailable model as a reason to
// degrade, and the degraded path is a working fallback — so a schema that can never be
// accepted presents as software that runs fine and quietly never uses the model at all.
// Nothing short of reading the logs or calling the real provider would show it.
//
// Called from a test in each package that owns a schema, on the principle of ADR 0020: the
// suite runs against the incumbent as well as the newcomer.
func RunStrictSchemaConformance(t *testing.T, name string, schema json.RawMessage) {
	t.Helper()

	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		t.Fatalf("%s: schema is not valid JSON: %v", name, err)
	}
	var problems []string
	walkSchema(root, "$", &problems)

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%s violates strict structured output:\n  %s\n"+
			"A provider rejects this schema, and the caller degrades rather than failing, "+
			"so the model is silently never used.", name, strings.Join(problems, "\n  "))
	}
}

func walkSchema(node any, path string, problems *[]string) {
	object, ok := node.(map[string]any)
	if !ok {
		if list, isList := node.([]any); isList {
			for i, item := range list {
				walkSchema(item, fmt.Sprintf("%s[%d]", path, i), problems)
			}
		}
		return
	}

	for _, keyword := range unsupportedKeywords {
		if _, present := object[keyword]; present {
			*problems = append(*problems,
				fmt.Sprintf("%s: %q is not supported and is rejected, not ignored", path, keyword))
		}
	}

	if isObjectSchema(object) {
		switch additional, present := object["additionalProperties"]; {
		case !present:
			*problems = append(*problems,
				fmt.Sprintf("%s: needs \"additionalProperties\": false", path))
		case additional != false:
			*problems = append(*problems,
				fmt.Sprintf("%s: \"additionalProperties\" must be false, not an open map", path))
		}

		properties, _ := object["properties"].(map[string]any)
		required := map[string]bool{}
		if list, isList := object["required"].([]any); isList {
			for _, name := range list {
				if text, isText := name.(string); isText {
					required[text] = true
				}
			}
		}
		for name := range properties {
			if !required[name] {
				*problems = append(*problems, fmt.Sprintf(
					"%s: %q is optional; strict mode requires every property, so express "+
						"an optional value as a nullable type instead", path, name))
			}
		}
	}

	for key, child := range object {
		switch key {
		case "enum", "required", "description", "type", "additionalProperties":
			continue
		}
		walkSchema(child, path+"."+key, problems)
	}
}

func isObjectSchema(object map[string]any) bool {
	if _, hasProperties := object["properties"]; hasProperties {
		return true
	}
	kind, _ := object["type"].(string)
	return kind == "object"
}
