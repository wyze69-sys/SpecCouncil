package review

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

func rowComplete(r domain.Role) RoleRow {
	return RoleRow{Role: r, Status: domain.RoleComplete}
}

func rowFailed(r domain.Role) RoleRow {
	return RoleRow{Role: r, Status: domain.RoleFailed}
}

func rowInterrupted(r domain.Role, cause domain.InterruptCause) RoleRow {
	return RoleRow{Role: r, Status: domain.RoleInterrupted, InterruptCause: cause}
}

func TestComposeVerdictTable(t *testing.T) {
	cases := []struct {
		name           string
		rows           []RoleRow
		wantStatus     domain.SessionStatus
		wantReason     domain.TerminalReason
		wantCompleted  int
		wantIncomplete int
	}{
		{
			name: "four complete",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowComplete(domain.RoleArchitecture),
				rowComplete(domain.RoleQA),
				rowComplete(domain.RoleSecurity),
			},
			wantStatus:     domain.SessionComplete,
			wantReason:     domain.ReasonAllRolesComplete,
			wantCompleted:  4,
			wantIncomplete: 0,
		},
		{
			name: "three complete plus failed",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowComplete(domain.RoleArchitecture),
				rowComplete(domain.RoleQA),
				rowFailed(domain.RoleSecurity),
			},
			wantStatus:     domain.SessionPartial,
			wantReason:     domain.ReasonRoleFailures,
			wantCompleted:  3,
			wantIncomplete: 1,
		},
		{
			name: "one complete plus process restart",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowInterrupted(domain.RoleArchitecture, domain.CauseProcessRestart),
				rowFailed(domain.RoleQA),
				rowFailed(domain.RoleSecurity),
			},
			wantStatus:     domain.SessionPartial,
			wantReason:     domain.ReasonProcessRestart,
			wantCompleted:  1,
			wantIncomplete: 3,
		},
		{
			name: "one complete plus deadline cutoff",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowInterrupted(domain.RoleArchitecture, domain.CauseDeadlineCutoff),
				rowFailed(domain.RoleQA),
				rowFailed(domain.RoleSecurity),
			},
			wantStatus:     domain.SessionPartial,
			wantReason:     domain.ReasonDeadlineCutoff,
			wantCompleted:  1,
			wantIncomplete: 3,
		},
		{
			name: "zero complete: all failed",
			rows: []RoleRow{
				rowFailed(domain.RoleRequirements),
				rowFailed(domain.RoleArchitecture),
				rowFailed(domain.RoleQA),
				rowFailed(domain.RoleSecurity),
			},
			wantStatus:     domain.SessionFailed,
			wantReason:     domain.ReasonRoleFailures,
			wantCompleted:  0,
			wantIncomplete: 4,
		},
		{
			name: "zero complete: user cancel",
			rows: []RoleRow{
				rowInterrupted(domain.RoleRequirements, domain.CauseUserCancelled),
				rowInterrupted(domain.RoleArchitecture, domain.CauseUserCancelled),
				rowInterrupted(domain.RoleQA, domain.CauseUserCancelled),
				rowInterrupted(domain.RoleSecurity, domain.CauseUserCancelled),
			},
			wantStatus:     domain.SessionPartial,
			wantReason:     domain.ReasonUserCancelled,
			wantCompleted:  0,
			wantIncomplete: 4,
		},
		{
			name: "zero complete: process restart",
			rows: []RoleRow{
				rowInterrupted(domain.RoleRequirements, domain.CauseProcessRestart),
				rowFailed(domain.RoleArchitecture),
				rowFailed(domain.RoleQA),
				rowFailed(domain.RoleSecurity),
			},
			wantStatus:     domain.SessionFailed,
			wantReason:     domain.ReasonProcessRestart,
			wantCompleted:  0,
			wantIncomplete: 4,
		},
		{
			name: "zero complete: deadline cutoff",
			rows: []RoleRow{
				rowInterrupted(domain.RoleRequirements, domain.CauseDeadlineCutoff),
				rowFailed(domain.RoleArchitecture),
				rowFailed(domain.RoleQA),
				rowFailed(domain.RoleSecurity),
			},
			wantStatus:     domain.SessionFailed,
			wantReason:     domain.ReasonDeadlineCutoff,
			wantCompleted:  0,
			wantIncomplete: 4,
		},
		{
			name: "mixed user/process/deadline proves user precedence (1 complete)",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowInterrupted(domain.RoleArchitecture, domain.CauseUserCancelled),
				rowInterrupted(domain.RoleQA, domain.CauseProcessRestart),
				rowInterrupted(domain.RoleSecurity, domain.CauseDeadlineCutoff),
			},
			wantStatus:     domain.SessionPartial,
			wantReason:     domain.ReasonUserCancelled,
			wantCompleted:  1,
			wantIncomplete: 3,
		},
		{
			name: "mixed user/process/deadline proves user precedence (0 complete)",
			rows: []RoleRow{
				rowFailed(domain.RoleRequirements),
				rowInterrupted(domain.RoleArchitecture, domain.CauseUserCancelled),
				rowInterrupted(domain.RoleQA, domain.CauseProcessRestart),
				rowInterrupted(domain.RoleSecurity, domain.CauseDeadlineCutoff),
			},
			wantStatus:     domain.SessionPartial,
			wantReason:     domain.ReasonUserCancelled,
			wantCompleted:  0,
			wantIncomplete: 4,
		},
		{
			name: "mixed process/deadline proves process precedence (1 complete)",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowFailed(domain.RoleArchitecture),
				rowInterrupted(domain.RoleQA, domain.CauseProcessRestart),
				rowInterrupted(domain.RoleSecurity, domain.CauseDeadlineCutoff),
			},
			wantStatus:     domain.SessionPartial,
			wantReason:     domain.ReasonProcessRestart,
			wantCompleted:  1,
			wantIncomplete: 3,
		},
		{
			name: "mixed process/deadline proves process precedence (0 complete)",
			rows: []RoleRow{
				rowFailed(domain.RoleRequirements),
				rowFailed(domain.RoleArchitecture),
				rowInterrupted(domain.RoleQA, domain.CauseProcessRestart),
				rowInterrupted(domain.RoleSecurity, domain.CauseDeadlineCutoff),
			},
			wantStatus:     domain.SessionFailed,
			wantReason:     domain.ReasonProcessRestart,
			wantCompleted:  0,
			wantIncomplete: 4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Compose(ComposerInput{Roles: tc.rows})
			if err != nil {
				t.Fatalf("Compose returned unexpected error: %v", err)
			}
			if v.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", v.Status, tc.wantStatus)
			}
			if v.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", v.Reason, tc.wantReason)
			}
			if v.CompletedRoleCount != tc.wantCompleted {
				t.Errorf("CompletedRoleCount = %d, want %d", v.CompletedRoleCount, tc.wantCompleted)
			}
			if v.IncompleteRoleCount != tc.wantIncomplete {
				t.Errorf("IncompleteRoleCount = %d, want %d", v.IncompleteRoleCount, tc.wantIncomplete)
			}
		})
	}
}

