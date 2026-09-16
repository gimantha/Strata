package ledger

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

func TestAnchoredTermsSplitsRequiredPhrasesFromTheRest(t *testing.T) {
	anchors, rest := anchoredTerms(domain.LexicalQuery{
		Text: "why was CASE-42 approved, and who signed?", Anchors: []string{"CASE-42", "case-42", " "},
	})
	if len(anchors) != 1 || anchors[0] != "CASE-42" {
		t.Fatalf("anchors = %v", anchors)
	}
	if rest != "why was approved and who signed" {
		t.Fatalf("rest = %q", rest)
	}
	if got, _ := anchoredTerms(domain.LexicalQuery{Text: "CASE-42", Anchors: []string{"CASE-42"}, Exact: true}); got != nil {
		t.Fatal("exact searches must not be anchored: the identifier is already the whole search")
	}
}

func TestAnchoredLexicalSearchReferencesEveryParameterOnce(t *testing.T) {
	scope := domain.Scope{WorkspaceID: domain.WorkspaceID(domain.NewUUIDString())}
	sql, args, err := buildLexicalSearch("test", domain.LexicalQuery{
		Scope: scope, Text: "why was CASE-42 approved after review",
		Anchors: []string{"CASE-42", "REV-7"}, MatchAny: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A parameter the statement never references has no inferable type and PostgreSQL
	// rejects the statement, so the anchored form must reference $1..$N exactly.
	seen := map[int]bool{}
	for _, m := range regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(sql, -1) {
		n, _ := strconv.Atoi(m[1])
		seen[n] = true
	}
	for i := 1; i <= len(args); i++ {
		if !seen[i] {
			t.Fatalf("parameter $%d is not referenced in:\n%s\nargs=%v", i, sql, args)
		}
	}
	if args[len(args)-1] != 10 {
		t.Fatalf("the limit must be the last parameter, got %v", args[len(args)-1])
	}
	if !strings.Contains(sql, " && ") || !strings.Contains(sql, `"CASE-42"`) && !strings.Contains(strings.Join(anyStrings(args), " "), `"CASE-42"`) {
		t.Fatalf("anchors were not composed as a tsquery conjunction:\n%s\nargs=%v", sql, args)
	}
}

func anyStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
