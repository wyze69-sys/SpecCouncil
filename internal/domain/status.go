package domain

// RoleStatus is the persisted state of one role row.
//
// Retrying is NOT a persistent role state. Both provider attempts of a role
// happen inside a single in_flight execution.
type RoleStatus string

const (
	RolePending     RoleStatus = "pending"
	RoleInFlight    RoleStatus = "in_flight"
	RoleComplete    RoleStatus = "complete"
	RoleFailed      RoleStatus = "failed"
	RoleInterrupted RoleStatus = "interrupted"
)

// TerminalRoleStatuses is the complete terminal role-state set.
// These are the only states in which a role's execution is finished forever.
var TerminalRoleStatuses = []RoleStatus{RoleComplete, RoleFailed, RoleInterrupted}

// IsTerminal reports whether the role state is terminal.
func (s RoleStatus) IsTerminal() bool {
	for _, t := range TerminalRoleStatuses {
		if s == t {
			return true
		}
	}
	return false
}

// IsValidRoleStatus reports whether s is a canonical persistent role state.
func IsValidRoleStatus(s RoleStatus) bool {
	switch s {
	case RolePending, RoleInFlight, RoleComplete, RoleFailed, RoleInterrupted:
		return true
	}
	return false
}

// CanTransition reports whether a role may move from -> to.
//
// The legal transition set is deliberately small:
//
//	pending   -> in_flight | interrupted
//	in_flight -> complete  | failed | interrupted
//
// Terminal states have no outgoing transitions.
func CanTransition(from, to RoleStatus) bool {
	switch from {
	case RolePending:
		return to == RoleInFlight || to == RoleInterrupted
	case RoleInFlight:
		return to == RoleComplete || to == RoleFailed || to == RoleInterrupted
	}
	return false
}

// SessionStatus is the persisted state of a review session.
//
// v1 has no persistent CANCELLED session state. Cancellation is represented by
// cancel_requested plus a terminal_reason, never by a session status.
type SessionStatus string

const (
	SessionQueued    SessionStatus = "queued"
	SessionReviewing SessionStatus = "reviewing"
	SessionComplete  SessionStatus = "complete"
	SessionPartial   SessionStatus = "partial"
	SessionFailed    SessionStatus = "failed"
)

// TerminalSessionStatuses is the complete terminal session-state set.
var TerminalSessionStatuses = []SessionStatus{SessionComplete, SessionPartial, SessionFailed}

// IsTerminal reports whether the session state is terminal.
func (s SessionStatus) IsTerminal() bool {
	for _, t := range TerminalSessionStatuses {
		if s == t {
			return true
		}
	}
	return false
}

// LegalSessionTransition reports whether a session may move from -> to.
func LegalSessionTransition(from, to SessionStatus) bool {
	switch from {
	case SessionQueued:
		return to == SessionReviewing
	case SessionReviewing:
		return to.IsTerminal()
	}
	return false
}

// TerminalReason explains why a session reached its terminal state.
type TerminalReason string

const (
	ReasonAllRolesComplete TerminalReason = "all_roles_complete"
	ReasonUserCancelled    TerminalReason = "user_cancelled"
	ReasonProcessRestart   TerminalReason = "process_restart"
	ReasonDeadlineCutoff   TerminalReason = "deadline_cutoff"
)

// InterruptCause is the reason a pending or in-flight role became interrupted.
type InterruptCause string

const (
	CauseUserCancelled  InterruptCause = "user_cancelled"
	CauseDeadlineCutoff InterruptCause = "deadline_cutoff"
	CauseProcessRestart InterruptCause = "process_restart"
)
