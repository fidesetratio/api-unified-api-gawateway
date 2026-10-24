package domain

import "fmt"

// Status is the normalized payment status stored in the database.
type Status string

const (
	// StatusCreating is internal: the row exists but the provider has not
	// confirmed a payment yet. The API shows it as PENDING.
	StatusCreating  Status = "creating"
	StatusPending   Status = "pending"
	StatusPaid      Status = "paid"
	StatusFailed    Status = "failed"
	StatusExpired   Status = "expired"
	StatusCancelled Status = "cancelled"
	StatusRefunded  Status = "refunded"
)

// AllStatuses lists every status in a stable order.
var AllStatuses = []Status{
	StatusCreating, StatusPending, StatusPaid, StatusFailed,
	StatusExpired, StatusCancelled, StatusRefunded,
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, k := range AllStatuses {
		if s == k {
			return true
		}
	}
	return false
}

// APIName is the uppercase name shown to callers. The internal CREATING state
// is shown as PENDING.
func (s Status) APIName() string {
	switch s {
	case StatusCreating, StatusPending:
		return "PENDING"
	case StatusPaid:
		return "PAID"
	case StatusFailed:
		return "FAILED"
	case StatusExpired:
		return "EXPIRED"
	case StatusCancelled:
		return "CANCELLED"
	case StatusRefunded:
		return "REFUNDED"
	}
	return "UNKNOWN"
}

// Outcome classifies a transition for the caller of CheckTransition.
type Outcome int

const (
	// OutcomeNoop means from == to: nothing to do (duplicate delivery).
	OutcomeNoop Outcome = iota
	// OutcomeAllowed is a normal transition.
	OutcomeAllowed
	// OutcomeAllowedNeedsReview is accepted but must open a review item
	// (verified payment arriving after expiry or cancellation).
	OutcomeAllowedNeedsReview
	// OutcomeRejected must not be applied.
	OutcomeRejected
)

// transitions is the single source of truth for status changes.
var transitions = map[Status]map[Status]Outcome{
	StatusCreating: {
		StatusPending: OutcomeAllowed, StatusFailed: OutcomeAllowed,
		StatusExpired: OutcomeAllowed, StatusCancelled: OutcomeAllowed,
		// A verified payment can be reported before we recorded the provider reply.
		StatusPaid: OutcomeAllowed,
	},
	StatusPending: {
		StatusPaid: OutcomeAllowed, StatusFailed: OutcomeAllowed,
		StatusExpired: OutcomeAllowed, StatusCancelled: OutcomeAllowed,
	},
	StatusPaid: {
		StatusRefunded: OutcomeAllowed,
	},
	StatusExpired: {
		StatusPaid: OutcomeAllowedNeedsReview,
	},
	StatusCancelled: {
		StatusPaid: OutcomeAllowedNeedsReview,
	},
	// failed and refunded are terminal.
}

// CheckTransition decides what a change from one status to another means.
// Unknown statuses are rejected.
func CheckTransition(from, to Status) Outcome {
	if !from.Valid() || !to.Valid() {
		return OutcomeRejected
	}
	if from == to {
		return OutcomeNoop
	}
	if o, ok := transitions[from][to]; ok {
		return o
	}
	return OutcomeRejected
}

// TransitionError describes a rejected transition.
type TransitionError struct{ From, To Status }

func (e *TransitionError) Error() string {
	return fmt.Sprintf("transition %s -> %s is not allowed", e.From, e.To)
}
