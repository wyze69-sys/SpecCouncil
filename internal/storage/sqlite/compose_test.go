package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

// Helper to set up a reviewing session ready for role execution and composition.
func setupComposeTestSession(t *testing.T, sessionID, projectID string) (*Store, *sql.DB, time.Time, evidence.Snapshot) {
	t.Helper()
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	t.Cleanup(func() { clock = origClock })

	snap := createTestFrozenSnapshot(t, "snap_"+sessionID)
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      sessionID,
		ProjectID:      projectID,
		IdempotencyKey: "key_" + sessionID,
		Title:          "Composition Test " + sessionID,
		Content:        "Spec content for composition tests",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit %s: %v", sessionID, err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	claimRes, err := store.ClaimSession(ctx, policy)
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim %s: %v", sessionID, err)
	}

	return store, writer, t0, snap
}

// 1. All-Role Success Composition
func TestCompose_AllRoleSuccess(t *testing.T) {
	store, _, t0, _ := setupComposeTestSession(t, "sess_comp_all_succ", "proj_comp")
	ctx := context.Background()

	// Complete all 4 roles in order with findings and basis refs.
	roleFindings := map[domain.Role][]domain.Finding{
		domain.RoleRequirements: {
			{
				ID:             "find-req-1",
				Kind:           domain.FindingExisting,
				Severity:       domain.SeverityHigh,
				Category:       "correctness",
				Issue:          "Ambiguous requirement phrasing",
				Recommendation: "Clarify wording",
				BasisRefs:      []string{"req-1"},
			},
		},
		domain.RoleArchitecture: {
			{
				ID:             "find-arch-1",
				Kind:           domain.FindingExisting,
				Severity:       domain.SeverityCritical,
				Category:       "architecture",
				Issue:          "Single point of failure",
				Recommendation: "Introduce clustering",
				BasisRefs:      []string{"arch-1", "brief-1"},
			},
		},
		domain.RoleQA: {
			{
				ID:             "find-qa-1",
				Kind:           domain.FindingExisting,
				Severity:       domain.SeverityLow,
				Category:       "testability",
				Issue:          "Missing test scenarios",
				Recommendation: "Add integration tests",
				BasisRefs:      []string{"flow-1"},
			},
		},
		domain.RoleSecurity: {
			{
				ID:             "find-sec-1",
				Kind:           domain.FindingExisting,
				Severity:       domain.SeverityHigh,
				Category:       "security",
				Issue:          "Cleartext secret storage",
				Recommendation: "Use secret vault",
				BasisRefs:      []string{"const-1", "data-1"},
			},
		},
	}

	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_comp_all_succ", t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve role %s: %v", role, err)
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_comp_all_succ",
			RoleRunID:   res.RoleRunID,
			Role:        role,
			Findings:    roleFindings[role],
			CallCount:   1,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish role %s: %v", role, err)
		}
	}

	composeTime := t0.Add(10 * time.Minute)
	res, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_comp_all_succ",
		Now:       composeTime,
	})
	if err != nil {
		t.Fatalf("ComposeSession failed: %v", err)
	}

	if !res.IsComposed() {
		t.Errorf("expected IsComposed() true, got false")
	}
	if res.IsAlreadyTerminal() {
		t.Errorf("expected IsAlreadyTerminal() false, got true")
	}
	if res.Status != domain.SessionComplete {
		t.Errorf("status = %s, want complete", res.Status)
	}
	if res.TerminalReason != domain.ReasonAllRolesComplete {
		t.Errorf("terminal_reason = %s, want all_roles_complete", res.TerminalReason)
	}
	if res.Report == nil {
		t.Fatalf("expected non-nil Report")
	}
	if res.Report.CompletedRoleCount != 4 || res.Report.IncompleteRoleCount != 0 {
		t.Errorf("completed/incomplete = %d/%d, want 4/0", res.Report.CompletedRoleCount, res.Report.IncompleteRoleCount)
	}
	if len(res.Report.Findings) != 4 {
		t.Fatalf("expected 4 findings, got %d", len(res.Report.Findings))
	}

	// Verify total order of findings: severity rank -> role rank -> primary basis_ref -> category -> finding id
	// Critical arch-1 outranks High req-1 and sec-1; High req-1 outranks High sec-1 (req outranks sec in role order); Low qa-1 is last.
	wantOrder := []string{"find-arch-1", "find-req-1", "find-sec-1", "find-qa-1"}
	for i, wantID := range wantOrder {
		if res.Report.Findings[i].Finding.ID != wantID {
			t.Errorf("finding[%d] = %s, want %s", i, res.Report.Findings[i].Finding.ID, wantID)
		}
	}
}

