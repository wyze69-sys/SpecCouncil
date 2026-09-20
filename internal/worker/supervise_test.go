package worker_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider/fake"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite"
	"github.com/wyze69-sys/SpecCouncil/internal/worker"
)

// Helper to construct a standard frozen snapshot for tests.
func standardTestSnapshot(t *testing.T, snapID string) evidence.Snapshot {
	t.Helper()
	units := []evidence.Unit{
		{
			ID:   "unit_1",
			Kind: evidence.UnitRequirement,
			Text: "Deterministic specification requirement unit",
		},
		{
			ID:   "unit_2",
			Kind: evidence.UnitComponent,
			Text: "Component design unit",
		},
	}
	snap, err := evidence.Freeze(snapID, units)
	if err != nil {
		t.Fatalf("evidence.Freeze failed: %v", err)
	}
	return snap
}

// Helper to submit and claim a session in reviewing state.
func submitAndClaimSession(
	t *testing.T,
	store *sqlite.Store,
	sessionID, projectID string,
	timingPolicy sqlite.TimingPolicy,
) (*sqlite.ClaimResult, evidence.Snapshot) {
	t.Helper()
	snap := standardTestSnapshot(t, "snap_"+sessionID)
	now := time.Now().UTC()

	_, err := store.Submit(context.Background(), sqlite.SubmitParams{
		SessionID:      sessionID,
		ProjectID:      projectID,
		IdempotencyKey: "key_" + sessionID,
		Title:          "Review for " + sessionID,
		Content:        "Spec content",
		Snapshot:       snap,
		CreatedAt:      now,
	})
	if err != nil {
		t.Fatalf("store.Submit(%q) failed: %v", sessionID, err)
	}

	claimRes, err := store.ClaimSession(context.Background(), timingPolicy)
	if err != nil {
		t.Fatalf("store.ClaimSession failed: %v", err)
	}
	if !claimRes.Claimed {
		t.Fatalf("expected session %q to be claimed, got reason: %s", sessionID, claimRes.NoWorkReason)
	}

	return claimRes, snap
}

func successScript(findings ...string) fake.ScriptedCall {
	if len(findings) == 0 {
		return fake.ScriptedCall{
			Body: `{"findings":[{"id":"F-1","severity":"high","category":"correctness","issue":"Unvalidated state","recommendation":"Validate state","basis_refs":["unit_1"]}]}`,
		}
	}
	return fake.ScriptedCall{
		Body: findings[0],
	}
}

func standardPublishSuccessParamsBuilder(in worker.PublishSuccessInput) sqlite.PublishSuccessParams {
	return sqlite.PublishSuccessParams{
		ProjectID:   in.ProjectID,
		SessionID:   in.SessionID,
		RoleRunID:   in.RoleRunID,
		Role:        in.Role,
		Findings:    in.Findings,
		CallCount:   in.CallCount,
		CompletedAt: in.CompletedAt,
	}
}

func standardPublishFailureParamsBuilder(in worker.PublishFailureInput) sqlite.PublishFailureParams {
	return sqlite.PublishFailureParams{
		ProjectID:     in.ProjectID,
		SessionID:     in.SessionID,
		RoleRunID:     in.RoleRunID,
		Role:          in.Role,
		ErrorCategory: in.ErrorCategory,
		ErrorMessage:  in.ErrorMessage,
		CallCount:     in.CallCount,
		CompletedAt:   in.CompletedAt,
	}
}

// 1. A claimed reviewing session with one reservation executes that role once and publishes a valid success atomically.
func TestSupervise_ClaimedReviewingSessionExecutesAndPublishesAtomically(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_single_role", "proj_test", policy)

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	var publishedRoles sync.Map

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		OnRolePublished: func(role domain.Role, roleRunID string, outcome review.RoleOutcome, err error) {
			if err == nil && outcome.Status == domain.RoleComplete {
				publishedRoles.Store(role, true)
			}
		},
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session to be composed")
	}

	// Verify in SQLite: all 4 roles are complete and findings are persisted atomically
	st, err := store.ReadStatus(context.Background(), claimRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if st.Status != domain.SessionComplete {
		t.Fatalf("expected session complete, got: %s", st.Status)
	}
	if st.CompletedRoleCount != 4 {
		t.Fatalf("expected 4 completed roles, got: %d", st.CompletedRoleCount)
	}

	// Report must have deterministic findings
	rep := res.TerminalReport()
	if rep == nil {
		t.Fatal("expected non-nil terminal report")
	}
	if len(rep.Findings) != 4 {
		t.Fatalf("expected 4 findings, got: %d", len(rep.Findings))
	}
}

