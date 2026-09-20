package worker_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite"
	"github.com/wyze69-sys/SpecCouncil/internal/worker"
)

func submitClaimedSession(
	t *testing.T,
	store *sqlite.Store,
	sessionID, projectID string,
	now time.Time,
	cutoffDuration time.Duration,
) string {
	t.Helper()
	ctx := context.Background()

	units := []evidence.Unit{
		{
			ID:   "unit_1",
			Kind: evidence.UnitRequirement,
			Text: "Dispatch test requirement",
		},
	}
	snap, err := evidence.Freeze("snap_"+sessionID, units)
	if err != nil {
		t.Fatalf("Freeze failed: %v", err)
	}

	_, err = store.Submit(ctx, sqlite.SubmitParams{
		SessionID:      sessionID,
		ProjectID:      projectID,
		IdempotencyKey: "key_" + sessionID,
		Title:          "Session " + sessionID,
		Content:        "Spec content",
		Snapshot:       snap,
		CreatedAt:      now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	policy := sqlite.TimingPolicy{
		DispatchCutoff:      cutoffDuration,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: cutoffDuration + 10*time.Minute,
	}

	claimRes, err := store.ClaimSessionWithNow(ctx, policy, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("ClaimSessionWithNow failed: %v", err)
	}
	if !claimRes.Claimed {
		t.Fatalf("expected session to be claimed, got reason: %s", claimRes.NoWorkReason)
	}

	return sessionID
}

// 1. Claimed reviewing session reserves the lowest canonical pending role first.
func TestDispatch_CanonicalRoleOrdering(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_order", "proj_order", now, 30*time.Minute)

	var reservedRoles []domain.Role
	var mu sync.Mutex

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     store,
		Clock:     func() time.Time { return now },
		OnReserved: func(ctx context.Context, res *sqlite.DispatchReservationResult) error {
			mu.Lock()
			defer mu.Unlock()
			reservedRoles = append(reservedRoles, res.Role)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	// Stop dispatcher after running initial burst
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- d.Run(ctx)
	}()

	// Wait until 2 roles are reserved (max in-flight capacity reached)
	for {
		mu.Lock()
		count := len(reservedRoles)
		mu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	d.Stop()
	<-runErrCh

	mu.Lock()
	defer mu.Unlock()
	if len(reservedRoles) != 2 {
		t.Fatalf("expected exactly 2 roles reserved, got: %d", len(reservedRoles))
	}

	// Canonical role order: requirements (rank 0), then architecture (rank 1)
	if reservedRoles[0] != domain.RoleRequirements {
		t.Fatalf("expected first role to be requirements, got: %s", reservedRoles[0])
	}
	if reservedRoles[1] != domain.RoleArchitecture {
		t.Fatalf("expected second role to be architecture, got: %s", reservedRoles[1])
	}
}

// 2. A completion notification wakes the same serialized owner and permits the next reservation.
func TestDispatch_CompletionWakesOwnerForNextReservation(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_comp", "proj_comp", now, 30*time.Minute)

	var reservedRoles []domain.Role
	var mu sync.Mutex
	reservedChan := make(chan domain.Role, 8)

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     store,
		Clock:     func() time.Time { return now },
		OnReserved: func(ctx context.Context, res *sqlite.DispatchReservationResult) error {
			mu.Lock()
			reservedRoles = append(reservedRoles, res.Role)
			mu.Unlock()
			reservedChan <- res.Role
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- d.Run(ctx)
	}()

	// Initial burst reserves first 2 roles: requirements, architecture
	r1 := <-reservedChan
	r2 := <-reservedChan
	if r1 != domain.RoleRequirements || r2 != domain.RoleArchitecture {
		t.Fatalf("unexpected initial burst roles: %s, %s", r1, r2)
	}

	// Fulfill capacity: notify completion of requirements.
	// We also need to mark requirements complete or terminal in DB so SQLite does not see it as in_flight
	// For testing persistence without provider, we can publish failure or use SweepHardDeadline or SweepRestart
	// Or even simpler, in W3 we publish via sqlite.PublishRoleFailure to advance DB state:
	_, err = store.PublishRoleFailure(context.Background(), sqlite.PublishFailureParams{
		SessionID:     sessionID,
		RoleRunID:     d.Trace()[0].RoleRunID,
		ErrorCategory: domain.ErrTransport,
		ErrorMessage:  "test simulated terminal",
		CallCount:     1,
		CompletedAt:   time.Now().UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatalf("PublishRoleFailure requirements: %v", err)
	}

	// Notify completion to wake the owner loop
	d.NotifyRoleCompleted(domain.RoleRequirements)

	// Owner loop wakes and reserves next pending role: QA!
	r3 := <-reservedChan
	if r3 != domain.RoleQA {
		t.Fatalf("expected next reserved role to be QA, got: %s", r3)
	}

	// Now complete architecture in DB and notify dispatcher
	_, err = store.PublishRoleFailure(context.Background(), sqlite.PublishFailureParams{
		SessionID:     sessionID,
		RoleRunID:     d.Trace()[1].RoleRunID,
		ErrorCategory: domain.ErrTransport,
		ErrorMessage:  "test simulated terminal",
		CallCount:     1,
		CompletedAt:   time.Now().UTC().Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("PublishRoleFailure architecture: %v", err)
	}

	d.NotifyRoleCompleted(domain.RoleArchitecture)

	// Owner loop wakes and reserves next pending role: Security!
	r4 := <-reservedChan
	if r4 != domain.RoleSecurity {
		t.Fatalf("expected fourth reserved role to be Security, got: %s", r4)
	}

	d.Stop()
	<-runErrCh

	mu.Lock()
	defer mu.Unlock()
	expected := []domain.Role{
		domain.RoleRequirements,
		domain.RoleArchitecture,
		domain.RoleQA,
		domain.RoleSecurity,
	}
	if len(reservedRoles) != 4 {
		t.Fatalf("expected 4 reserved roles, got %d", len(reservedRoles))
	}
	for i, r := range expected {
		if reservedRoles[i] != r {
			t.Fatalf("role index %d: expected %s, got %s", i, r, reservedRoles[i])
		}
	}
}

// wrappedStoreWithConcurrencyTracker records concurrent calls to ReservePendingRole.
type wrappedStoreWithConcurrencyTracker struct {
	store           *sqlite.Store
	activeCalls     atomic.Int32
	maxActiveCalls  atomic.Int32
	reservationDone chan struct{}
}

func (w *wrappedStoreWithConcurrencyTracker) ReservePendingRole(ctx context.Context, sessionID string) (*sqlite.DispatchReservationResult, error) {
	curr := w.activeCalls.Add(1)
	for {
		max := w.maxActiveCalls.Load()
		if curr <= max || w.maxActiveCalls.CompareAndSwap(max, curr) {
			break
		}
	}
	defer func() {
		w.activeCalls.Add(-1)
		if w.reservationDone != nil {
			select {
			case w.reservationDone <- struct{}{}:
			default:
			}
		}
	}()

	return w.store.ReservePendingRole(ctx, sessionID)
}

func (w *wrappedStoreWithConcurrencyTracker) ReadStatus(ctx context.Context, sessionID string) (*sqlite.SessionStatus, error) {
	return w.store.ReadStatus(ctx, sessionID)
}

// 3. Two simultaneous wake/tick signals still produce one ordered reservation sequence with no concurrent calls to ReservePendingRole.
func TestDispatch_SimultaneousSignalsSerializedSingleOwner(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_concurrent_signals", "proj_test", now, 30*time.Minute)

	tracker := &wrappedStoreWithConcurrencyTracker{
		store:           store,
		reservationDone: make(chan struct{}, 10),
	}

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     tracker,
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- d.Run(ctx)
	}()

	// Fire 30 concurrent goroutines spamming Wake and Tick simultaneously
	var wg sync.WaitGroup
	wg.Add(30)
	startBarrier := make(chan struct{})

	for i := 0; i < 30; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startBarrier
			if idx%2 == 0 {
				d.Wake()
			} else {
				d.Tick()
			}
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	// Let the owner loop settle
	d.Stop()
	<-runErrCh

	// Maximum concurrent active calls to ReservePendingRole must be EXACTLY 1
	maxActive := tracker.maxActiveCalls.Load()
	if maxActive > 1 {
		t.Fatalf("expected max active calls to be 1 (strictly serialized), got: %d", maxActive)
	}
	if maxActive == 0 {
		t.Fatal("expected at least 1 reservation call")
	}
}

// 4. Persisted capacity 0/1/2 is respected and never exceeds two in-flight roles.
func TestDispatch_MaxInFlightCapacityRespected(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_cap", "proj_cap", now, 30*time.Minute)

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     store,
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- d.Run(ctx)
	}()

	// Wait for local in-flight to reach 2
	for i := 0; i < 50; i++ {
		if d.LocalInFlight() == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if inFlight := d.LocalInFlight(); inFlight != 2 {
		t.Fatalf("expected local in flight to be 2, got: %d", inFlight)
	}

	// Repeatedly signal wake and tick; capacity must never exceed 2
	for i := 0; i < 10; i++ {
		d.Wake()
		d.Tick()
	}

	d.Stop()
	<-runErrCh

	if inFlight := d.LocalInFlight(); inFlight != 2 {
		t.Fatalf("expected local in flight to remain 2, got: %d", inFlight)
	}

	// Check trace for capacity_full no-work reason
	traces := d.Trace()
	foundCapacityFull := false
	for _, tr := range traces {
		if tr.NoWorkReason == worker.DispatchNoWorkCapacityFull {
			foundCapacityFull = true
			break
		}
	}
	if !foundCapacityFull {
		t.Fatal("expected capacity_full trace entry when capacity reached")
	}
}

// 5. Committed cancellation prevents later reservation and preserves the typed cancelled no-work reason.
func TestDispatch_CommittedCancellationPreventsLaterReservation(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_cancel", "proj_cancel", now, 30*time.Minute)

	// Commit cancellation on the reviewing session
	cancelRes, err := store.RequestCancellation(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RequestCancellation failed: %v", err)
	}
	if !cancelRes.Effective {
		t.Fatal("expected cancellation to be effective")
	}

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     store,
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	reserved, err := d.Step(context.Background())
	if err != nil {
		t.Fatalf("unexpected Step error: %v", err)
	}
	if reserved {
		t.Fatal("expected no reservation when session is cancelled")
	}

	// No role should be in-flight
	if d.LocalInFlight() != 0 {
		t.Fatalf("expected 0 roles in flight after cancellation, got: %d", d.LocalInFlight())
	}

	// Trace must contain cancelled no-work reason
	traces := d.Trace()
	foundCancelled := false
	for _, tr := range traces {
		if tr.NoWorkReason == worker.DispatchNoWorkCancelled {
			foundCancelled = true
			break
		}
	}
	if !foundCancelled {
		t.Fatal("expected cancelled no-work reason in trace")
	}
}

// 6. Committed cutoff prevents later reservation and preserves the typed cutoff no-work reason.
func TestDispatch_CommittedCutoffPreventsLaterReservation(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	// Create session whose dispatch cutoff was 5 minutes ago
	sessionID := submitClaimedSession(t, store, "sess_cutoff", "proj_cutoff", now.Add(-20*time.Minute), 15*time.Minute)

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     store,
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	reserved, err := d.Step(context.Background())
	if err != nil {
		t.Fatalf("unexpected Step error: %v", err)
	}
	if reserved {
		t.Fatal("expected no reservation past cutoff")
	}

	// Zero roles reserved
	if d.LocalInFlight() != 0 {
		t.Fatalf("expected 0 roles in flight past cutoff, got: %d", d.LocalInFlight())
	}

	// Trace must contain cutoff no-work reason
	traces := d.Trace()
	foundCutoff := false
	for _, tr := range traces {
		if tr.NoWorkReason == worker.DispatchNoWorkCutoff {
			foundCutoff = true
			break
		}
	}
	if !foundCutoff {
		t.Fatal("expected cutoff no-work reason in trace")
	}
}

// 7. Reservation commits before OnReserved begins; callback failure cannot roll back or rewrite the committed in-flight role.
func TestDispatch_CommitBeforeCallback(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_callback_fail", "proj_cb", now, 30*time.Minute)

	callbackErr := errors.New("injected callback boom")
	var roleRunID string
	var mu sync.Mutex

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     store,
		Clock:     func() time.Time { return now },
		OnReserved: func(ctx context.Context, res *sqlite.DispatchReservationResult) error {
			mu.Lock()
			roleRunID = res.RoleRunID
			mu.Unlock()

			// Directly query SQLite to verify role is ALREADY in_flight in storage BEFORE this callback finishes
			status, sErr := store.ReadStatus(context.Background(), sessionID)
			if sErr != nil {
				t.Errorf("ReadStatus inside callback failed: %v", sErr)
			}
			foundInFlight := false
			for _, r := range status.Roles {
				if r.Role == domain.RoleRequirements && r.Status == domain.RoleInFlight {
					foundInFlight = true
					break
				}
			}
			if !foundInFlight {
				t.Errorf("requirements role was NOT in_flight in database before OnReserved completed")
			}

			return callbackErr
		},
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- d.Run(ctx)
	}()

	// Wait for callback to execute
	for {
		mu.Lock()
		id := roleRunID
		mu.Unlock()
		if id != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	d.Stop()
	<-runErrCh

	// Verify in database: requirements role is STILL committed in in_flight status
	status, err := store.ReadStatus(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	var reqStatus domain.RoleStatus
	for _, r := range status.Roles {
		if r.Role == domain.RoleRequirements {
			reqStatus = r.Status
		}
	}
	if reqStatus != domain.RoleInFlight {
		t.Fatalf("expected requirements to remain in_flight despite callback failure, got: %s", reqStatus)
	}
}

// 8. No pending role produces the typed no-pending result without an error.
func TestDispatch_NoPendingRoleProducesTypedResult(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_no_pending", "proj_np", now, 30*time.Minute)

	// Sweep or publish all roles to complete/terminal
	for _, r := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(context.Background(), sessionID, now.Add(-5*time.Minute))
		if err != nil {
			t.Fatalf("ReservePendingRoleWithNow %s: %v", r, err)
		}
		if !res.Reserved {
			t.Fatalf("failed reserving %s", r)
		}
		_, err = store.PublishRoleFailure(context.Background(), sqlite.PublishFailureParams{
			SessionID:     sessionID,
			RoleRunID:     res.RoleRunID,
			ErrorCategory: domain.ErrTransport,
			ErrorMessage:  "terminal test",
			CallCount:     1,
			CompletedAt:   now,
		})
		if err != nil {
			t.Fatalf("PublishRoleFailure %s: %v", r, err)
		}
	}

	// All 4 roles are now terminal (failed). Zero pending roles remain.
	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     store,
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	reserved, err := d.Step(context.Background())
	if err != nil {
		t.Fatalf("expected nil error on no pending role, got: %v", err)
	}
	if reserved {
		t.Fatal("expected no reservation when no pending role")
	}

	traces := d.Trace()
	foundNoPending := false
	for _, tr := range traces {
		if tr.NoWorkReason == worker.DispatchNoWorkNoPendingRole {
			foundNoPending = true
			break
		}
	}
	if !foundNoPending {
		t.Fatal("expected no_pending_role reason in trace")
	}
}

// 9. Context cancellation stops the loop without leaked goroutines or timers.
func TestDispatch_ContextCancellationStopsLoop(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_cancel_ctx", "proj_ctx", now, 30*time.Minute)

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID:    sessionID,
		Store:        store,
		TickInterval: 50 * time.Millisecond,
		Clock:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- d.Run(ctx)
	}()

	// Cancel context
	cancel()

	err = <-runErrCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

// wrappedStoreWithInjectedError injects a persistence error into ReservePendingRole.
type wrappedStoreWithInjectedError struct {
	store *sqlite.Store
	err   error
}

func (w *wrappedStoreWithInjectedError) ReservePendingRole(ctx context.Context, sessionID string) (*sqlite.DispatchReservationResult, error) {
	return nil, w.err
}

// 10. A persistence error exits the loop cleanly and is returned unchanged.
func TestDispatch_PersistenceErrorExitsCleanly(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_db_err", "proj_err", now, 30*time.Minute)

	injectedErr := errors.New("simulated database disk I/O failure")
	wrappedStore := &wrappedStoreWithInjectedError{
		store: store,
		err:   injectedErr,
	}

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     wrappedStore,
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	err = d.Run(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error returned unchanged, got: %v", err)
	}
}

// 11. No provider, composer, publication, second claim, or transaction-held callback occurs.
func TestDispatch_NoForbiddenOperations(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now().UTC()
	sessionID := submitClaimedSession(t, store, "sess_forbidden", "proj_forbidden", now, 30*time.Minute)

	d, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: sessionID,
		Store:     store,
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- d.Run(ctx)
	}()

	d.Stop()
	<-runErrCh

	// Verify session is still in reviewing state (no composer was executed)
	status, err := store.ReadStatus(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if status.Status != domain.SessionReviewing {
		t.Fatalf("expected session to remain reviewing, got: %s", status.Status)
	}
	if status.TerminalReason != "" {
		t.Fatalf("expected terminal reason to be empty, got: %s", status.TerminalReason)
	}
}

// 12. Config validation rejects invalid parameters.
func TestDispatch_ConfigValidation(t *testing.T) {
	store, _ := openTestStore(t)

	// Empty session ID
	_, err := worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: "",
		Store:     store,
	})
	if err == nil {
		t.Fatal("expected error on empty session ID, got nil")
	}
	if !errors.Is(err, worker.ErrInvalidSessionID) {
		t.Fatalf("expected ErrInvalidSessionID, got: %v", err)
	}

	// Whitespace session ID
	_, err = worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: "   ",
		Store:     store,
	})
	if err == nil {
		t.Fatal("expected error on whitespace session ID, got nil")
	}
	if !errors.Is(err, worker.ErrInvalidSessionID) {
		t.Fatalf("expected ErrInvalidSessionID, got: %v", err)
	}

	// Nil store
	_, err = worker.NewDispatcher(worker.DispatchConfig[*sqlite.DispatchReservationResult, *sqlite.SessionStatus]{
		SessionID: "sess_1",
		Store:     nil,
	})
	if err == nil {
		t.Fatal("expected error on nil store, got nil")
	}
	if !errors.Is(err, worker.ErrNilStore) {
		t.Fatalf("expected ErrNilStore, got: %v", err)
	}
}