// 2. Partial with Cancellation (both with completed roles and zero completed roles)
func TestCompose_PartialWithCancellation(t *testing.T) {
	t.Run("partial with completed role and cancellation", func(t *testing.T) {
		store, _, t0, _ := setupComposeTestSession(t, "sess_part_cancel_1", "proj_part_cancel")
		ctx := context.Background()

		// Complete 1 role (Requirements).
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_part_cancel_1", t0)
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_part_cancel_1",
			RoleRunID:   res.RoleRunID,
			Role:        domain.RoleRequirements,
			CallCount:   1,
			CompletedAt: t0.Add(1 * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}

		// Request cancellation and sweep remaining pending roles to interrupted/user_cancelled.
		_, err = store.RequestCancellation(ctx, "sess_part_cancel_1")
		if err != nil {
			t.Fatalf("RequestCancellation: %v", err)
		}
		sweepRes, err := store.SweepCancellation(ctx, "sess_part_cancel_1")
		if err != nil || sweepRes.InterruptedCount != 3 {
			t.Fatalf("SweepCancellation: %v (count=%d)", err, sweepRes.InterruptedCount)
		}

		compRes, err := store.ComposeSession(ctx, "sess_part_cancel_1")
		if err != nil {
			t.Fatalf("ComposeSession: %v", err)
		}

		if compRes.Status != domain.SessionPartial {
			t.Errorf("status = %s, want partial", compRes.Status)
		}
		if compRes.TerminalReason != domain.ReasonUserCancelled {
			t.Errorf("terminal_reason = %s, want user_cancelled", compRes.TerminalReason)
		}
		if compRes.Report.CompletedRoleCount != 1 || compRes.Report.IncompleteRoleCount != 3 {
			t.Errorf("counts = %d/%d, want 1/3", compRes.Report.CompletedRoleCount, compRes.Report.IncompleteRoleCount)
		}
		if !compRes.Report.CancelRequested {
			t.Errorf("report CancelRequested = false, want true")
		}
	})

	t.Run("partial with zero completed roles and cancellation", func(t *testing.T) {
		store, _, _, _ := setupComposeTestSession(t, "sess_part_cancel_0", "proj_part_cancel")
		ctx := context.Background()

		// Cancel immediately before any role is reserved.
		_, err := store.RequestCancellation(ctx, "sess_part_cancel_0")
		if err != nil {
			t.Fatalf("RequestCancellation: %v", err)
		}
		sweepRes, err := store.SweepCancellation(ctx, "sess_part_cancel_0")
		if err != nil || sweepRes.InterruptedCount != 4 {
			t.Fatalf("SweepCancellation: %v (count=%d)", err, sweepRes.InterruptedCount)
		}

		compRes, err := store.ComposeSession(ctx, "sess_part_cancel_0")
		if err != nil {
			t.Fatalf("ComposeSession: %v", err)
		}

		// Canonical flow: user-cancelled session is partial even when 0 roles completed.
		if compRes.Status != domain.SessionPartial {
			t.Errorf("status = %s, want partial", compRes.Status)
		}
		if compRes.TerminalReason != domain.ReasonUserCancelled {
			t.Errorf("terminal_reason = %s, want user_cancelled", compRes.TerminalReason)
		}
		if compRes.Report.CompletedRoleCount != 0 || compRes.Report.IncompleteRoleCount != 4 {
			t.Errorf("counts = %d/%d, want 0/4", compRes.Report.CompletedRoleCount, compRes.Report.IncompleteRoleCount)
		}
	})
}

// 3. Partial with Completed Roles (not cancelled, e.g. failures or deadline cutoff)
func TestCompose_PartialWithCompletedRoles(t *testing.T) {
	t.Run("2 complete, 2 failed -> partial with role_failures", func(t *testing.T) {
		store, _, t0, _ := setupComposeTestSession(t, "sess_part_fail", "proj_part")
		ctx := context.Background()

		// Complete 2 roles.
		for _, role := range []domain.Role{domain.RoleRequirements, domain.RoleArchitecture} {
			res, err := store.ReservePendingRoleWithNow(ctx, "sess_part_fail", t0)
			if err != nil || !res.Reserved {
				t.Fatalf("reserve: %v", err)
			}
			_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
				SessionID:   "sess_part_fail",
				RoleRunID:   res.RoleRunID,
				Role:        role,
				CallCount:   1,
				CompletedAt: t0.Add(1 * time.Minute),
			})
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
		}

		// Fail 2 roles.
		for _, role := range []domain.Role{domain.RoleQA, domain.RoleSecurity} {
			res, err := store.ReservePendingRoleWithNow(ctx, "sess_part_fail", t0)
			if err != nil || !res.Reserved {
				t.Fatalf("reserve: %v", err)
			}
			_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
				SessionID:     "sess_part_fail",
				RoleRunID:     res.RoleRunID,
				Role:          role,
				ErrorCategory: domain.ErrTimeout,
				ErrorMessage:  "timed out",
				CallCount:     2,
				CompletedAt:   t0.Add(2 * time.Minute),
			})
			if err != nil {
				t.Fatalf("publish failure: %v", err)
			}
		}

		compRes, err := store.ComposeSession(ctx, "sess_part_fail")
		if err != nil {
			t.Fatalf("ComposeSession: %v", err)
		}

		if compRes.Status != domain.SessionPartial {
			t.Errorf("status = %s, want partial", compRes.Status)
		}
		if compRes.TerminalReason != domain.ReasonRoleFailures {
			t.Errorf("terminal_reason = %s, want role_failures", compRes.TerminalReason)
		}
		if compRes.Report.CompletedRoleCount != 2 || compRes.Report.IncompleteRoleCount != 2 {
			t.Errorf("counts = %d/%d, want 2/2", compRes.Report.CompletedRoleCount, compRes.Report.IncompleteRoleCount)
		}
	})

	t.Run("1 complete, 3 cutoff -> partial with deadline_cutoff", func(t *testing.T) {
		store, _, t0, _ := setupComposeTestSession(t, "sess_part_cutoff", "proj_part")
		ctx := context.Background()

		// Complete 1 role.
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_part_cutoff", t0)
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_part_cutoff",
			RoleRunID:   res.RoleRunID,
			Role:        domain.RoleRequirements,
			CallCount:   1,
			CompletedAt: t0.Add(1 * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}

		// Cutoff sweep for remaining pending roles at now >= dispatch_cutoff_at (t0 + 30m).
		cutoffTime := t0.Add(35 * time.Minute)
		sweepRes, err := store.SweepCutoff(ctx, "sess_part_cutoff", cutoffTime)
		if err != nil || sweepRes.InterruptedCount != 3 {
			t.Fatalf("SweepCutoff: %v (count=%d)", err, sweepRes.InterruptedCount)
		}

		compRes, err := store.ComposeSession(ctx, "sess_part_cutoff")
		if err != nil {
			t.Fatalf("ComposeSession: %v", err)
		}

		if compRes.Status != domain.SessionPartial {
			t.Errorf("status = %s, want partial", compRes.Status)
		}
		if compRes.TerminalReason != domain.ReasonDeadlineCutoff {
			t.Errorf("terminal_reason = %s, want deadline_cutoff", compRes.TerminalReason)
		}
		if compRes.Report.CompletedRoleCount != 1 || compRes.Report.IncompleteRoleCount != 3 {
			t.Errorf("counts = %d/%d, want 1/3", compRes.Report.CompletedRoleCount, compRes.Report.IncompleteRoleCount)
		}
	})
}