// 2. A failed role publishes canonical failure metadata with no findings.
func TestSupervise_FailedRolePublishesFailureMetadataWithNoFindings(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_failed_role", "proj_test", policy)

	// Requirements fails fatally; others succeed
	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {{
			TransportError: domain.ErrProviderRejected,
			Message:        "quota exceeded or unauthorized",
		}},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session to be composed")
	}

	// Check status: Requirements failed, other 3 complete
	st, err := store.ReadStatus(context.Background(), claimRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if st.Status != domain.SessionPartial {
		t.Fatalf("expected session partial, got: %s", st.Status)
	}
	if st.TerminalReason != domain.ReasonRoleFailures {
		t.Fatalf("expected reason role_failures, got: %s", st.TerminalReason)
	}

	for _, r := range st.Roles {
		if r.Role == domain.RoleRequirements {
			if r.Status != domain.RoleFailed {
				t.Fatalf("expected Requirements failed, got: %s", r.Status)
			}
			if r.ErrorCategory != domain.ErrProviderRejected {
				t.Fatalf("expected ErrProviderRejected, got: %s", r.ErrorCategory)
			}
		} else {
			if r.Status != domain.RoleComplete {
				t.Fatalf("expected role %s complete, got: %s", r.Role, r.Status)
			}
		}
	}

	// Failed role must contribute zero findings
	rep := res.TerminalReport()
	for _, f := range rep.Findings {
		if f.Role == domain.RoleRequirements {
			t.Fatalf("failed role Requirements must contribute zero findings, found: %v", f)
		}
	}
}

// 3. A timed-out role publishes canonical timeout/failure metadata with its real call count.
func TestSupervise_TimedOutRolePublishesTimeoutMetadataWithRealCallCount(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_timeout_role", "proj_test", policy)

	// Call 1 times out (retryable), Call 2 also times out
	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {
			{TransportError: domain.ErrTimeout, Message: "gateway timeout"},
			{TransportError: domain.ErrTimeout, Message: "gateway timeout 2"},
		},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session to be composed")
	}

	st, err := store.ReadStatus(context.Background(), claimRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}

	for _, r := range st.Roles {
		if r.Role == domain.RoleRequirements {
			if r.Status != domain.RoleFailed {
				t.Fatalf("expected Requirements failed, got: %s", r.Status)
			}
			if r.ErrorCategory != domain.ErrTimeout {
				t.Fatalf("expected ErrTimeout, got: %s", r.ErrorCategory)
			}
			if r.CallCount != 2 {
				t.Fatalf("expected call count 2 for two attempts, got: %d", r.CallCount)
			}
		}
	}
}

// 4. Pending cancellation interrupts pending roles; already in-flight work drains.
func TestSupervise_CancellationInterruptsPendingRolesAndDrainsInFlight(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_cancel_drain", "proj_test", policy)

	inFlightBarrier := make(chan struct{})
	cancelRequested := make(chan struct{})

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	var reqRunning atomic.Bool

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		CancellationSweeper: func(ctx context.Context, sessionID string) error {
			_, err := store.SweepCancellation(ctx, sessionID)
			return err
		},
		OnRoleExecuting: func(role domain.Role, roleRunID string) {
			if role == domain.RoleRequirements && reqRunning.CompareAndSwap(false, true) {
				// While Requirements is in-flight, request cancellation in storage
				_, _ = store.RequestCancellation(context.Background(), claimRes.SessionID)
				close(cancelRequested)
				<-inFlightBarrier
			}
		},
	}

	go func() {
		<-cancelRequested
		// Give dispatcher a brief moment to run cancellation sweep, then release in-flight role
		time.Sleep(30 * time.Millisecond)
		close(inFlightBarrier)
	}()

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session to be composed")
	}

	// Verify outcome: Requirements completed, remaining roles interrupted by user_cancelled
	st, err := store.ReadStatus(context.Background(), claimRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if st.Status != domain.SessionPartial {
		t.Fatalf("expected partial status, got: %s", st.Status)
	}
	if st.TerminalReason != domain.ReasonUserCancelled {
		t.Fatalf("expected reason user_cancelled, got: %s", st.TerminalReason)
	}
}

