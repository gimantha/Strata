package domain

import (
	"math"
	"strings"
	"time"
)

// Lifecycle is the fourth clock: when knowledge is relevant to an agent, which is not the
// same question as whether it is true (AGENTS.md sections 7.1, 21.3).
//
// "The user is staying at the Hilton tonight" is true forever and useful for a day. World
// validity says the first thing; this says the second. Conflating them is how a system
// either forgets facts it should keep or keeps surfacing ones nobody wants.
type Lifecycle struct {
	ActiveFrom    *time.Time
	ActiveUntil   *time.Time
	DecayStartsAt *time.Time
	ExpiresAt     *time.Time
}

// LifecycleOf reads the context clock off a claim's temporal coordinates.
func LifecycleOf(t TemporalCoordinates) Lifecycle {
	return Lifecycle{
		ActiveFrom:    t.ActiveFrom,
		ActiveUntil:   t.ActiveUntil,
		DecayStartsAt: t.DecayStartsAt,
		ExpiresAt:     t.ExpiresAt,
	}
}

// Active reports whether knowledge is in scope for an agent at an instant.
//
// Half-open, like every other interval here: something active until noon is not active at
// noon. Expiry is the harder bound — an expired memory is out of scope whatever its active
// window said, because expiry is the deployment saying "stop using this", not a schedule.
func (l Lifecycle) Active(at time.Time) bool {
	if l.ActiveFrom != nil && at.Before(*l.ActiveFrom) {
		return false
	}
	if l.ActiveUntil != nil && !at.Before(*l.ActiveUntil) {
		return false
	}
	if l.ExpiresAt != nil && !at.Before(*l.ExpiresAt) {
		return false
	}
	return true
}

// Expired reports whether a memory has passed its expiry.
//
// Separate from Active because the two mean different things to a reader: expired knowledge
// is still true, still cited, and still answerable as of an earlier instant. It has simply
// stopped being current context.
func (l Lifecycle) Expired(at time.Time) bool {
	return l.ExpiresAt != nil && !at.Before(*l.ExpiresAt)
}

// DecayHalfLife is how long it takes a decaying memory to lose half its ranking weight.
//
// Thirty days is a working default, not a discovery: it is long enough that a fact from last
// week ranks like a fact from today, and short enough that a year-old note about a temporary
// arrangement stops competing with current knowledge.
const DecayHalfLife = 30 * 24 * time.Hour

// MinDecayWeight is the floor a decayed memory settles at.
//
// Never zero. Decay affects ranking, not truth (AGENTS.md section 21.2), and a weight of zero
// would make an old fact unfindable — which is deletion wearing a ranking function's clothes.
// A floor means an old memory loses to a fresh one and still beats nothing.
const MinDecayWeight = 0.2

// DecayWeight scores how much ranking weight knowledge still carries at an instant.
//
// One before decay begins, then halving every half-life, floored. Applied as a multiplier on
// a retrieval score: it reorders results and never removes them.
func (l Lifecycle) DecayWeight(at time.Time, halfLife time.Duration) float64 {
	if l.DecayStartsAt == nil {
		return 1
	}
	if halfLife <= 0 {
		halfLife = DecayHalfLife
	}
	elapsed := at.Sub(*l.DecayStartsAt)
	if elapsed <= 0 {
		return 1
	}

	weight := math.Pow(0.5, float64(elapsed)/float64(halfLife))
	if weight < MinDecayWeight {
		return MinDecayWeight
	}
	return weight
}

// MemoryPriority weights kinds of memory against each other for retention and ranking
// (AGENTS.md section 9).
//
// Semantic knowledge outlives the episodes it came from; working memory is scaffolding. A
// consolidation pass that treated them alike would either keep every scratch note forever or
// throw away the facts they support.
func MemoryPriority(kind MemoryKind) float64 {
	switch kind {
	case MemorySemantic:
		return 1
	case MemoryProcedural:
		return 0.95
	case MemoryPreference:
		return 0.9
	case MemoryEpisodic:
		return 0.85
	case MemoryDerived:
		return 0.9
	case MemoryWorking:
		// Scaffolding for a task in progress. Useful now, noise next week.
		return 0.5
	default:
		return 0.8
	}
}

// ForgetKind names one of the four distinct ways knowledge stops being used
// (AGENTS.md section 21.4).
//
// They are enumerated rather than collapsed into a delete flag because they differ in what
// survives, who may perform them, and what an auditor can still see afterwards. A system with
// one "delete" cannot answer "was this withdrawn because it was wrong, or removed because we
// were told to erase it" — and those have opposite implications for everything derived from
// it.
type ForgetKind string

const (
	// ForgetDeactivate takes knowledge out of an agent's working context while leaving it
	// true, cited, and answerable as of any earlier instant. Reversible.
	ForgetDeactivate ForgetKind = "deactivate"
	// ForgetRetract withdraws a claim because it was wrong. A knowledge-time event: the
	// claim stays queryable as of before the retraction.
	ForgetRetract ForgetKind = "retract"
	// ForgetRetention removes records a retention policy no longer permits keeping. The
	// claim is gone; the fact that something was removed is not.
	ForgetRetention ForgetKind = "retention"
	// ForgetErasure is privacy hard-delete: the payload is destroyed everywhere,
	// projections included, and only an audit proof remains (AGENTS.md section 23).
	ForgetErasure ForgetKind = "erasure"
)