// 4. Failed with No Completed Roles
func TestCompose_FailedWithNoCompletedRoles(t *testing.T) {
	t.Run("all 4 roles failed -> failed with role_failures", func(t *testing.T) {
		store, _, t0, _ := setupComposeTestSession(t, "sess_fail_all", "proj_fail")
		ctx := context.Background()

		for _, role := range domain.Roles {
			res, err := store.ReservePendingRoleWithNow(ctx, "sess_fail_all", t0)
			if err != nil || !res.Reserved {
				t.Fatalf("reserve: %v", err)
			}
			_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
				SessionID:     "sess_fail_all",
				RoleRunID:     res.RoleRunID,
				Role:          role,
				ErrorCategory: domain.ErrProviderRejected,
				ErrorMessage:  "rejected",
				CallCount:     1,
				CompletedAt:   t0.Add(1 * time.Minute),
			})
			if err != nil {
				t.Fatalf("publish failure: %v", err)
			}
		}

		compRes, err := store.ComposeSession(ctx, "sess_fail_all")
		if err != nil {
			t.Fatalf("ComposeSession: %v", err)
		}

		if compRes.Status != domain.SessionFailed {
			t.Errorf("status = %s, want failed", compRes.Status)
		}
		if compRes.TerminalReason != domain.ReasonRoleFailures {
			t.Errorf("terminal_reason = %s, want role_failures", compRes.TerminalReason)
		}
		if compRes.Report.CompletedRoleCount != 0 || compRes.Report.IncompleteRoleCount != 4 {
			t.Errorf("counts = %d/%d, want 0/4", compRes.Report.CompletedRoleCount, compRes.Report.IncompleteRoleCount)
		}
		if len(compRes.Report.Findings) != 0 {
			t.Errorf("failed session report has %d findings, want 0", len(compRes.Report.Findings))
		}
	})
}

// 5. Reason Precedence
func TestCompose_ReasonPrecedence(t *testing.T) {
	// Table of precedence tests:
	// Remaining reason precedence: process_restart -> deadline_cutoff -> role_failures
	// And user_cancelled outranks all remaining reasons when not all 4 are complete.
	t.Run("restart outranks cutoff and failures", func(t *testing.T) {
		store, _, t0, _ := setupComposeTestSession(t, "sess_prec_restart", "proj_prec")
		ctx := context.Background()

		// 1 in-flight role that gets recovered by restart sweep.
		rRes, err := store.ReservePendingRoleWithNow(ctx, "sess_prec_restart", t0)
		if err != nil || !rRes.Reserved {
			t.Fatalf("reserve: %v", err)
		}

		// Restart recovery sweep: claimed_at <= cutoff
		sweepRes, err := store.SweepRestartRecovery(ctx, t0.Add(1*time.Hour))
		if err != nil || !sweepRes.HasRecovered() || sweepRes.InFlightInterrupted != 1 {
			t.Fatalf("SweepRestartRecovery: %v (res=%+v)", err, sweepRes)
		}

		// Sweep remaining pending roles with cutoff.
		cutoffTime := t0.Add(35 * time.Minute)
		_, err = store.SweepCutoff(ctx, "sess_prec_restart", cutoffTime)
		if err != nil {
			t.Fatalf("SweepCutoff: %v", err)
		}

		compRes, err := store.ComposeSession(ctx, "sess_prec_restart")
		if err != nil {
			t.Fatalf("ComposeSession: %v", err)
		}

		if compRes.Status != domain.SessionFailed {
			t.Errorf("status = %s, want failed", compRes.Status)
		}
		if compRes.TerminalReason != domain.ReasonProcessRestart {
			t.Errorf("terminal_reason = %s, want process_restart", compRes.TerminalReason)
		}
	})

	t.Run("user_cancelled outranks process_restart", func(t *testing.T) {
		store, _, t0, _ := setupComposeTestSession(t, "sess_prec_cancel", "proj_prec")
		ctx := context.Background()

		// 1 in-flight role.
		rRes, err := store.ReservePendingRoleWithNow(ctx, "sess_prec_cancel", t0)
		if err != nil || !rRes.Reserved {
			t.Fatalf("reserve: %v", err)
		}

		// Request cancellation first.
		_, err = store.RequestCancellation(ctx, "sess_prec_cancel")
		if err != nil {
			t.Fatalf("RequestCancellation: %v", err)
		}

		// Cancellation sweep: pending roles become interrupted/user_cancelled.
		_, err = store.SweepCancellation(ctx, "sess_prec_cancel")
		if err != nil {
			t.Fatalf("SweepCancellation: %v", err)
		}

		// Restart recovery interrupts in-flight role with process_restart.
		_, err = store.SweepRestartRecovery(ctx, t0.Add(1*time.Hour))
		if err != nil {
			t.Fatalf("SweepRestartRecovery: %v", err)
		}

		compRes, err := store.ComposeSession(ctx, "sess_prec_cancel")
		if err != nil {
			t.Fatalf("ComposeSession: %v", err)
		}

		if compRes.Status != domain.SessionPartial {
			t.Errorf("status = %s, want partial", compRes.Status)
		}
		if compRes.TerminalReason != domain.ReasonUserCancelled {
			t.Errorf("terminal_reason = %s, want user_cancelled", compRes.TerminalReason)
		}
	})
}