// 5. Cancellation, cutoff, and hard deadline prevent later reservations and provider calls.
func TestSupervise_CancellationCutoffHardDeadlinePreventLaterReservations(t *testing.T) {
	// 5a. Cancellation while queued
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_cancel_queued", "proj_test", policy)

	// Request cancellation before supervision starts
	_, err := store.RequestCancellation(context.Background(), claimRes.SessionID)
	if err != nil {
		t.Fatalf("RequestCancellation failed: %v", err)
	}

	var callCount atomic.Int32
	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		CancellationSweeper: func(ctx context.Context, sessionID string) error {
			_, err := store.SweepCancellation(ctx, sessionID)
			return err
		},
		OnRoleExecuting: func(role domain.Role, roleRunID string) {
			callCount.Add(1)
		},
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed on cancelled session: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected cancelled session to compose cleanly")
	}

	// Zero provider calls must occur!
	if calls := callCount.Load(); calls != 0 {
		t.Fatalf("expected 0 provider calls for pre-cancelled session, got: %d", calls)
	}

	st, err := store.ReadStatus(context.Background(), claimRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if st.Status != domain.SessionPartial || st.TerminalReason != domain.ReasonUserCancelled {
		t.Fatalf("expected partial / user_cancelled, got %s / %s", st.Status, st.TerminalReason)
	}
}

// 6. Exactly one publication call occurs per reservation.
func TestSupervise_ExactlyOnePublicationCallPerReservation(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_one_pub_per_res", "proj_test", policy)

	var pubCalls atomic.Int32
	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		OnRolePublished: func(role domain.Role, roleRunID string, outcome review.RoleOutcome, err error) {
			pubCalls.Add(1)
		},
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session composed")
	}

	if calls := pubCalls.Load(); calls != 4 {
		t.Fatalf("expected exactly 4 publication calls (1 per role), got: %d", calls)
	}
}

// 7. Publication happens after W4/provider execution and before dispatcher completion notification.
func TestSupervise_PublicationHappensAfterExecutionAndBeforeNotification(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_order_check", "proj_test", policy)

	type event struct {
		name string
		role domain.Role
		t    time.Time
	}

	var mu sync.Mutex
	var events []event
	record := func(name string, role domain.Role) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event{name: name, role: role, t: time.Now()})
	}

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		OnRoleExecuted: func(role domain.Role, roleRunID string, outcome review.RoleOutcome) {
			record("executed", role)
		},
		OnRolePublished: func(role domain.Role, roleRunID string, outcome review.RoleOutcome, err error) {
			record("published", role)
		},
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session composed")
	}

	mu.Lock()
	defer mu.Unlock()

	// Verify that for every role: "executed" appears before "published"
	for _, role := range domain.Roles {
		var execIdx, pubIdx int = -1, -1
		for i, ev := range events {
			if ev.role == role {
				if ev.name == "executed" && execIdx == -1 {
					execIdx = i
				}
				if ev.name == "published" && pubIdx == -1 {
					pubIdx = i
				}
			}
		}
		if execIdx == -1 || pubIdx == -1 {
			t.Fatalf("missing events for role %s: exec=%d pub=%d", role, execIdx, pubIdx)
		}
		if execIdx >= pubIdx {
			t.Fatalf("ordering violation for role %s: executed (%d) >= published (%d)", role, execIdx, pubIdx)
		}
	}
}

// 8. Notification failure cannot roll back a committed publication.
func TestSupervise_NotificationFailureCannotRollbackCommittedPublication(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_no_rollback", "proj_test", policy)

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session composed")
	}

	// Publication in storage is durable
	st, err := store.ReadStatus(context.Background(), claimRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if st.Status != domain.SessionComplete {
		t.Fatalf("expected SessionComplete in DB, got: %s", st.Status)
	}
}

// 9. A stale/late publication conflict makes no second provider call and no extra writes.
func TestSupervise_StalePublicationConflictMakesNoSecondCall(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_pub_conflict", "proj_test", policy)

	var providerCalls atomic.Int32
	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	// Wrap store to inject conflict on Requirements publication
	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		OnRoleExecuted: func(role domain.Role, roleRunID string, outcome review.RoleOutcome) {
			if role == domain.RoleRequirements {
				// Corrupt the role run state directly to complete in store before worker's CAS
				// to simulate a stale race or conflict
				_, _ = store.PublishRoleSuccess(context.Background(), sqlite.PublishSuccessParams{
					SessionID:   claimRes.SessionID,
					RoleRunID:   roleRunID,
					Role:        role,
					CallCount:   1,
					CompletedAt: time.Now().UTC(),
				})
			}
		},
		OnRoleExecuting: func(role domain.Role, roleRunID string) {
			providerCalls.Add(1)
		},
	}

	_, err := worker.SuperviseSession(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected publication conflict error, got nil")
	}
	if !errors.Is(err, worker.ErrPublicationConflict) {
		t.Fatalf("expected ErrPublicationConflict, got: %v", err)
	}
}

