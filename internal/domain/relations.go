package domain

import (
	"net/url"
	"sort"
	"strings"
)

// EncodeContextQualifiers turns named n-ary relation roles into a stable scope key. The
// assertion remains the fact; qualifiers retain the project, case, region, phase, or other
// context in which that fact holds without inventing a different predicate for each one.
func EncodeContextQualifiers(qualifiers map[string]string) (string, error) {
	if len(qualifiers) == 0 {
		return "", nil
	}
	roles := make([]string, 0, len(qualifiers))
	values := make(map[string]string, len(qualifiers))
	for rawRole, rawValue := range qualifiers {
		role := strings.ToLower(NormalizePredicateName(rawRole))
		value := strings.TrimSpace(rawValue)
		if role == "" || value == "" {
			return "", Errorf(CodeInvalidArgument, "domain.EncodeContextQualifiers",
				"qualifier roles and values must not be empty")
		}
		if _, duplicate := values[role]; duplicate {
			return "", Errorf(CodeInvalidArgument, "domain.EncodeContextQualifiers",
				"qualifier role %q is duplicated after normalization", rawRole)
		}
		roles = append(roles, role)
		values[role] = value
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		parts = append(parts, url.QueryEscape(role)+"="+url.QueryEscape(values[role]))
	}
	return strings.Join(parts, ";"), nil
}

// ContextQualifiers decodes structured scope keys. A legacy unstructured key remains
// readable as the generic context role, so adopting qualifiers does not rewrite history.
func ContextQualifiers(scopeKey string) map[string]string {
	scopeKey = strings.TrimSpace(scopeKey)
	if scopeKey == "" {
		return nil
	}
	if !strings.Contains(scopeKey, "=") {
		return map[string]string{"context": scopeKey}
	}
	out := map[string]string{}
	for _, part := range strings.Split(scopeKey, ";") {
		pair := strings.SplitN(part, "=", 2)
		if len(pair) != 2 {
			return map[string]string{"context": scopeKey}
		}
		role, roleErr := url.QueryUnescape(pair[0])
		value, valueErr := url.QueryUnescape(pair[1])
		if roleErr != nil || valueErr != nil || role == "" || value == "" {
			return map[string]string{"context": scopeKey}
		}
		out[role] = value
	}
	return out
}

// DisplayContextQualifiers renders a scope for search and human-readable context.
func DisplayContextQualifiers(scopeKey string) string {
	qualifiers := ContextQualifiers(scopeKey)
	roles := make([]string, 0, len(qualifiers))
	for role := range qualifiers {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		parts = append(parts, strings.ReplaceAll(role, "_", " ")+" "+qualifiers[role])
	}
	return strings.Join(parts, ", ")
}
