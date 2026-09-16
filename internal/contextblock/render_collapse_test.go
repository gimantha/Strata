package contextblock

import "testing"

func TestCollapsePreservesLineBreaksButNotBlankRuns(t *testing.T) {
	in := "Event ID: EV-0233\nProject: Project-001\n\n\n\nDecision for   Project-001: approved.  \n"
	got := collapse(in)
	want := "Event ID: EV-0233\nProject: Project-001\n\nDecision for Project-001: approved."
	if got != want {
		t.Fatalf("collapse:\n got %q\nwant %q", got, want)
	}
	// A quote taken from the rendered body must still be a substring of the source, which
	// is the property phase 8 promised and single-line folding broke.
	if !contains(in, "Decision for   Project-001: approved.") || contains(got, "2026-01-05T00:00:00Z Decision") {
		t.Fatal("rendering must not join header and body onto one line")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