func TestComposeRejectsInvalidInterruptCause(t *testing.T) {
	cases := []struct {
		name string
		rows []RoleRow
	}{
		{
			name: "interrupted with empty cause",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowComplete(domain.RoleArchitecture),
				rowComplete(domain.RoleQA),
				{Role: domain.RoleSecurity, Status: domain.RoleInterrupted, InterruptCause: ""},
			},
		},
		{
			name: "interrupted with unknown cause",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowComplete(domain.RoleArchitecture),
				rowComplete(domain.RoleQA),
				{Role: domain.RoleSecurity, Status: domain.RoleInterrupted, InterruptCause: "unknown"},
			},
		},
		{
			name: "complete with cause",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowComplete(domain.RoleArchitecture),
				rowComplete(domain.RoleQA),
				{Role: domain.RoleSecurity, Status: domain.RoleComplete, InterruptCause: domain.CauseUserCancelled},
			},
		},
		{
			name: "failed with cause",
			rows: []RoleRow{
				rowComplete(domain.RoleRequirements),
				rowComplete(domain.RoleArchitecture),
				rowComplete(domain.RoleQA),
				{Role: domain.RoleSecurity, Status: domain.RoleFailed, InterruptCause: domain.CauseProcessRestart},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compose(ComposerInput{Roles: tc.rows})
			if err == nil {
				t.Errorf("%s: Compose succeeded, want rejection", tc.name)
			}
		})
	}
}

func TestCancelAfterEverythingCompletedStillCompletes(t *testing.T) {
	rows := []RoleRow{
		rowComplete(domain.RoleRequirements),
		rowComplete(domain.RoleArchitecture),
		rowComplete(domain.RoleQA),
		rowComplete(domain.RoleSecurity),
	}
	v, err := Compose(ComposerInput{Roles: rows})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if v.Status != domain.SessionComplete || v.Reason != domain.ReasonAllRolesComplete {
		t.Errorf("verdict = %s/%s, want complete/all_roles_complete", v.Status, v.Reason)
	}
	rep := BuildReport("sess-1", "snap-1", "hash-1", nil, v, true)
	if rep.Status != domain.SessionComplete || rep.Reason != domain.ReasonAllRolesComplete {
		t.Errorf("report status/reason = %s/%s, want complete/all_roles_complete", rep.Status, rep.Reason)
	}
	if !rep.CancelRequested {
		t.Errorf("report CancelRequested = false, want true")
	}
}

