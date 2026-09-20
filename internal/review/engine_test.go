package review

import (
	"context"
	"reflect"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/provider/fake"
)

func scriptAll(calls ...fake.ScriptedCall) map[domain.Role][]fake.ScriptedCall {
	m := make(map[domain.Role][]fake.ScriptedCall, domain.RoleCount)
	for _, r := range domain.Roles {
		m[r] = calls
	}
	return m
}

func newEngine(s map[domain.Role][]fake.ScriptedCall) (Engine, *fake.FakeProvider) {
	fake := fake.NewFakeProvider(s)
	return Engine{Provider: fake, Budget: Budget{}, Policy: DefaultPolicy()}, fake
}

func TestEngineRunsAllFourRolesAndReports(t *testing.T) {
	eng, fake := newEngine(scriptAll(fake.ScriptedCall{Body: validBody()}))

	report, err := eng.Run(context.Background(), "rev-1", testSnapshot(t), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.Status != domain.SessionComplete || report.Reason != domain.ReasonAllRolesComplete {
		t.Errorf("verdict = %s/%s, want complete/all_roles_complete", report.Status, report.Reason)
	}
	if report.CompletedRoleCount != 4 || report.IncompleteRoleCount != 0 {
		t.Errorf("counts = %d/%d, want 4/0", report.CompletedRoleCount, report.IncompleteRoleCount)
	}
	if report.CancelRequested {
		t.Errorf("cancel_requested = true, want false")
	}
	if len(report.Roles) != domain.RoleCount {
		t.Fatalf("report has %d role summaries, want %d", len(report.Roles), domain.RoleCount)
	}
	if got := len(fake.Calls()); got != domain.RoleCount {
		t.Errorf("provider calls = %d, want %d (one per role)", got, domain.RoleCount)
	}
	if report.SnapshotHash == "" || report.SnapshotID != "snap-1" {
		t.Error("report must identify the frozen snapshot it was built from")
	}
}

// AC-04: cancel before the first dispatch makes no provider call at all.
func TestCancelBeforeFirstDispatchMakesNoProviderCall(t *testing.T) {
	eng, fake := newEngine(scriptAll(fake.ScriptedCall{Body: validBody()}))

	report, err := eng.Run(context.Background(), "rev-1", testSnapshot(t),
		RunOptions{Cancelled: func() bool { return true }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(fake.Calls()); got != 0 {
		t.Errorf("provider calls = %d, want 0", got)
	}
	for _, r := range report.Roles {
		if r.Status != domain.RoleInterrupted {
			t.Errorf("role %s = %s, want interrupted", r.Role, r.Status)
		}
		if r.InterruptCause != domain.CauseUserCancelled {
			t.Errorf("role %s cause = %q, want user_cancelled", r.Role, r.InterruptCause)
		}
	}
	if report.Status != domain.SessionPartial || report.Reason != domain.ReasonUserCancelled {
		t.Errorf("verdict = %s/%s, want partial/user_cancelled", report.Status, report.Reason)
	}
	if len(report.Findings) != 0 {
		t.Errorf("report has %d findings, want 0", len(report.Findings))
	}
}

// AC-05 shape: cancellation stops new dispatch but lets started roles finish.
func TestCancelMidwayKeepsCompletedFindings(t *testing.T) {
	eng, fake := newEngine(scriptAll(fake.ScriptedCall{Body: validBody()}))

	dispatches := 0
	report, err := eng.Run(context.Background(), "rev-1", testSnapshot(t),
		RunOptions{Cancelled: func() bool {
			dispatches++
			return dispatches > 2
		}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	completed, interrupted := 0, 0
	for _, r := range report.Roles {
		switch r.Status {
		case domain.RoleComplete:
			completed++
		case domain.RoleInterrupted:
			interrupted++
		}
	}
	if completed != 2 || interrupted != 2 {
		t.Errorf("roles = %d complete / %d interrupted, want 2/2", completed, interrupted)
	}
	if got := len(fake.Calls()); got != 2 {
		t.Errorf("provider calls = %d, want 2 (only dispatched roles call)", got)
	}
	// Findings from roles that finished before the cancel are preserved.
	if len(report.Findings) != 2 {
		t.Errorf("report has %d findings, want 2 preserved", len(report.Findings))
	}
}

func TestReportIsByteIdenticalAcrossRuns(t *testing.T) {
	body := mkResult(mkFinding("F-1", "high", "R-1"))

	first := runReport(t, scriptAll(fake.ScriptedCall{Body: body}))
	second := runReport(t, scriptAll(fake.ScriptedCall{Body: body}))

	if !reflect.DeepEqual(first, second) {
		t.Errorf("report is not deterministic:\n%+v\n%+v", first, second)
	}
}

func runReport(t *testing.T, s map[domain.Role][]fake.ScriptedCall) Report {
	t.Helper()
	eng, _ := newEngine(s)
	report, err := eng.Run(context.Background(), "rev-1", testSnapshot(t), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

// Completion order must never influence report ordering.
func TestReportOrderingIgnoresOutcomeOrder(t *testing.T) {
	snap := testSnapshot(t)
	outcomes := []RoleOutcome{
		{Role: domain.RoleSecurity, Status: domain.RoleComplete,
			Findings: []domain.Finding{{ID: "S-1", Severity: domain.SeverityHigh, Category: "exposure", Issue: "i", Recommendation: "r", BasisRefs: []string{"C-1"}}}},
		{Role: domain.RoleRequirements, Status: domain.RoleComplete,
			Findings: []domain.Finding{{ID: "R-9", Severity: domain.SeverityCritical, Category: "scope", Issue: "i", Recommendation: "r", BasisRefs: []string{"F-1"}}}},
		{Role: domain.RoleArchitecture, Status: domain.RoleComplete},
		{Role: domain.RoleQA, Status: domain.RoleComplete},
	}
	verdict, err := Compose(ComposerInput{Roles: rowsFrom(outcomes)})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	forward := BuildReport("rev-1", snap.ID, snap.Hash, outcomes, verdict, false)

	shuffled := []RoleOutcome{outcomes[2], outcomes[3], outcomes[0], outcomes[1]}
	verdict2, err := Compose(ComposerInput{Roles: rowsFrom(shuffled)})
	if err != nil {
		t.Fatalf("Compose shuffled: %v", err)
	}
	backward := BuildReport("rev-1", snap.ID, snap.Hash, shuffled, verdict2, false)

	if !reflect.DeepEqual(forward, backward) {
		t.Error("report differs when roles finish in a different order")
	}

	// Critical sorts before high, regardless of which role produced it.
	if forward.Findings[0].Finding.ID != "R-9" {
		t.Errorf("first finding = %s, want R-9 (critical outranks high)", forward.Findings[0].Finding.ID)
	}
}

// A failed role must contribute no findings to the report.
func TestFailedRoleContributesNoFindings(t *testing.T) {
	snap := testSnapshot(t)
	outcomes := []RoleOutcome{
		{Role: domain.RoleRequirements, Status: domain.RoleFailed, ErrorCategory: domain.ErrInvalidJSON,
			Findings: []domain.Finding{{ID: "ghost", Severity: domain.SeverityHigh, Issue: "i", Recommendation: "r", BasisRefs: []string{"R-1"}}}},
		{Role: domain.RoleArchitecture, Status: domain.RoleComplete},
		{Role: domain.RoleQA, Status: domain.RoleComplete},
		{Role: domain.RoleSecurity, Status: domain.RoleComplete},
	}
	verdict, err := Compose(ComposerInput{Roles: rowsFrom(outcomes)})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	report := BuildReport("rev-1", snap.ID, snap.Hash, outcomes, verdict, false)
	if len(report.Findings) != 0 {
		t.Errorf("report has %d findings from a failed role, want 0", len(report.Findings))
	}
}
