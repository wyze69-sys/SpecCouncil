package domain

import "testing"

func TestRoleOrderIsCanonical(t *testing.T) {
	want := []Role{RoleRequirements, RoleArchitecture, RoleQA, RoleSecurity}
	if len(Roles) != RoleCount {
		t.Fatalf("Roles has %d entries, want %d", len(Roles), RoleCount)
	}
	for i, r := range want {
		if got := RoleOrder(r); got != i {
			t.Errorf("RoleOrder(%s) = %d, want %d", r, got, i)
		}
	}
	if got := RoleOrder("clarity"); got != -1 {
		t.Errorf("RoleOrder(clarity) = %d, want -1: placeholder roles are not canonical", got)
	}
}

func TestRoleStatusTerminalSet(t *testing.T) {
	terminal := map[RoleStatus]bool{
		RolePending:     false,
		RoleInFlight:    false,
		RoleComplete:    true,
		RoleFailed:      true,
		RoleInterrupted: true,
	}
	for status, want := range terminal {
		if got := status.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", status, got, want)
		}
	}
}

func TestRoleTransitionGuard(t *testing.T) {
	legal := []struct{ from, to RoleStatus }{
		{RolePending, RoleInFlight},
		{RolePending, RoleInterrupted},
		{RoleInFlight, RoleComplete},
		{RoleInFlight, RoleFailed},
		{RoleInFlight, RoleInterrupted},
	}
	for _, c := range legal {
		if !CanTransition(c.from, c.to) {
			t.Errorf("CanTransition(%s, %s) = false, want true", c.from, c.to)
		}
	}

	illegal := []struct{ from, to RoleStatus }{
		{RolePending, RoleComplete},
		{RolePending, RoleFailed},
		{RoleComplete, RoleInFlight},
		{RoleFailed, RolePending},
		{RoleInterrupted, RoleInFlight},
		{RoleInFlight, RolePending},
	}
	for _, c := range illegal {
		if CanTransition(c.from, c.to) {
			t.Errorf("CanTransition(%s, %s) = true, want false", c.from, c.to)
		}
	}
}

func TestSessionHasNoCancelledState(t *testing.T) {
	// Cancellation is cancel_requested plus a reason, never a session status.
	for _, forbidden := range []SessionStatus{"cancelled", "canceled"} {
		if forbidden.IsTerminal() {
			t.Errorf("%q must not be a terminal session state", forbidden)
		}
	}
	if !LegalSessionTransition(SessionQueued, SessionReviewing) {
		t.Error("queued -> reviewing must be legal")
	}
	if LegalSessionTransition(SessionComplete, SessionPartial) {
		t.Error("terminal session states must not transition")
	}
}

func TestIsValidTerminalReason(t *testing.T) {
	cases := []struct {
		reason TerminalReason
		want   bool
	}{
		{ReasonAllRolesComplete, true},
		{ReasonUserCancelled, true},
		{ReasonProcessRestart, true},
		{ReasonDeadlineCutoff, true},
		{ReasonRoleFailures, true},
		{TerminalReason(""), false},
		{TerminalReason("unknown"), false},
		{TerminalReason("timeout"), false},
		{TerminalReason("all_roles_done"), false},
	}
	for _, tc := range cases {
		if got := IsValidTerminalReason(tc.reason); got != tc.want {
			t.Errorf("IsValidTerminalReason(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

func TestIsValidInterruptCause(t *testing.T) {
	cases := []struct {
		cause InterruptCause
		want  bool
	}{
		{CauseUserCancelled, true},
		{CauseDeadlineCutoff, true},
		{CauseProcessRestart, true},
		{InterruptCause(""), false},
		{InterruptCause("unknown"), false},
		{InterruptCause("role_failures"), false},
		{InterruptCause("timeout"), false},
	}
	for _, tc := range cases {
		if got := IsValidInterruptCause(tc.cause); got != tc.want {
			t.Errorf("IsValidInterruptCause(%q) = %v, want %v", tc.cause, got, tc.want)
		}
	}
}