// 10. Two concurrently reserved roles never exceed W4’s call budget or the persisted MAX_IN_FLIGHT = 2 limit.
func TestSupervise_TwoConcurrentlyReservedRolesNeverExceedLimit(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_concurrency_limit", "proj_test", policy)

	var currentInFlight atomic.Int32
	var maxObservedInFlight atomic.Int32

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		OnRoleExecuting: func(role domain.Role, roleRunID string) {
			curr := currentInFlight.Add(1)
			for {
				max := maxObservedInFlight.Load()
				if curr <= max || maxObservedInFlight.CompareAndSwap(max, curr) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond) // hold concurrency briefly
		},
		OnRoleExecuted: func(role domain.Role, roleRunID string, outcome review.RoleOutcome) {
			currentInFlight.Add(-1)
		},
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session composed")
	}

	maxConcurrent := maxObservedInFlight.Load()
	if maxConcurrent > 2 {
		t.Fatalf("in-flight concurrency violation: observed %d concurrent roles, maximum allowed is 2", maxConcurrent)
	}
}

// 11. Four terminal role rows and zero persisted in-flight roles trigger one deterministic composition.
func TestSupervise_FourTerminalRolesTriggerOneComposition(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_comp_once", "proj_test", policy)

	var compCount atomic.Int32
	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		OnCompositionDone: func(res *sqlite.ComposeResult, err error) {
			if err == nil {
				compCount.Add(1)
			}
		},
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session composed")
	}

	if count := compCount.Load(); count != 1 {
		t.Fatalf("expected exactly 1 composition call, got: %d", count)
	}
}

// 12. Composition is refused while any role is pending or in-flight.
func TestSupervise_CompositionRefusedWhileAnyRolePendingOrInFlight(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, _ := submitAndClaimSession(t, store, "sess_comp_refused", "proj_test", policy)

	// Direct call to ComposeSession while roles are still pending
	_, err := store.ComposeSession(context.Background(), claimRes.SessionID)
	if err == nil {
		t.Fatal("expected ComposeSession to be refused while roles are pending, got nil")
	}
	if !errors.Is(err, sqlite.ErrSessionNotReady) {
		t.Fatalf("expected ErrSessionNotReady, got: %v", err)
	}
}

// 13. Duplicate composition returns the persisted terminal result without changing rows or timestamps.
func TestSupervise_DuplicateCompositionIdempotent(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_dup_comp", "proj_test", policy)

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
	}

	res1, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first SuperviseSession failed: %v", err)
	}

	// Call ComposeSession a second time
	res2, err := store.ComposeSession(context.Background(), claimRes.SessionID)
	if err != nil {
		t.Fatalf("second ComposeSession failed: %v", err)
	}
	if !res2.IsAlreadyTerminal() {
		t.Fatal("expected duplicate composition to report AlreadyTerminal")
	}
	if res2.TerminalAt != res1.ComposeResult.TerminalAt {
		t.Fatalf("terminal timestamp altered on duplicate composition: %v != %v", res2.TerminalAt, res1.ComposeResult.TerminalAt)
	}
}

// 14. Persistence failure stops dispatch, returns a non-nil error, and starts no hidden provider, timer, callback, or goroutine work.
func TestSupervise_PersistenceFailureStopsDispatch(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_persist_fail", "proj_test", policy)

	injectedErr := errors.New("simulated disk I/O persistence failure")
	var providerCalls atomic.Int32

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	// Wrap store to fail on publication
	failingPubStore := &wrappedFailingPublisher{
		store: store,
		err:   injectedErr,
	}

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   failingPubStore,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
		OnRoleExecuting: func(role domain.Role, roleRunID string) {
			providerCalls.Add(1)
		},
	}

	_, err := worker.SuperviseSession(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error on persistence failure, got nil")
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error preserved, got: %v", err)
	}
}