// 6. Non-Terminal Refusal
func TestCompose_NonTerminalRefusal(t *testing.T) {
	t.Run("refuses queued session", func(t *testing.T) {
		store, _ := openGuardTestStore(t)
		ctx := context.Background()
		snap := createTestFrozenSnapshot(t, "snap_q_refuse")

		sessID := "sess_q_refuse"
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      sessID,
			ProjectID:      "proj_refuse",
			IdempotencyKey: "key_" + sessID,
			Title:          "Refusal Test",
			Content:        "Spec content",
			Snapshot:       snap,
			CreatedAt:      time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		_, err = store.ComposeSession(ctx, sessID)
		if err == nil {
			t.Fatalf("expected ComposeSession to fail for queued session")
		}
		if !errors.Is(err, ErrSessionNotReady) {
			t.Errorf("expected ErrSessionNotReady, got %v", err)
		}
		var notReadyErr *SessionNotReadyError
		if !errors.As(err, &notReadyErr) {
			t.Errorf("expected *SessionNotReadyError, got %T", err)
		}

		// Verify no writes made.
		st, err := store.ReadStatus(ctx, sessID)
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if st.Status != domain.SessionQueued {
			t.Errorf("status was modified to %s, want queued", st.Status)
		}
		if st.TerminalAt != nil {
			t.Errorf("terminal_at was set: %v", st.TerminalAt)
		}
	})

	t.Run("refuses reviewing session with pending and in-flight roles", func(t *testing.T) {
		store, _, t0, _ := setupComposeTestSession(t, "sess_rev_refuse", "proj_refuse")
		ctx := context.Background()

		// 1 role in_flight, 3 pending.
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_rev_refuse", t0)
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}

		_, err = store.ComposeSession(ctx, "sess_rev_refuse")
		if err == nil {
			t.Fatalf("expected ComposeSession to fail when roles are not terminal")
		}
		if !errors.Is(err, ErrSessionNotReady) {
			t.Errorf("expected ErrSessionNotReady, got %v", err)
		}
		var notReadyErr *SessionNotReadyError
		if !errors.As(err, &notReadyErr) {
			t.Fatalf("expected *SessionNotReadyError, got %T", err)
		}
		if len(notReadyErr.InFlightRoles) != 1 || notReadyErr.InFlightRoles[0] != domain.RoleRequirements {
			t.Errorf("in_flight roles = %v, want [requirements]", notReadyErr.InFlightRoles)
		}
		if len(notReadyErr.PendingRoles) != 3 {
			t.Errorf("pending roles count = %d, want 3", len(notReadyErr.PendingRoles))
		}

		// Verify session remains reviewing and no terminal writes made.
		st, err := store.ReadStatus(ctx, "sess_rev_refuse")
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if st.Status != domain.SessionReviewing {
			t.Errorf("status was modified to %s, want reviewing", st.Status)
		}
		if st.TerminalAt != nil {
			t.Errorf("terminal_at was modified: %v", st.TerminalAt)
		}
	})
}

// 7. Concurrent Composers with One Writer
func TestCompose_ConcurrentComposersWithOneWriter(t *testing.T) {
	store, _, t0, _ := setupComposeTestSession(t, "sess_conc_comp", "proj_conc")
	ctx := context.Background()

	// Complete all 4 roles.
	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_conc_comp", t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_conc_comp",
			RoleRunID:   res.RoleRunID,
			Role:        role,
			CallCount:   1,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	const concurrency = 8
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)

	var composedCount int32
	var alreadyTerminalCount int32
	results := make([]*ComposeResult, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-ready
			res, err := store.ComposeSession(ctx, "sess_conc_comp")
			results[idx] = res
			errs[idx] = err
			if err == nil {
				if res.IsComposed() {
					atomic.AddInt32(&composedCount, 1)
				}
				if res.IsAlreadyTerminal() {
					atomic.AddInt32(&alreadyTerminalCount, 1)
				}
			}
		}()
	}

	close(ready)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
	}

	if composedCount != 1 {
		t.Errorf("composedCount = %d, want exactly 1 winner", composedCount)
	}
	if alreadyTerminalCount != concurrency-1 {
		t.Errorf("alreadyTerminalCount = %d, want %d", alreadyTerminalCount, concurrency-1)
	}

	// Verify all returned identical terminal status and report bytes.
	var referenceBytes []byte
	for i, res := range results {
		if res.Status != domain.SessionComplete {
			t.Errorf("goroutine %d status = %s, want complete", i, res.Status)
		}
		data, err := json.Marshal(res.Report)
		if err != nil {
			t.Fatalf("goroutine %d report json marshal: %v", i, err)
		}
		if referenceBytes == nil {
			referenceBytes = data
		} else if !bytes.Equal(referenceBytes, data) {
			t.Errorf("goroutine %d report json differs from winner", i)
		}
	}
}

