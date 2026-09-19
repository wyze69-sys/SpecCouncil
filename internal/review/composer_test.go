package review

import (
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

func rows(statuses ...domain.RoleStatus) []RoleRow {
	out := make([]RoleRow, 0, len(statuses))
	for i, s := range statuses {
		out = append(out, RoleRow{Role: domain.Roles[i], Status: s})
	}
	return out
}

// AC-01: all four roles valid.
func TestAllRolesCompleteIsComplete(t *testing.T) {
	v, err := Compose(ComposerInput{Roles: rows(
		domain.RoleComplete, domain.RoleComplete, domain.RoleComplete, domain.RoleComplete)})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if v.Status != domain.SessionComplete {
		t.Errorf("status = %s, want complete", v.Status)
	}
	if v.Reason != domain.ReasonAllRolesComplete {
		t.Errorf("reason = %s, want all_roles_complete", v.Reason)
	}
	if v.CompletedRoleCount != 4 || v.FailedRoleCount != 0 {
		t.Errorf("counts = %d/%d, want 4/0", v.CompletedRoleCount, v.FailedRoleCount)
	}
}

// AC-02: three valid, one validation failure.
func TestThreeCompleteIsPartial(t *testing.T) {
	v, err := Compose(ComposerInput{Roles: rows(
		domain.RoleComplete, domain.RoleComplete, domain.RoleComplete, domain.RoleFailed)})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if v.Status != domain.SessionPartial {
		t.Errorf("status = %s, want partial", v.Status)
	}
	if v.CompletedRoleCount != 3 || v.FailedRoleCount != 1 {
		t.Errorf("counts = %d/%d, want 3/1", v.CompletedRoleCount, v.FailedRoleCount)
	}
}

// AC-03: all four fail.
func TestNoCompleteRolesIsFailed(t *testing.T) {
	v, err := Compose(ComposerInput{Roles: rows(
		domain.RoleFailed, domain.RoleFailed, domain.RoleFailed, domain.RoleFailed)})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if v.Status != domain.SessionFailed {
		t.Errorf("status = %s, want failed", v.Status)
	}
	if v.CompletedRoleCount != 0 || v.FailedRoleCount != 4 {
		t.Errorf("counts = %d/%d, want 0/4", v.CompletedRoleCount, v.FailedRoleCount)
	}
}

// Interrupted roles are not complete, so they count toward failed_role_count.
func TestInterruptedRolesCountAsNotCompleted(t *testing.T) {
	v, err := Compose(ComposerInput{
		Roles: rows(domain.RoleComplete, domain.RoleInterrupted,
			domain.RoleInterrupted, domain.RoleInterrupted),
		CancelRequested: true,
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if v.Status != domain.SessionPartial || v.Reason != domain.ReasonUserCancelled {
		t.Errorf("verdict = %s/%s, want partial/user_cancelled", v.Status, v.Reason)
	}
	if v.CompletedRoleCount != 1 || v.FailedRoleCount != 3 {
		t.Errorf("counts = %d/%d, want 1/3", v.CompletedRoleCount, v.FailedRoleCount)
	}
}

// Rule 1 wins over cancellation: a late cancel does not change a finished review.
func TestCancelAfterEverythingCompletedStillCompletes(t *testing.T) {
	v, err := Compose(ComposerInput{
		Roles: rows(domain.RoleComplete, domain.RoleComplete,
			domain.RoleComplete, domain.RoleComplete),
		CancelRequested: true,
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if v.Status != domain.SessionComplete || v.Reason != domain.ReasonAllRolesComplete {
		t.Errorf("verdict = %s/%s, want complete/all_roles_complete", v.Status, v.Reason)
	}
}

// Aggregation may never start while a role is still executable.
func TestComposeRefusesNonTerminalRoles(t *testing.T) {
	for _, s := range []domain.RoleStatus{domain.RolePending, domain.RoleInFlight} {
		_, err := Compose(ComposerInput{Roles: rows(
			domain.RoleComplete, domain.RoleComplete, domain.RoleComplete, s)})
		if err == nil {
			t.Errorf("role status %s: Compose succeeded, want refusal", s)
		}
	}
}

func TestComposeRefusesMalformedRowSets(t *testing.T) {
	cases := map[string][]RoleRow{
		"too few rows": {
			{Role: domain.RoleRequirements, Status: domain.RoleComplete},
			{Role: domain.RoleArchitecture, Status: domain.RoleComplete},
		},
		"duplicate role": {
			{Role: domain.RoleRequirements, Status: domain.RoleComplete},
			{Role: domain.RoleRequirements, Status: domain.RoleComplete},
			{Role: domain.RoleQA, Status: domain.RoleComplete},
			{Role: domain.RoleSecurity, Status: domain.RoleComplete},
		},
		"unknown role": {
			{Role: "clarity", Status: domain.RoleComplete},
			{Role: domain.RoleArchitecture, Status: domain.RoleComplete},
			{Role: domain.RoleQA, Status: domain.RoleComplete},
			{Role: domain.RoleSecurity, Status: domain.RoleComplete},
		},
		"unknown status": {
			{Role: domain.RoleRequirements, Status: "running"},
			{Role: domain.RoleArchitecture, Status: domain.RoleComplete},
			{Role: domain.RoleQA, Status: domain.RoleComplete},
			{Role: domain.RoleSecurity, Status: domain.RoleComplete},
		},
	}
	for name, r := range cases {
		if _, err := Compose(ComposerInput{Roles: r}); err == nil {
			t.Errorf("%s: Compose succeeded, want refusal", name)
		}
	}
}
