package lifecycle

import "errors"

// ErrTransitionConflict is returned by RecordEvent when an optimistic
// CAS write loses to a concurrent transition — the bead's current
// status no longer matches the pre-event status the caller observed.
var ErrTransitionConflict = errors.New("lifecycle: transition conflict")
