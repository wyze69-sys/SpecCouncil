package review

import (
	"fmt"
	"sort"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// RoleRow is one persisted role row as the composer sees it.
type RoleRow struct {
	Role           domain.Role
	Status         domain.RoleStatus
	InterruptCause domain.InterruptCause
}

// ComposerInput is the committed state the composer derives its verdict from.
type ComposerInput struct {
	Roles []RoleRow
}

// Verdict is the terminal session decision.
type Verdict struct {
	Status              domain.SessionStatus
	Reason              domain.TerminalReason
	CompletedRoleCount  int
	IncompleteRoleCount int
}

// Compose derives the terminal session status and reason from committed role rows.
//
// The rules are evaluated first-match-wins:
//
//  1. 4 complete                    -> complete / all_roles_complete
//  2. else any interrupted/user_cancelled -> partial  / user_cancelled
//  3. else 1..3 complete            -> partial  / first remaining reason
//  4. else                          -> failed   / first remaining reason
//
// Remaining reason precedence:
//
//	process_restart -> deadline_cutoff -> role_failures
//
// Compose refuses to run unless all four roles are terminal: aggregation may
// never begin while a role is pending or in flight.
func Compose(in ComposerInput) (Verdict, error) {
	seen := make(map[domain.Role]RoleRow, len(in.Roles))
	for _, row := range in.Roles {
		if !domain.IsValidRole(row.Role) {
			return Verdict{}, fmt.Errorf("compose: unknown role %q", row.Role)
		}
		if _, dup := seen[row.Role]; dup {
			return Verdict{}, fmt.Errorf("compose: duplicate role row %q", row.Role)
		}
		if !domain.IsValidRoleStatus(row.Status) {
			return Verdict{}, fmt.Errorf("compose: role %q has invalid status %q", row.Role, row.Status)
		}
		if !row.Status.IsTerminal() {
			return Verdict{}, fmt.Errorf(
				"compose: role %q is %q, not terminal; aggregation requires all %d roles terminal",
				row.Role, row.Status, domain.RoleCount)
		}
		if row.Status == domain.RoleInterrupted {
			if !domain.IsValidInterruptCause(row.InterruptCause) {
				return Verdict{}, fmt.Errorf("compose: role %q is interrupted but has invalid interrupt cause %q", row.Role, row.InterruptCause)
			}
		} else if row.InterruptCause != "" {
			return Verdict{}, fmt.Errorf("compose: role %q has status %q but non-empty interrupt cause %q", row.Role, row.Status, row.InterruptCause)
		}
		seen[row.Role] = row
	}
	if len(seen) != domain.RoleCount {
		return Verdict{}, fmt.Errorf("compose: got %d role rows, expected %d", len(seen), domain.RoleCount)
	}

	completed := 0
	hasUserCancelled := false
	hasProcessRestart := false
	hasDeadlineCutoff := false

	for _, row := range seen {
		if row.Status == domain.RoleComplete {
			completed++
		}
		if row.Status == domain.RoleInterrupted {
			switch row.InterruptCause {
			case domain.CauseUserCancelled:
				hasUserCancelled = true
			case domain.CauseProcessRestart:
				hasProcessRestart = true
			case domain.CauseDeadlineCutoff:
				hasDeadlineCutoff = true
			}
		}
	}

	verdict := Verdict{
		CompletedRoleCount:  completed,
		IncompleteRoleCount: domain.RoleCount - completed,
	}

	remainingReason := func() domain.TerminalReason {
		switch {
		case hasProcessRestart:
			return domain.ReasonProcessRestart
		case hasDeadlineCutoff:
			return domain.ReasonDeadlineCutoff
		default:
			return domain.ReasonRoleFailures
		}
	}

	switch {
	case completed == domain.RoleCount:
		verdict.Status = domain.SessionComplete
		verdict.Reason = domain.ReasonAllRolesComplete
	case hasUserCancelled:
		verdict.Status = domain.SessionPartial
		verdict.Reason = domain.ReasonUserCancelled
	case completed > 0:
		verdict.Status = domain.SessionPartial
		verdict.Reason = remainingReason()
	default:
		verdict.Status = domain.SessionFailed
		verdict.Reason = remainingReason()
	}

	return verdict, nil
}

// RoleSummary is one role's contribution to the report.
type RoleSummary struct {
	Role           domain.Role           `json:"role"`
	Status         domain.RoleStatus     `json:"status"`
	ErrorCategory  domain.ErrorCategory  `json:"error_category,omitempty"`
	InterruptCause domain.InterruptCause `json:"interrupt_cause,omitempty"`
	FindingCount   int                   `json:"finding_count"`
	CallCount      int                   `json:"provider_call_count"`
	LastPurpose    domain.CallPurpose    `json:"last_call_purpose,omitempty"`
}

// ReportFinding is one finding together with the role that produced it.
type ReportFinding struct {
	Role    domain.Role    `json:"role"`
	Finding domain.Finding `json:"finding"`
}

// Report is the deterministic artifact the owner reads.
type Report struct {
	SessionID    string `json:"session_id"`
	SnapshotID   string `json:"snapshot_id"`
	SnapshotHash string `json:"snapshot_hash"`

	Status          domain.SessionStatus  `json:"status"`
	Reason          domain.TerminalReason `json:"terminal_reason,omitempty"`
	CancelRequested bool                  `json:"cancel_requested"`

	CompletedRoleCount  int `json:"completed_role_count"`
	IncompleteRoleCount int `json:"incomplete_role_count"`

	Roles    []RoleSummary   `json:"roles"`
	Findings []ReportFinding `json:"findings"`
}

// BuildReport assembles the report from committed role outcomes.
//
// Ordering is a total order, so the report bytes do not depend on execution
// order or on which role happened to finish first:
//
//	severity rank, role rank, primary basis_ref, category, finding id
func BuildReport(
	sessionID string,
	snapshotID string,
	snapshotHash string,
	outcomes []RoleOutcome,
	verdict Verdict,
	cancelRequested bool,
) Report {
	report := Report{
		SessionID:           sessionID,
		SnapshotID:          snapshotID,
		SnapshotHash:        snapshotHash,
		Status:              verdict.Status,
		Reason:              verdict.Reason,
		CancelRequested:     cancelRequested,
		CompletedRoleCount:  verdict.CompletedRoleCount,
		IncompleteRoleCount: verdict.IncompleteRoleCount,
	}

	roles := make([]RoleSummary, 0, len(outcomes))
	for _, o := range outcomes {
		roles = append(roles, RoleSummary{
			Role:           o.Role,
			Status:         o.Status,
			ErrorCategory:  o.ErrorCategory,
			InterruptCause: o.InterruptCause,
			FindingCount:   len(o.Findings),
			CallCount:      o.CallCount,
			LastPurpose:    o.LastPurpose,
		})
		if o.Status != domain.RoleComplete {
			// A failed or interrupted role contributes no findings.
			continue
		}
		for _, f := range o.Findings {
			report.Findings = append(report.Findings, ReportFinding{Role: o.Role, Finding: f})
		}
	}
	sort.Slice(roles, func(i, j int) bool {
		return domain.RoleOrder(roles[i].Role) < domain.RoleOrder(roles[j].Role)
	})
	report.Roles = roles

	sort.SliceStable(report.Findings, func(i, j int) bool {
		return findingLess(report.Findings[i], report.Findings[j])
	})
	return report
}

// findingLess is the frozen total order over report findings.
func findingLess(a, b ReportFinding) bool {
	as, bs := domain.SeverityRank(a.Finding.Severity), domain.SeverityRank(b.Finding.Severity)
	if as != bs {
		return as < bs
	}
	ar, br := domain.RoleOrder(a.Role), domain.RoleOrder(b.Role)
	if ar != br {
		return ar < br
	}
	ap, bp := primaryRef(a.Finding), primaryRef(b.Finding)
	if ap != bp {
		return ap < bp
	}
	if a.Finding.Category != b.Finding.Category {
		return a.Finding.Category < b.Finding.Category
	}
	return a.Finding.ID < b.Finding.ID
}

// primaryRef is the lowest basis_ref of a finding (or AnchorRef when basis_refs is empty), used as a stable tiebreak.
func primaryRef(f domain.Finding) string {
	if len(f.BasisRefs) == 0 {
		return f.AnchorRef
	}
	lowest := f.BasisRefs[0]
	for _, ref := range f.BasisRefs[1:] {
		if ref < lowest {
			lowest = ref
		}
	}
	return lowest
}