// 8. Repeated Composition (Idempotency)
func TestCompose_RepeatedComposition(t *testing.T) {
	store, _, t0, _ := setupComposeTestSession(t, "sess_repeat_comp", "proj_repeat")
	ctx := context.Background()

	// Complete all 4 roles.
	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_repeat_comp", t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_repeat_comp",
			RoleRunID:   res.RoleRunID,
			Role:        role,
			CallCount:   1,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// 1st call: performs composition.
	res1, err := store.ComposeSession(ctx, "sess_repeat_comp")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !res1.IsComposed() || res1.IsAlreadyTerminal() {
		t.Errorf("res1 composed=%v, alreadyTerminal=%v, want true/false", res1.IsComposed(), res1.IsAlreadyTerminal())
	}

	// 2nd call: idempotent read of terminal result.
	res2, err := store.ComposeSession(ctx, "sess_repeat_comp")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if res2.IsComposed() || !res2.IsAlreadyTerminal() {
		t.Errorf("res2 composed=%v, alreadyTerminal=%v, want false/true", res2.IsComposed(), res2.IsAlreadyTerminal())
	}

	// 3rd call: still idempotent.
	res3, err := store.ComposeSession(ctx, "sess_repeat_comp")
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if res3.IsComposed() || !res3.IsAlreadyTerminal() {
		t.Errorf("res3 composed=%v, alreadyTerminal=%v, want false/true", res3.IsComposed(), res3.IsAlreadyTerminal())
	}

	// Compare reports and terminal timestamps across calls.
	if !res1.TerminalAt.Equal(res2.TerminalAt) || !res2.TerminalAt.Equal(res3.TerminalAt) {
		t.Errorf("terminal_at changed between repeated calls: %v vs %v vs %v",
			res1.TerminalAt, res2.TerminalAt, res3.TerminalAt)
	}

	b1, _ := json.Marshal(res1.Report)
	b2, _ := json.Marshal(res2.Report)
	b3, _ := json.Marshal(res3.Report)
	if !bytes.Equal(b1, b2) || !bytes.Equal(b2, b3) {
		t.Errorf("report bytes changed across repeated composition calls")
	}
}

// 9. Report After Composition
func TestTerminalReport_ReportAfterComposition(t *testing.T) {
	store, _, t0, _ := setupComposeTestSession(t, "sess_rep_after", "proj_rep")
	ctx := context.Background()

	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_rep_after", t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_rep_after",
			RoleRunID:   res.RoleRunID,
			Role:        role,
			CallCount:   1,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	compRes, err := store.ComposeSession(ctx, "sess_rep_after")
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}

	// Read via read-only path ReadReport.
	readRep, err := store.ReadReport(ctx, "sess_rep_after")
	if err != nil {
		t.Fatalf("ReadReport failed after composition: %v", err)
	}

	// Read via ReadTerminalReport.
	termRep, err := store.ReadTerminalReport(ctx, "sess_rep_after")
	if err != nil {
		t.Fatalf("ReadTerminalReport failed after composition: %v", err)
	}

	// Compare reports.
	bComp, _ := json.Marshal(compRes.Report)
	bRead, _ := json.Marshal(readRep)
	bTerm, _ := json.Marshal(termRep)

	if !bytes.Equal(bComp, bRead) {
		t.Errorf("composed report bytes != readReport bytes:\n%s\nvs\n%s", string(bComp), string(bRead))
	}
	if !bytes.Equal(bRead, bTerm) {
		t.Errorf("readReport bytes != readTerminalReport bytes")
	}
}

// 10. Report Before Composition
func TestTerminalReport_ReportBeforeComposition(t *testing.T) {
	t.Run("queued session returns SessionNotTerminalError", func(t *testing.T) {
		store, _ := openGuardTestStore(t)
		ctx := context.Background()
		snap := createTestFrozenSnapshot(t, "snap_q_rep")

		sessID := "sess_q_rep"
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      sessID,
			ProjectID:      "proj_rep_before",
			IdempotencyKey: "key_" + sessID,
			Title:          "Report Gate Test",
			Content:        "Spec content",
			Snapshot:       snap,
			CreatedAt:      time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		_, err = store.ReadReport(ctx, sessID)
		if err == nil {
			t.Fatalf("expected ReadReport to fail on queued session")
		}
		if !errors.Is(err, ErrNotTerminal) || !errors.Is(err, ErrNotFinished) {
			t.Errorf("expected ErrNotTerminal/ErrNotFinished, got %v", err)
		}
		var notTermErr *SessionNotTerminalError
		if !errors.As(err, &notTermErr) {
			t.Errorf("expected *SessionNotTerminalError, got %T", err)
		}
		if notTermErr.Status != domain.SessionQueued {
			t.Errorf("status in error = %s, want queued", notTermErr.Status)
		}
	})

	t.Run("reviewing session returns SessionNotTerminalError", func(t *testing.T) {
		store, _, _, _ := setupComposeTestSession(t, "sess_rev_rep", "proj_rep_before")
		ctx := context.Background()

		_, err := store.ReadReport(ctx, "sess_rev_rep")
		if err == nil {
			t.Fatalf("expected ReadReport to fail on reviewing session")
		}
		if !errors.Is(err, ErrNotTerminal) {
			t.Errorf("expected ErrNotTerminal, got %v", err)
		}
		var notTermErr *SessionNotTerminalError
		if !errors.As(err, &notTermErr) {
			t.Errorf("expected *SessionNotTerminalError, got %T", err)
		}
		if notTermErr.Status != domain.SessionReviewing {
			t.Errorf("status in error = %s, want reviewing", notTermErr.Status)
		}
	})
}

