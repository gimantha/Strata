package domain

import "time"

// TemporalCoordinates carries all four clock layers this system reasons over
// (AGENTS.md section 7.2). Assertions in phase 2 embed it; source events record
// the same clocks as explicit columns.
//
// The layers are deliberately separate. Collapsing them into one created_at is the
// single most common modeling mistake this architecture exists to avoid.
type TemporalCoordinates struct {
	// World time: when something was true in the modeled world.
	EventTime     *time.Time
	ValidFrom     *time.Time
	ValidTo       *time.Time
	EffectiveFrom *time.Time
	EffectiveTo   *time.Time

	// Knowledge time: when this system learned and recorded it.
	ObservedAt   time.Time
	RecordedAt   time.Time
	SupersededAt *time.Time

	// Source time: ordering and timing as reported upstream.
	SourceTime       *time.Time
	SourceCommitTime *time.Time
	SourceSequence   string
	SourceVersion    string

	// Context lifecycle: when knowledge is relevant to an agent, which is not the
	// same question as whether it was ever true.
	ActiveFrom    *time.Time
	ActiveUntil   *time.Time
	DecayStartsAt *time.Time
	ExpiresAt     *time.Time
}

// Normalize converts every timestamp to UTC. Internal time is always UTC; original
// timezone metadata belongs in a metadata field when it is semantically relevant
// (AGENTS.md section 34).
func (t TemporalCoordinates) Normalize() TemporalCoordinates {
	t.EventTime = utcPtr(t.EventTime)
	t.ValidFrom = utcPtr(t.ValidFrom)
	t.ValidTo = utcPtr(t.ValidTo)
	t.EffectiveFrom = utcPtr(t.EffectiveFrom)
	t.EffectiveTo = utcPtr(t.EffectiveTo)
	t.ObservedAt = t.ObservedAt.UTC()
	t.RecordedAt = t.RecordedAt.UTC()
	t.SupersededAt = utcPtr(t.SupersededAt)
	t.SourceTime = utcPtr(t.SourceTime)
	t.SourceCommitTime = utcPtr(t.SourceCommitTime)
	t.ActiveFrom = utcPtr(t.ActiveFrom)
	t.ActiveUntil = utcPtr(t.ActiveUntil)
	t.DecayStartsAt = utcPtr(t.DecayStartsAt)
	t.ExpiresAt = utcPtr(t.ExpiresAt)
	return t
}

// Validate rejects inverted intervals and missing knowledge time. Knowledge time is
// mandatory: every record must say when the system learned of it.
func (t TemporalCoordinates) Validate() error {
	const op = "domain.TemporalCoordinates.Validate"
	if t.ObservedAt.IsZero() {
		return Errorf(CodeInvalidArgument, op, "observed_at is required")
	}
	if t.RecordedAt.IsZero() {
		return Errorf(CodeInvalidArgument, op, "recorded_at is required")
	}
	for _, iv := range []struct {
		name     string
		from, to *time.Time
	}{
		{"valid", t.ValidFrom, t.ValidTo},
		{"effective", t.EffectiveFrom, t.EffectiveTo},
		{"active", t.ActiveFrom, t.ActiveUntil},
	} {
		if iv.from != nil && iv.to != nil && iv.to.Before(*iv.from) {
			return Errorf(CodeInvalidArgument, op, "%s interval ends before it starts", iv.name)
		}
	}
	return nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