var forgetKinds = []ForgetKind{
	ForgetDeactivate, ForgetRetract, ForgetRetention, ForgetErasure,
}

func ParseForgetKind(s string) (ForgetKind, error) {
	return parseEnum("forget kind", s, forgetKinds)
}

// Destructive reports whether a kind of forgetting destroys records.
//
// The two that do are deliberately not implemented alongside the two that do not: they need
// erasure jobs, projection sweeps, and their own authorization, and shipping them as a flag
// on the same endpoint is how a reversible operation and an irreversible one end up one typo
// apart.
func (k ForgetKind) Destructive() bool {
	return k == ForgetRetention || k == ForgetErasure
}

// ConsolidationRule decides when repeated observation becomes a stable fact
// (AGENTS.md section 21.1).
type ConsolidationRule struct {
	// MinObservations is how many separate episodes must state a claim before it is
	// consolidated. Two is not a pattern; the default of three is the smallest count that
	// distinguishes repetition from coincidence.
	MinObservations int
	// MinDistinctSources requires corroboration across sources rather than one chatty
	// upstream repeating itself. One means a single source may consolidate alone.
	MinDistinctSources int
	// MinConfidence ignores weak candidates entirely.
	MinConfidence float64
	// Kinds restricts which memory kinds are consolidated from. Empty means episodic
	// only, because consolidating already-semantic knowledge produces a copy rather than
	// a conclusion.
	Kinds []MemoryKind
}

// DefaultConsolidationRule returns the tuned defaults.
func DefaultConsolidationRule() ConsolidationRule {
	return ConsolidationRule{
		MinObservations:    3,
		MinDistinctSources: 1,
		MinConfidence:      0.5,
		Kinds:              []MemoryKind{MemoryEpisodic},
	}
}

// Normalize fills anything left unset.
func (r ConsolidationRule) Normalize() ConsolidationRule {
	defaults := DefaultConsolidationRule()
	if r.MinObservations <= 0 {
		r.MinObservations = defaults.MinObservations
	}
	if r.MinDistinctSources <= 0 {
		r.MinDistinctSources = defaults.MinDistinctSources
	}
	if r.MinConfidence <= 0 {
		r.MinConfidence = defaults.MinConfidence
	}
	if len(r.Kinds) == 0 {
		r.Kinds = defaults.Kinds
	}
	return r
}

// ObservationGroup is a set of claims saying the same thing, gathered for consolidation.
type ObservationGroup struct {
	SubjectID EntityID
	Predicate string
	ScopeKey  string
	ObjectKey string

	// Members are the supporting claims, which become the derivation's inputs. A derived
	// fact that cannot name what it was derived from is an assertion nobody can check.
	Members []Assertion
	// Sources counts distinct origins, which is what separates corroboration from one
	// source repeating itself.
	Sources map[SourceID]struct{}
}

// Key identifies the slot this group fills.
func (g ObservationGroup) Key() string {
	return string(g.SubjectID) + "|" + g.Predicate + "|" + g.ScopeKey + "|" + g.ObjectKey
}

// Qualifies reports whether a group has earned consolidation under a rule.
func (g ObservationGroup) Qualifies(rule ConsolidationRule) bool {
	rule = rule.Normalize()
	if len(g.Members) < rule.MinObservations {
		return false
	}
	if len(g.Sources) < rule.MinDistinctSources {
		return false
	}
	return true
}

// Confidence is how much a consolidated fact should be believed.
//
// It rises with repetition and with independent sources, and is capped below certainty: a
// conclusion drawn from observations is never more certain than an observation, however many
// times it was seen. The cap is what stops repetition from manufacturing confidence.
func (g ObservationGroup) Confidence() float64 {
	if len(g.Members) == 0 {
		return 0
	}

	mean := 0.0
	for _, member := range g.Members {
		mean += member.Confidence
	}
	mean /= float64(len(g.Members))

	// Each observation past the first adds a diminishing amount, and each independent
	// source adds more than a repetition from the same one.
	repetition := 1 - math.Pow(0.6, float64(len(g.Members)))
	corroboration := 1 - math.Pow(0.5, float64(len(g.Sources)))

	confidence := mean * (0.6 + 0.25*repetition + 0.15*corroboration)
	if confidence > 0.95 {
		confidence = 0.95
	}
	return confidence
}

// EarliestValidFrom is the start of the consolidated claim's world validity: the earliest any
// supporting observation said it held.
func (g ObservationGroup) EarliestValidFrom() *time.Time {
	var earliest *time.Time
	for _, member := range g.Members {
		from := member.Temporal.ValidFrom
		if from == nil {
			continue
		}
		if earliest == nil || from.Before(*earliest) {
			earliest = from
		}
	}
	return earliest
}

// Summary describes a consolidation in one line, for the derivation's parameters and for
// anyone reading the audit trail later.
func (g ObservationGroup) Summary() string {
	var b strings.Builder
	b.WriteString("observed ")
	b.WriteString(itoa(len(g.Members)))
	b.WriteString(" times")
	if len(g.Sources) > 1 {
		b.WriteString(" across ")
		b.WriteString(itoa(len(g.Sources)))
		b.WriteString(" sources")
	}
	return b.String()
}
