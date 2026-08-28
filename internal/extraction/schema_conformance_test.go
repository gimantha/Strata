package extraction_test

import (
	"testing"

	"github.com/gimantha/strata/internal/extraction"
	"github.com/gimantha/strata/internal/llm"
)

// The incumbent runs the suite too (ADR 0020). This schema documented the rule; running it
// here is what keeps the documentation and the schema from drifting apart.
func TestResultSchemaIsStrictConformant(t *testing.T) {
	llm.RunStrictSchemaConformance(t, "extraction.ResultSchema", extraction.ResultSchema)
}