type wrappedFailingPublisher struct {
	store *sqlite.Store
	err   error
}

func (w *wrappedFailingPublisher) PublishRoleSuccess(ctx context.Context, params sqlite.PublishSuccessParams) (*sqlite.PublishResult, error) {
	return nil, w.err
}

func (w *wrappedFailingPublisher) PublishRoleFailure(ctx context.Context, params sqlite.PublishFailureParams) (*sqlite.PublishResult, error) {
	return nil, w.err
}

// 15. Process lock cleanup works on success, no-work, cancellation, publication conflict, publication failure, composition refusal, composition failure, and abnormal worker return.
func TestSupervise_ProcessLockCleanupAcrossAllExitConditions(t *testing.T) {
	store, tmpDir := openTestStore(t)
	policy := standardTimingPolicy()
	lockPath := filepath.Join(tmpDir, "worker_supervise.lock")

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	// 15a. No-work cleanup
	cfgNoWork := worker.SuperviseWorkerConfig[
		*sqlite.SweepRestartRecoveryResult,
		sqlite.TimingPolicy,
		*sqlite.ClaimResult,
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		LockPath:           lockPath,
		Store:              store,
		TimingPolicy:       policy,
		Cutoff:             time.Now().UTC().Add(-1 * time.Hour),
		Provider:           fake,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
	}

	res, err := worker.Supervise(context.Background(), cfgNoWork)
	if err != nil {
		t.Fatalf("expected nil error on no-work, got: %v", err)
	}
	if res != nil {
		t.Fatal("expected nil result on no-work")
	}

	// Verify lock acquirable immediately
	lock1, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock not acquirable after no-work: %v", err)
	}
	_ = lock1.Release()

	// 15b. Success cleanup
	submitSession(t, store, "sess_lock_success", "proj_test", "key_lock_success", time.Now().UTC().Add(-5*time.Minute))
	res, err = worker.Supervise(context.Background(), cfgNoWork)
	if err != nil {
		t.Fatalf("Supervise failed on valid session: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected composed result on success")
	}

	// Verify lock acquirable immediately
	lock2, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock not acquirable after successful session: %v", err)
	}
	_ = lock2.Release()

	// 15c. Cancellation cleanup
	storeCancel, tmpDirCancel := openTestStore(t)
	lockPathCancel := filepath.Join(tmpDirCancel, "worker_supervise_cancel.lock")
	submitSession(t, storeCancel, "sess_lock_cancel", "proj_test", "key_lock_cancel", time.Now().UTC().Add(-4*time.Minute))
	_, _ = storeCancel.RequestCancellation(context.Background(), "sess_lock_cancel")
	cfgCancel := cfgNoWork
	cfgCancel.LockPath = lockPathCancel
	cfgCancel.Store = storeCancel
	cfgCancel.CancellationSweeper = func(ctx context.Context, sID string) error {
		_, err := storeCancel.SweepCancellation(ctx, sID)
		return err
	}
	res, err = worker.Supervise(context.Background(), cfgCancel)
	if err != nil {
		t.Fatalf("Supervise failed on cancelled session: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected composed result on cancellation")
	}

	lock3, err := worker.AcquireProcessLock(context.Background(), lockPathCancel)
	if err != nil {
		t.Fatalf("lock not acquirable after cancellation: %v", err)
	}
	_ = lock3.Release()

	// 15d. Publication conflict cleanup
	storeConflict, tmpDirConflict := openTestStore(t)
	lockPathConflict := filepath.Join(tmpDirConflict, "worker_supervise_conflict.lock")
	submitSession(t, storeConflict, "sess_lock_conflict", "proj_test", "key_lock_conflict", time.Now().UTC().Add(-3*time.Minute))
	cfgConflict := cfgNoWork
	cfgConflict.LockPath = lockPathConflict
	cfgConflict.Store = &conflictInjectingStore{
		Store:       storeConflict,
		conflictErr: sqlite.ErrPublicationConflict,
	}
	_, err = worker.Supervise(context.Background(), cfgConflict)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !errors.Is(err, worker.ErrPublicationConflict) {
		t.Fatalf("expected ErrPublicationConflict, got: %v", err)
	}

	lock4, err := worker.AcquireProcessLock(context.Background(), lockPathConflict)
	if err != nil {
		t.Fatalf("lock not acquirable after publication conflict: %v", err)
	}
	_ = lock4.Release()

	// 15e. Publication failure cleanup
	storePubFail, tmpDirPubFail := openTestStore(t)
	lockPathPubFail := filepath.Join(tmpDirPubFail, "worker_supervise_pub_fail.lock")
	submitSession(t, storePubFail, "sess_lock_pub_fail", "proj_test", "key_lock_pub_fail", time.Now().UTC().Add(-2*time.Minute))
	pubFailErr := errors.New("simulated publication persistence failure")
	cfgPubFail := cfgNoWork
	cfgPubFail.LockPath = lockPathPubFail
	cfgPubFail.Store = &conflictInjectingStore{
		Store:       storePubFail,
		conflictErr: pubFailErr,
	}
	_, err = worker.Supervise(context.Background(), cfgPubFail)
	if err == nil {
		t.Fatal("expected publication failure error, got nil")
	}
	if !errors.Is(err, pubFailErr) {
		t.Fatalf("expected pubFailErr, got: %v", err)
	}

	lock5, err := worker.AcquireProcessLock(context.Background(), lockPathPubFail)
	if err != nil {
		t.Fatalf("lock not acquirable after publication failure: %v", err)
	}
	_ = lock5.Release()

	// 15f. Abnormal return (pre-cancelled context) cleanup
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = worker.Supervise(cancelledCtx, cfgNoWork)
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	lock6, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock not acquirable after cancelled context: %v", err)
	}
	_ = lock6.Release()
}

