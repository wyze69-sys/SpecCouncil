package review

import (
	"context"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
)

// RunOptions carries the runtime signals the dispatcher observes.
type RunOptions struct {
	// Cancelled is consulted before every dispatch. A nil func never cancels.
	//
	// Cancellation is cooperative: it stops future dispatch only. A role that
	// has already started is allowed to finish.
	Cancelled func() bool
}

// Engine executes one review session.
//
// The first milestone runs the four roles sequentially and owns no HTTP, no
// database and no concurrency. Bounded parallelism is added later without
// changing the composer or the report.
type Engine struct {
	Provider provider.Provider
	Budget   Budget
	Policy   Policy
}

// Run executes all four roles to a terminal state and returns the report.
func (e Engine) Run(
	ctx context.Context,
	sessionID string,
	snap evidence.Snapshot,
	opts RunOptions,
) (Report, error) {
	outcomes := make([]RoleOutcome, 0, domain.RoleCount)
	cancelObserved := false

	for _, role := range domain.Roles {
		if opts.Cancelled != nil && opts.Cancelled() {
			// Cancellation stops new dispatch. The role never runs.
			cancelObserved = true
			outcomes = append(outcomes, interrupted(role, domain.CauseUserCancelled))
			continue
		}
		outcomes = append(outcomes, RunRole(ctx, e.Provider, role, snap, e.Budget, e.Policy))
	}

	verdict, err := Compose(ComposerInput{
		Roles:           rowsFrom(outcomes),
		CancelRequested: cancelObserved,
	})
	if err != nil {
		return Report{}, err
	}

	return BuildReport(sessionID, snap.ID, snap.Hash, outcomes, verdict), nil
}

// interrupted builds the terminal outcome of a role that never executed.
func interrupted(role domain.Role, cause domain.InterruptCause) RoleOutcome {
	return RoleOutcome{
		Role:           role,
		Status:         domain.RoleInterrupted,
		InterruptCause: cause,
	}
}

// rowsFrom projects role outcomes into the composer's row view.
func rowsFrom(outcomes []RoleOutcome) []RoleRow {
	rows := make([]RoleRow, 0, len(outcomes))
	for _, o := range outcomes {
		rows = append(rows, RoleRow{Role: o.Role, Status: o.Status})
	}
	return rows
}