// 11. Project Mismatch
func TestCompose_ProjectMismatch(t *testing.T) {
	store, _, t0, _ := setupComposeTestSession(t, "sess_proj_mismatch", "actual_proj")
	ctx := context.Background()

	// Complete all roles.
	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_proj_mismatch", t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_proj_mismatch",
			RoleRunID:   res.RoleRunID,
			Role:        role,
			CallCount:   1,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Attempt composition with mismatched project ID.
	_, err := store.ComposeSessionScoped(ctx, "different_proj", "sess_proj_mismatch")
	if err == nil {
		t.Fatalf("expected error for mismatched project ID")
	}
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	var notFoundErr *SessionNotFoundError
	if !errors.As(err, &notFoundErr) {
		t.Errorf("expected *SessionNotFoundError, got %T", err)
	}

	// Unknown session ID.
	_, err = store.ComposeSession(ctx, "non_existent_session")
	if err == nil {
		t.Fatalf("expected error for non-existent session ID")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// Verify session in database remains in reviewing status.
	st, err := store.ReadStatus(ctx, "sess_proj_mismatch")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.Status != domain.SessionReviewing {
		t.Errorf("status was modified to %s, want reviewing", st.Status)
	}
}

// 12. Rollback Entirely on Injected Failure or Malformed Data
func TestCompose_Rollback(t *testing.T) {
	t.Run("rollback on injected commit failure", func(t *testing.T) {
		store, _, t0, _ := setupComposeTestSession(t, "sess_rb_commit", "proj_rb")
		ctx := context.Background()

		for i, role := range domain.Roles {
			res, err := store.ReservePendingRoleWithNow(ctx, "sess_rb_commit", t0.Add(time.Duration(i)*time.Minute))
			if err != nil || !res.Reserved {
				t.Fatalf("reserve: %v", err)
			}
			_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
				SessionID:   "sess_rb_commit",
				RoleRunID:   res.RoleRunID,
				Role:        role,
				CallCount:   1,
				CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
			})
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
		}

		injectedErr := errors.New("simulated commit failure")
		composeBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
			return injectedErr
		}
		t.Cleanup(func() { composeBeforeCommitHook = nil })

		_, err := store.ComposeSession(ctx, "sess_rb_commit")
		if err == nil {
			t.Fatalf("expected ComposeSession to fail due to hook error")
		}
		if !errors.Is(err, injectedErr) {
			t.Errorf("expected injectedErr, got %v", err)
		}

		// Verify database state rolled back: session remains reviewing, terminal_at is null.
		st, err := store.ReadStatus(ctx, "sess_rb_commit")
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if st.Status != domain.SessionReviewing {
			t.Errorf("session status = %s, want reviewing", st.Status)
		}
		if st.TerminalAt != nil {
			t.Errorf("terminal_at = %v, want nil", st.TerminalAt)
		}
		if st.TerminalReason != "" {
			t.Errorf("terminal_reason = %q, want empty", st.TerminalReason)
		}
	})

	t.Run("rollback on malformed persisted role data", func(t *testing.T) {
		store, writer, t0, _ := setupComposeTestSession(t, "sess_rb_mal", "proj_rb")
		ctx := context.Background()

		for i, role := range domain.Roles {
			res, err := store.ReservePendingRoleWithNow(ctx, "sess_rb_mal", t0.Add(time.Duration(i)*time.Minute))
			if err != nil || !res.Reserved {
				t.Fatalf("reserve: %v", err)
			}
			_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
				SessionID:   "sess_rb_mal",
				RoleRunID:   res.RoleRunID,
				Role:        role,
				CallCount:   1,
				CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
			})
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
		}

		// Disable triggers temporarily or inject invalid enum into role_runs directly.
		// Since role_runs triggers prevent invalid transitions, let's corrupt cause to non-null on completed role.
		// Trigger allows: complete with cause IS NULL. So updating cause requires bypassing triggers.
		// Instead, we can delete one role run so len(roles) == 3, which is malformed persisted data.
		_, err := writer.ExecContext(ctx, `PRAGMA foreign_keys = OFF; DELETE FROM role_runs WHERE session_id = 'sess_rb_mal' AND role = 'security'; PRAGMA foreign_keys = ON;`)
		if err != nil {
			t.Fatalf("delete role run: %v", err)
		}

		_, err = store.ComposeSession(ctx, "sess_rb_mal")
		if err == nil {
			t.Fatalf("expected ComposeSession to fail on missing role")
		}
		if !errors.Is(err, ErrMalformedData) {
			t.Errorf("expected ErrMalformedData, got %v", err)
		}

		// Verify session remains reviewing and not partially terminalized.
		var currentStatus string
		var termAt sql.NullString
		err = writer.QueryRowContext(ctx, "SELECT status, terminal_at FROM sessions WHERE id = 'sess_rb_mal';").
			Scan(&currentStatus, &termAt)
		if err != nil {
			t.Fatalf("query status: %v", err)
		}
		if currentStatus != "reviewing" {
			t.Errorf("status = %s, want reviewing", currentStatus)
		}
		if termAt.Valid {
			t.Errorf("terminal_at = %v, want NULL", termAt.String)
		}
	})
}