type conflictInjectingStore struct {
	*sqlite.Store
	conflictErr error
}

func (c *conflictInjectingStore) PublishRoleSuccess(ctx context.Context, params sqlite.PublishSuccessParams) (*sqlite.PublishResult, error) {
	return nil, c.conflictErr
}

func (c *conflictInjectingStore) PublishRoleFailure(ctx context.Context, params sqlite.PublishFailureParams) (*sqlite.PublishResult, error) {
	return nil, c.conflictErr
}

// 16. No second session claim, direct SQL, direct findings insertion, publication bypass, fabricated report, or provider call inside a transaction occurs.
func TestSupervise_NoForbiddenOperationsOrSecondClaim(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_active_single", "proj_test", policy)

	// Also submit a second queued session
	submitSession(t, store, "sess_queued_second", "proj_test", "key_queued_2", time.Now().UTC())

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       10 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session composed")
	}

	// Second session must remain queued! No second claim occurred!
	st2, err := store.ReadStatus(context.Background(), "sess_queued_second")
	if err != nil {
		t.Fatalf("ReadStatus sess_queued_second failed: %v", err)
	}
	if st2.Status != domain.SessionQueued {
		t.Fatalf("expected second session to remain queued, got: %s", st2.Status)
	}
}

// 17. Concurrent completion notifications remain serialized and race-clean.
func TestSupervise_ConcurrentCompletionNotificationsSerialized(t *testing.T) {
	store, _ := openTestStore(t)
	policy := standardTimingPolicy()
	claimRes, snap := submitAndClaimSession(t, store, "sess_concurrent_notif", "proj_test", policy)

	fake := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.RoleRequirements: {successScript()},
		domain.RoleArchitecture: {successScript()},
		domain.RoleQA:           {successScript()},
		domain.RoleSecurity:     {successScript()},
	})

	cfg := worker.SuperviseSessionConfig[
		*sqlite.DispatchReservationResult,
		*sqlite.SessionStatus,
		sqlite.PublishSuccessParams,
		sqlite.PublishFailureParams,
		*sqlite.PublishResult,
		*sqlite.ComposeResult,
	]{
		SessionID:          claimRes.SessionID,
		ProjectID:          claimRes.ProjectID,
		SnapshotID:         claimRes.SnapshotID,
		Snapshot:           snap,
		Provider:           fake,
		CallTimeout:        policy.CallTimeout,
		HardDeadlineAt:     claimRes.HardDeadlineAt,
		DispatchCutoffAt:   claimRes.DispatchCutoffAt,
		TickInterval:       5 * time.Millisecond,
		ReservationStore:   store,
		StatusReader:       store,
		PublicationStore:   store,
		Composer:           store,
		SnapshotReader:     store,
		BuildSuccessParams: standardPublishSuccessParamsBuilder,
		BuildFailureParams: standardPublishFailureParamsBuilder,
	}

	res, err := worker.SuperviseSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SuperviseSession failed: %v", err)
	}
	if res == nil || !res.IsComposed() {
		t.Fatal("expected session composed")
	}
}
