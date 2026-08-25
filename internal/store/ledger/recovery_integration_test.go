package ledger_test

import (
	"testing"

	"github.com/gimantha/strata/internal/store/ledger"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

// TestIntegrationEveryTableIsClassifiedForBackup keeps AGENTS.md section 40's guidance from
// going stale.
//
// Section 40 divides the database in two: canonical tables must be restored from backup,
// derived indexes may be backed up for speed but must be rebuildable. That division is only
// useful if it covers every table, and prose in a runbook cannot enforce that. A table added
// next year without a decision about which half it belongs to would leave the runbook quietly
// wrong, and the discovery would happen during a restore.
//
// So the classification is data, and this test walks the live schema against it. Adding a
// table now requires saying which half it is in, which is the decision that was going to be
// needed eventually anyway.
func TestIntegrationEveryTableIsClassifiedForBackup(t *testing.T) {
	store := pgtest.Store(t)

	classification, err := store.ClassifyTables(t.Context())
	if err != nil {
		t.Fatalf("classify tables: %v", err)
	}
	if !classification.Complete() {
		t.Fatalf("the backup classification does not match the schema: %s",
			classification.Problem())
	}

	// A sanity floor, so a classification that had drifted to empty would fail loudly
	// rather than pass by covering nothing.
	if len(classification.Canonical) < 20 {
		t.Fatalf("only %d canonical tables; the classification looks truncated",
			len(classification.Canonical))
	}
	if len(classification.Derived) == 0 {
		t.Fatal("no derived tables; section 40's rebuildable half has gone missing")
	}
}

// TestCanonicalAndDerivedDoNotOverlap checks the two halves are actually halves.
//
// A table in both lists would be backed up as authoritative and also treated as disposable,
// and the two policies disagree about what a restore should contain.
func TestCanonicalAndDerivedDoNotOverlap(t *testing.T) {
	derived := map[string]bool{}
	for _, name := range ledger.DerivedTables() {
		derived[name] = true
	}
	for _, name := range ledger.CanonicalTables() {
		if derived[name] {
			t.Fatalf("%s is classified as both canonical and derived", name)
		}
	}
}