// 13. Exact Unchanged Row Snapshots
func TestCompose_ExactUnchangedRowSnapshots(t *testing.T) {
	store, writer, t0, _ := setupComposeTestSession(t, "sess_unchanged", "proj_unchanged")
	ctx := context.Background()

	// Complete roles with findings and basis refs.
	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_unchanged", t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}
		var findings []domain.Finding
		if role == domain.RoleRequirements {
			findings = []domain.Finding{
				{
					ID:             "find-req-1",
					Kind:           domain.FindingExisting,
					Severity:       domain.SeverityHigh,
					Category:       "correctness",
					Issue:          "Issue description",
					Recommendation: "Fix recommendation",
					BasisRefs:      []string{"req-1"},
				},
			}
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_unchanged",
			RoleRunID:   res.RoleRunID,
			Role:        role,
			Findings:    findings,
			CallCount:   1,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Capture exact row snapshots before composition.
	type sessionStatic struct {
		id               string
		projectID        string
		idempotencyKey   string
		requestHash      string
		snapshotID       string
		cancelRequested  int
		dispatchCutoffAt sql.NullString
		hardDeadlineAt   sql.NullString
		createdAt        string
		claimedAt        sql.NullString
	}
	var sessBefore sessionStatic
	err := writer.QueryRowContext(ctx, `SELECT
		id, project_id, idempotency_key, request_hash, snapshot_id, cancel_requested,
		dispatch_cutoff_at, hard_deadline_at, created_at, claimed_at
	FROM sessions WHERE id = 'sess_unchanged';`).Scan(
		&sessBefore.id, &sessBefore.projectID, &sessBefore.idempotencyKey, &sessBefore.requestHash,
		&sessBefore.snapshotID, &sessBefore.cancelRequested, &sessBefore.dispatchCutoffAt,
		&sessBefore.hardDeadlineAt, &sessBefore.createdAt, &sessBefore.claimedAt,
	)
	if err != nil {
		t.Fatalf("query sessBefore: %v", err)
	}

	// Dump role_runs rows.
	dumpRoles := func() map[string]string {
		rows, err := writer.QueryContext(ctx, `SELECT
			id, session_id, role, status, cause, error_category, error_message, call_count, started_at, completed_at, created_at
		FROM role_runs WHERE session_id = 'sess_unchanged' ORDER BY role;`)
		if err != nil {
			t.Fatalf("dump roles: %v", err)
		}
		defer rows.Close()
		m := make(map[string]string)
		for rows.Next() {
			var id, sessID, role, status, creatStr string
			var cause, errCat, errMsg, startStr, compStr sql.NullString
			var callCnt int
			if err := rows.Scan(&id, &sessID, &role, &status, &cause, &errCat, &errMsg, &callCnt, &startStr, &compStr, &creatStr); err != nil {
				t.Fatalf("scan dump roles: %v", err)
			}
			m[role] = fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d|%s|%s|%s",
				id, sessID, role, status, cause.String, errCat.String, errMsg.String, callCnt, startStr.String, compStr.String, creatStr)
		}
		return m
	}
	rolesBefore := dumpRoles()

	// Dump findings and basis refs.
	dumpFindings := func() map[string]string {
		rows, err := writer.QueryContext(ctx, `SELECT
			f.id, f.role_run_id, f.finding_id, f.severity, f.category, f.issue, f.recommendation, f.created_at
		FROM findings f
		JOIN role_runs rr ON f.role_run_id = rr.id
		WHERE rr.session_id = 'sess_unchanged' ORDER BY f.finding_id;`)
		if err != nil {
			t.Fatalf("dump findings: %v", err)
		}
		defer rows.Close()
		m := make(map[string]string)
		for rows.Next() {
			var id, rrID, fID, sev, cat, iss, rec, creat string
			if err := rows.Scan(&id, &rrID, &fID, &sev, &cat, &iss, &rec, &creat); err != nil {
				t.Fatalf("scan dump findings: %v", err)
			}
			m[fID] = fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s", id, rrID, fID, sev, cat, iss, rec, creat)
		}
		return m
	}
	findingsBefore := dumpFindings()

	// Execute composition.
	compRes, err := store.ComposeSession(ctx, "sess_unchanged")
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}
	if !compRes.IsComposed() {
		t.Fatalf("expected IsComposed() true")
	}

	// Verify static session columns.
	var sessAfter sessionStatic
	err = writer.QueryRowContext(ctx, `SELECT
		id, project_id, idempotency_key, request_hash, snapshot_id, cancel_requested,
		dispatch_cutoff_at, hard_deadline_at, created_at, claimed_at
	FROM sessions WHERE id = 'sess_unchanged';`).Scan(
		&sessAfter.id, &sessAfter.projectID, &sessAfter.idempotencyKey, &sessAfter.requestHash,
		&sessAfter.snapshotID, &sessAfter.cancelRequested, &sessAfter.dispatchCutoffAt,
		&sessAfter.hardDeadlineAt, &sessAfter.createdAt, &sessAfter.claimedAt,
	)
	if err != nil {
		t.Fatalf("query sessAfter: %v", err)
	}
	if sessBefore != sessAfter {
		t.Errorf("static session fields changed:\nbefore: %+v\nafter:  %+v", sessBefore, sessAfter)
	}

	// Verify role_runs rows unchanged.
	rolesAfter := dumpRoles()
	for role, bStr := range rolesBefore {
		if aStr, ok := rolesAfter[role]; !ok || aStr != bStr {
			t.Errorf("role %s changed:\nbefore: %s\nafter:  %s", role, bStr, aStr)
		}
	}

	// Verify findings rows unchanged.
	findingsAfter := dumpFindings()
	for fID, bStr := range findingsBefore {
		if aStr, ok := findingsAfter[fID]; !ok || aStr != bStr {
			t.Errorf("finding %s changed:\nbefore: %s\nafter:  %s", fID, bStr, aStr)
		}
	}
}

