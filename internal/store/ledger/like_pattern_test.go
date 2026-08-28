package ledger

import "testing"

// TestLikePatternIsLiteral covers the characters exact search exists to find.
//
// Exact mode is for identifiers and codes that stemming mangles — ERR_7731X, AF-2291-B — and
// two of LIKE's three special characters are common in exactly those. Interpolating the
// caller's text into a pattern made "_" match any character and "%" match everything, so the
// mode meant to be literal was the one that was not.
func TestLikePatternIsLiteral(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		want  string
		notes string
	}{
		{
			name: "an underscore is a character, not a wildcard",
			text: "ERR_7731X",
			want: `%ERR\_7731X%`,
			// Without this, ERRX7731X matched a search for ERR_7731X.
			notes: "underscores are ordinary in error codes",
		},
		{
			name:  "a percent is a character, not everything",
			text:  "50%",
			want:  `%50\%%`,
			notes: "a bare % matched every record in the workspace",
		},
		{
			name:  "a backslash is escaped before the wildcards it could escape",
			text:  `a\_b`,
			want:  `%a\\\_b%`,
			notes: "otherwise the caller's backslash would escape our escape",
		},
		{
			name:  "ordinary text is untouched apart from the wrapping",
			text:  "industrial fasteners",
			want:  "%industrial fasteners%",
			notes: "escaping must not disturb the common case",
		},
		{
			name:  "empty text still wraps",
			text:  "",
			want:  "%%",
			notes: "the caller is rejected earlier, but the helper must not surprise",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := likePattern(tc.text); got != tc.want {
				t.Errorf("likePattern(%q) = %q, want %q (%s)", tc.text, got, tc.want, tc.notes)
			}
		})
	}
}
