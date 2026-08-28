package retrieval

import (
	"encoding/json"
	"testing"

	"github.com/gimantha/strata/internal/llm"
)

func TestPlanSchemaIsStrictConformant(t *testing.T) {
	llm.RunStrictSchemaConformance(t, "retrieval.planSchema", json.RawMessage(planSchema))
}