// 14. Deterministic Report Bytes
func TestTerminalReport_DeterministicReportBytes(t *testing.T) {
	// Build two sessions with the same findings but inserted in different orders.
	buildSession := func(sessID string, reverseInsert bool) []byte {
		store, _, t0, _ := setupComposeTestSession(t, sessID, "proj_det")
		ctx := context.Background()

		findingsByRole := map[domain.Role][]domain.Finding{
			domain.RoleRequirements: {
				{
					ID:             "find-req-1",
					Kind:           domain.FindingExisting,
					Severity:       domain.SeverityHigh,
					Category:       "correctness",
					Issue:          "Issue 1",
					Recommendation: "Rec 1",
					BasisRefs:      []string{"req-1"},
				},
				{
					ID:             "find-req-2",
					Kind:           domain.FindingExisting,
					Severity:       domain.SeverityCritical,
					Category:       "security",
					Issue:          "Issue 2",
					Recommendation: "Rec 2",
					BasisRefs:      []string{"brief-1"},
				},
			},
			domain.RoleArchitecture: {
				{
					ID:             "find-arch-1",
					Kind:           domain.FindingExisting,
					Severity:       domain.SeverityHigh,
					Category:       "perf",
					Issue:          "Issue 3",
					Recommendation: "Rec 3",
					BasisRefs:      []string{"arch-1"},
				},
			},
		}

		if reverseInsert {
			// Reserve two roles concurrently to test out-of-order completion.
			r1, err := store.ReservePendingRoleWithNow(ctx, sessID, t0)
			if err != nil || !r1.Reserved {
				t.Fatalf("reserve 1: %v", err)
			}
			r2, err := store.ReservePendingRoleWithNow(ctx, sessID, t0.Add(1*time.Minute))
			if err != nil || !r2.Reserved {
				t.Fatalf("reserve 2: %v", err)
			}

			// Publish second role (Architecture) before first role (Requirements).
			flist2 := findingsByRole[r2.Role]
			_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
				SessionID:   sessID,
				RoleRunID:   r2.RoleRunID,
				Role:        r2.Role,
				Findings:    flist2,
				CallCount:   1,
				CompletedAt: t0.Add(2 * time.Minute),
			})
			if err != nil {
				t.Fatalf("publish r2: %v", err)
			}

			// Publish first role (Requirements) with reversed findings list.
			flist1 := findingsByRole[r1.Role]
			if len(flist1) > 1 {
				flist1 = []domain.Finding{flist1[1], flist1[0]}
			}
			_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
				SessionID:   sessID,
				RoleRunID:   r1.RoleRunID,
				Role:        r1.Role,
				Findings:    flist1,
				CallCount:   1,
				CompletedAt: t0.Add(3 * time.Minute),
			})
			if err != nil {
				t.Fatalf("publish r1: %v", err)
			}

			// Complete remaining 2 roles (QA, Security).
			for i := 2; i < 4; i++ {
				r, err := store.ReservePendingRoleWithNow(ctx, sessID, t0.Add(time.Duration(i+2)*time.Minute))
				if err != nil || !r.Reserved {
					t.Fatalf("reserve %d: %v", i, err)
				}
				_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
					SessionID:   sessID,
					RoleRunID:   r.RoleRunID,
					Role:        r.Role,
					Findings:    findingsByRole[r.Role],
					CallCount:   1,
					CompletedAt: t0.Add(time.Duration(i+3) * time.Minute),
				})
				if err != nil {
					t.Fatalf("publish %d: %v", i, err)
				}
			}
		} else {
			for i := 0; i < domain.RoleCount; i++ {
				res, err := store.ReservePendingRoleWithNow(ctx, sessID, t0.Add(time.Duration(i)*time.Minute))
				if err != nil || !res.Reserved {
					t.Fatalf("reserve: %v", err)
				}
				role := res.Role
				flist := findingsByRole[role]
				_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
					SessionID:   sessID,
					RoleRunID:   res.RoleRunID,
					Role:        role,
					Findings:    flist,
					CallCount:   1,
					CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
				})
				if err != nil {
					t.Fatalf("publish: %v", err)
				}
			}
		}

		fixedTime := time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC)
		res, err := store.ComposeSessionWithParams(ctx, ComposeParams{
			SessionID: sessID,
			Now:       fixedTime,
		})
		if err != nil {
			t.Fatalf("ComposeSession: %v", err)
		}

		readRep, err := store.ReadReport(ctx, sessID)
		if err != nil {
			t.Fatalf("ReadReport: %v", err)
		}

		// Verify ComposeResult.Report and ReadReport produce exact same JSON bytes.
		bComp, _ := json.Marshal(res.Report)
		bRead, _ := json.Marshal(readRep)
		if !bytes.Equal(bComp, bRead) {
			t.Fatalf("composed report != read report")
		}

		// Re-marshal with normalized session ID and snapshot ID for cross-session comparison.
		norm := *readRep
		norm.SessionID = "normalized_session_id"
		norm.SnapshotID = "normalized_snapshot_id"
		normBytes, err := json.Marshal(norm)
		if err != nil {
			t.Fatalf("marshal norm: %v", err)
		}
		return normBytes
	}

	bytes1 := buildSession("sess_det_1", false)
	bytes2 := buildSession("sess_det_2", true)

	if !bytes.Equal(bytes1, bytes2) {
		t.Errorf("deterministic report bytes mismatch:\nsess1:\n%s\nsess2:\n%s", string(bytes1), string(bytes2))
	}
}

// 15. Forbidden Scope and API Aliases
func TestCompose_ForbiddenScope(t *testing.T) {
	ctx := context.Background()

	// Nil store.
	var nilStore *Store
	_, err := nilStore.ComposeSession(ctx, "any")
	if err == nil {
		t.Errorf("expected error on nil store")
	}
	_, err = ComposeSession(ctx, nil, "any")
	if err == nil {
		t.Errorf("expected error on nil store (package function)")
	}

	// Cancelled context.
	store, _ := openGuardTestStore(t)
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.ComposeSession(cancelledCtx, "any")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	// Empty session ID.
	_, err = store.ComposeSession(ctx, "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for empty session ID, got %v", err)
	}

	// Package-level aliases on a valid session.
	store2, _, t0, _ := setupComposeTestSession(t, "sess_aliases", "proj_aliases")
	for i, role := range domain.Roles {
		res, err := store2.ReservePendingRoleWithNow(ctx, "sess_aliases", t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve: %v", err)
		}
		_, err = store2.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   "sess_aliases",
			RoleRunID:   res.RoleRunID,
			Role:        role,
			CallCount:   1,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Finalize alias.
	res, err := store2.FinalizeSession(ctx, "sess_aliases")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if !res.IsComposed() {
		t.Errorf("FinalizeSession IsComposed() = false, want true")
	}

	// Finalize package function on already terminal session -> idempotent.
	res2, err := Finalize(ctx, store2, "sess_aliases")
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !res2.IsAlreadyFinalized() {
		t.Errorf("IsAlreadyFinalized() = false, want true")
	}
}