func TestComposeRefusesNonTerminalRoles(t *testing.T) {
	for _, s := range []domain.RoleStatus{domain.RolePending, domain.RoleInFlight} {
		_, err := Compose(ComposerInput{Roles: []RoleRow{
			rowComplete(domain.RoleRequirements),
			rowComplete(domain.RoleArchitecture),
			rowComplete(domain.RoleQA),
			{Role: domain.RoleSecurity, Status: s},
		}})
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

func TestReportJSONOutcomeCountsAndCancelRequested(t *testing.T) {
	for _, cancelReq := range []bool{false, true} {
		verdict := Verdict{
			Status:              domain.SessionComplete,
			Reason:              domain.ReasonAllRolesComplete,
			CompletedRoleCount:  4,
			IncompleteRoleCount: 0,
		}
		rep := BuildReport("sess-1", "snap-1", "hash-1", nil, verdict, cancelReq)

		data, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		jsonStr := string(data)

		// Must have incomplete_role_count, never the legacy field name.
		legacyField := "failed" + "_role" + "_count"
		if !strings.Contains(jsonStr, `"incomplete_role_count":0`) {
			t.Errorf("JSON missing incomplete_role_count: %s", jsonStr)
		}
		if strings.Contains(jsonStr, legacyField) {
			t.Errorf("JSON unexpectedly contains legacy field %q: %s", legacyField, jsonStr)
		}

		// Must always have cancel_requested with the supplied boolean value.
		expectedCancel := `"cancel_requested":false`
		if cancelReq {
			expectedCancel = `"cancel_requested":true`
		}
		if !strings.Contains(jsonStr, expectedCancel) {
			t.Errorf("JSON missing %s: %s", expectedCancel, jsonStr)
		}

		// Verify round-trip unmarshaling to a map.
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if _, exists := raw[legacyField]; exists {
			t.Errorf("raw map unexpectedly has legacy key %q", legacyField)
		}
		if incVal, ok := raw["incomplete_role_count"].(float64); !ok || int(incVal) != 0 {
			t.Errorf("incomplete_role_count in raw map = %v, want 0", raw["incomplete_role_count"])
		}
		if cancelVal, ok := raw["cancel_requested"].(bool); !ok || cancelVal != cancelReq {
			t.Errorf("cancel_requested in raw map = %v, want %v", raw["cancel_requested"], cancelReq)
		}
	}
}

func TestComposerAnchorFallbackOrdering(t *testing.T) {
	verdict := Verdict{
		Status:              domain.SessionComplete,
		Reason:              domain.ReasonAllRolesComplete,
		CompletedRoleCount:  4,
		IncompleteRoleCount: 0,
	}
	outcomes := []RoleOutcome{
		{
			Role:   domain.RoleRequirements,
			Status: domain.RoleComplete,
			Findings: []domain.Finding{
				{
					ID:             "F-2",
					Kind:           domain.FindingExisting,
					Severity:       domain.SeverityHigh,
					Category:       "correctness",
					Issue:          "issue 2",
					Recommendation: "rec 2",
					BasisRefs:      []string{"B-1"},
				},
				{
					ID:             "F-1",
					Kind:           domain.FindingMissing,
					Severity:       domain.SeverityHigh,
					Category:       "correctness",
					Issue:          "issue 1",
					Recommendation: "rec 1",
					BasisRefs:      nil,
					AnchorRef:      "A-1",
				},
			},
		},
		{Role: domain.RoleArchitecture, Status: domain.RoleComplete},
		{Role: domain.RoleQA, Status: domain.RoleComplete},
		{Role: domain.RoleSecurity, Status: domain.RoleComplete},
	}

	rep := BuildReport("sess-1", "snap-1", "hash-1", outcomes, verdict, false)
	if len(rep.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(rep.Findings))
	}
	if rep.Findings[0].Finding.ID != "F-1" {
		t.Errorf("expected first finding to be F-1 (anchored at A-1), got %s", rep.Findings[0].Finding.ID)
	}
	if rep.Findings[1].Finding.ID != "F-2" {
		t.Errorf("expected second finding to be F-2 (cites B-1), got %s", rep.Findings[1].Finding.ID)
	}
}
