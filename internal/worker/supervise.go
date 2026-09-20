package worker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
)

var (
	// ErrSessionNotReviewing is returned when a session passed to SuperviseSession is not in reviewing status.
	ErrSessionNotReviewing = errors.New("worker: session is not in reviewing status")

	// ErrSupervisorClosed is returned when an operation is attempted after supervisor is stopped.
	ErrSupervisorClosed = errors.New("worker: supervisor is closed")

	// ErrPublicationConflict surfaces compare-and-set publication conflict.
	ErrPublicationConflict = errors.New("worker: role publication conflict")

	// ErrCompositionRefused is returned when session composition is refused by storage.
	ErrCompositionRefused = errors.New("worker: session composition refused")
)

// SessionPublisher abstracts atomic compare-and-set publication of role outcomes.
// It is directly satisfied by *sqlite.Store.
type SessionPublisher[TSuccessParams any, TFailureParams any, TPublishResult any] interface {
	PublishRoleSuccess(ctx context.Context, params TSuccessParams) (TPublishResult, error)
	PublishRoleFailure(ctx context.Context, params TFailureParams) (TPublishResult, error)
}

// SessionComposer abstracts atomic transactional session composition.
// It is directly satisfied by *sqlite.Store.
type SessionComposer[TComposeResult any] interface {
	ComposeSession(ctx context.Context, sessionID string) (TComposeResult, error)
}

// SnapshotReader abstracts loading immutable evidence snapshots.
// It is directly satisfied by *sqlite.Store.
type SnapshotReader interface {
	ReadSnapshot(ctx context.Context, snapshotID string) (*evidence.Snapshot, error)
}

// PublishSuccessInput carries all parameters needed to construct a success publication.
type PublishSuccessInput struct {
	ProjectID   string
	SessionID   string
	RoleRunID   string
	Role        domain.Role
	Findings    []domain.Finding
	CallCount   int
	CompletedAt time.Time
}

// PublishFailureInput carries all parameters needed to construct a failure publication.
type PublishFailureInput struct {
	ProjectID     string
	SessionID     string
	RoleRunID     string
	Role          domain.Role
	ErrorCategory domain.ErrorCategory
	ErrorMessage  string
	CallCount     int
	CompletedAt   time.Time
}

// SuperviseSessionConfig defines configuration for supervising a single claimed reviewing session.
type SuperviseSessionConfig[
	TReservation any,
	TStatus any,
	TSuccessParams any,
	TFailureParams any,
	TPublishResult any,
	TComposeResult any,
] struct {
	SessionID        string
	ProjectID        string
	SnapshotID       string
	Snapshot         evidence.Snapshot
	Provider         provider.Provider
	Budget           review.Budget
	Policy           review.Policy
	CallTimeout      time.Duration
	HardDeadlineAt   time.Time
	DispatchCutoffAt time.Time
	BackoffMin       time.Duration
	BackoffMax       time.Duration
	TickInterval     time.Duration
	Clock            func() time.Time
	Sleep            func(ctx context.Context, d time.Duration) error
	Jitter           func(min, max time.Duration) time.Duration

	// Store components:
	ReservationStore RoleReservationStore[TReservation]
	StatusReader     SessionStatusReader[TStatus]
	PublicationStore SessionPublisher[TSuccessParams, TFailureParams, TPublishResult]
	Composer         SessionComposer[TComposeResult]
	SnapshotReader   SnapshotReader

	// Sweeper functions (optional):
	CancellationSweeper func(ctx context.Context, sessionID string) error
	CutoffSweeper       func(ctx context.Context, sessionID string, now time.Time) error
	HardDeadlineSweeper func(ctx context.Context, sessionID string, now time.Time) error

	// Param factories:
	BuildSuccessParams func(in PublishSuccessInput) TSuccessParams
	BuildFailureParams func(in PublishFailureInput) TFailureParams

	// Predicates:
	IsPublicationConflict func(err error) bool
	IsCompositionNotReady func(err error) bool

	// Testing hooks:
	OnRoleExecuting   func(role domain.Role, roleRunID string)
	OnRoleExecuted    func(role domain.Role, roleRunID string, outcome review.RoleOutcome)
	OnRolePublished   func(role domain.Role, roleRunID string, outcome review.RoleOutcome, err error)
	OnCompositionDone func(res TComposeResult, err error)
}

// SuperviseResult represents the authoritative outcome of session supervision.
type SuperviseResult[TComposeResult any] struct {
	SessionID     string
	Composed      bool
	ComposeResult TComposeResult
	Report        *review.Report
}

// IsComposed reports whether session composition was successfully performed or confirmed.
func (r *SuperviseResult[T]) IsComposed() bool {
	return r != nil && r.Composed
}

// TerminalReport returns the deterministic report produced by composition.
func (r *SuperviseResult[T]) TerminalReport() *review.Report {
	if r == nil {
		return nil
	}
	return r.Report
}

func defaultBuildSuccessParams[TSuccessParams any](in PublishSuccessInput) TSuccessParams {
	var zero TSuccessParams
	if v, ok := any(in).(TSuccessParams); ok {
		return v
	}
	return zero
}

func defaultBuildFailureParams[TFailureParams any](in PublishFailureInput) TFailureParams {
	var zero TFailureParams
	if v, ok := any(in).(TFailureParams); ok {
		return v
	}
	return zero
}

func isPublicationConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrPublicationConflict) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "publication conflict") ||
		strings.Contains(msg, "role run is no longer in_flight") ||
		strings.Contains(msg, "already terminal")
}

func isCompositionNotReady(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrCompositionRefused) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "not ready for composition") ||
		strings.Contains(msg, "session is not ready")
}

type readinessInfo struct {
	Valid           bool
	SessionStatus   domain.SessionStatus
	AllTerminal     bool
	InFlightCount   int
	PendingCount    int
	CancelRequested bool
	CutoffAt        *time.Time
	HardDeadlineAt  *time.Time
	RolesCount      int
}

func inspectReadiness(st any) readinessInfo {
	if st == nil {
		return readinessInfo{}
	}
	v := reflect.ValueOf(st)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return readinessInfo{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return readinessInfo{}
	}

	info := readinessInfo{Valid: true, AllTerminal: true}

	if f := v.FieldByName("CancelRequested"); f.IsValid() && f.Kind() == reflect.Bool {
		info.CancelRequested = f.Bool()
	}
	if f := v.FieldByName("DispatchCutoffAt"); f.IsValid() && !f.IsNil() {
		if t, ok := f.Interface().(*time.Time); ok {
			info.CutoffAt = t
		}
	}
	if f := v.FieldByName("HardDeadlineAt"); f.IsValid() && !f.IsNil() {
		if t, ok := f.Interface().(*time.Time); ok {
			info.HardDeadlineAt = t
		}
	}
	if f := v.FieldByName("Status"); f.IsValid() {
		if sStatus, ok := f.Interface().(domain.SessionStatus); ok {
			info.SessionStatus = sStatus
			if sStatus.IsTerminal() {
				info.AllTerminal = true
				return info
			}
		}
	}

	if f := v.FieldByName("Roles"); f.IsValid() && f.Kind() == reflect.Slice {
		info.RolesCount = f.Len()
		if info.RolesCount < domain.RoleCount {
			info.AllTerminal = false
		}
		for i := 0; i < f.Len(); i++ {
			elem := f.Index(i)
			if elem.Kind() == reflect.Struct {
				if sf := elem.FieldByName("Status"); sf.IsValid() {
					if rStatus, ok := sf.Interface().(domain.RoleStatus); ok {
						if !rStatus.IsTerminal() {
							info.AllTerminal = false
						}
						if rStatus == domain.RoleInFlight {
							info.InFlightCount++
						} else if rStatus == domain.RolePending {
							info.PendingCount++
						}
					}
				}
			}
		}
	} else {
		info.AllTerminal = false
	}

	return info
}

type supervisedReservationInfo struct {
	Reserved      bool
	NoWorkReason  string
	Role          domain.Role
	RoleRunID     string
	InFlightCount int
	StartedAt     time.Time
}

func extractReservationInfo(res any) supervisedReservationInfo {
	if res == nil {
		return supervisedReservationInfo{}
	}
	v := reflect.ValueOf(res)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return supervisedReservationInfo{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return supervisedReservationInfo{}
	}

	var info supervisedReservationInfo
	if f := v.FieldByName("Reserved"); f.IsValid() && f.Kind() == reflect.Bool {
		info.Reserved = f.Bool()
	}
	if f := v.FieldByName("NoWorkReason"); f.IsValid() {
		info.NoWorkReason = fmt.Sprint(f.Interface())
	}
	if f := v.FieldByName("Role"); f.IsValid() {
		if r, ok := f.Interface().(domain.Role); ok {
			info.Role = r
		} else {
			info.Role = domain.Role(fmt.Sprint(f.Interface()))
		}
	}
	if f := v.FieldByName("RoleRunID"); f.IsValid() && f.Kind() == reflect.String {
		info.RoleRunID = f.String()
	}
	if f := v.FieldByName("InFlightCount"); f.IsValid() && f.Kind() == reflect.Int {
		info.InFlightCount = int(f.Int())
	}
	if f := v.FieldByName("StartedAt"); f.IsValid() && !f.IsNil() {
		if t, ok := f.Interface().(*time.Time); ok && t != nil {
			info.StartedAt = *t
		} else if t, ok := f.Interface().(time.Time); ok {
			info.StartedAt = t
		}
	}
	return info
}

// SuperviseSession supervises execution of a single already-claimed reviewing session:
// 1. Accepts only an already-claimed reviewing session, rejecting non-reviewing sessions.
// 2. Coordinates serialized role dispatch, bounded role execution (W4), CAS publication, and dispatcher notification.
// 3. Enforces that dispatcher notification occurs only after atomic publication returns.
// 4. Halts dispatch and role execution promptly on cancellation, cutoff, hard deadline, or persistence failure.
// 5. Triggers deterministic session composition exactly once when all four roles are terminal and in-flight count is zero.
func SuperviseSession[
	TReservation any,
	TStatus any,
	TSuccessParams any,
	TFailureParams any,
	TPublishResult any,
	TComposeResult any,
](
	ctx context.Context,
	cfg SuperviseSessionConfig[TReservation, TStatus, TSuccessParams, TFailureParams, TPublishResult, TComposeResult],
) (*SuperviseResult[TComposeResult], error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	if strings.TrimSpace(cfg.SessionID) == "" {
		return nil, ErrInvalidSessionID
	}
	if isNil(cfg.ReservationStore) {
		return nil, ErrNilStore
	}
	if isNil(cfg.PublicationStore) {
		return nil, errors.New("worker: publication store cannot be nil")
	}
	if isNil(cfg.Composer) {
		return nil, errors.New("worker: composer cannot be nil")
	}
	if isNil(cfg.Provider) {
		return nil, ErrNilProvider
	}
	if cfg.CallTimeout <= 0 {
		return nil, ErrZeroCallTimeout
	}
	if cfg.HardDeadlineAt.IsZero() {
		return nil, ErrZeroHardDeadline
	}

	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	tickInterval := cfg.TickInterval
	if tickInterval <= 0 {
		tickInterval = 50 * time.Millisecond
	}

	// Verify session is reviewing if StatusReader is provided.
	if !isNil(cfg.StatusReader) {
		st, err := cfg.StatusReader.ReadStatus(ctx, cfg.SessionID)
		if err != nil {
			return nil, err
		}
		readiness := inspectReadiness(st)
		if readiness.Valid && readiness.SessionStatus != "" {
			if readiness.SessionStatus != domain.SessionReviewing {
				return nil, fmt.Errorf("%w (found %s)", ErrSessionNotReviewing, readiness.SessionStatus)
			}
		}
	}

	// Resolve immutable frozen snapshot.
	snapshot := cfg.Snapshot
	if len(snapshot.Units) == 0 {
		if !isNil(cfg.SnapshotReader) && strings.TrimSpace(cfg.SnapshotID) != "" {
			snap, err := cfg.SnapshotReader.ReadSnapshot(ctx, cfg.SnapshotID)
			if err != nil {
				return nil, err
			}
			if snap != nil {
				snapshot = *snap
			}
		}
	}
	if len(snapshot.Units) == 0 {
		return nil, ErrEmptySnapshot
	}

	// Setup synchronization and fatal error tracking.
	var inFlightWG sync.WaitGroup
	var activeInFlight atomic.Int32
	var fatalErrMu sync.Mutex
	var fatalErr error

	setFatalErr := func(err error) {
		if err == nil {
			return
		}
		fatalErrMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalErrMu.Unlock()
	}

	getFatalErr := func() error {
		fatalErrMu.Lock()
		defer fatalErrMu.Unlock()
		return fatalErr
	}

	gateWakeCh := make(chan struct{}, 16)
	wakeGate := func() {
		select {
		case gateWakeCh <- struct{}{}:
		default:
		}
	}

	roleParentCtx, cancelRoleParent := context.WithCancel(ctx)
	defer cancelRoleParent()

	var onceStopDispatcher sync.Once
	var dispatcher *Dispatcher[TReservation, TStatus]

	stopDispatcher := func() {
		onceStopDispatcher.Do(func() {
			if dispatcher != nil {
				dispatcher.Stop()
			}
		})
	}
	defer stopDispatcher()

	// Reservation callback invoked on dispatcher owner goroutine after reservation transaction commits.
	onReserved := func(resCtx context.Context, reservation TReservation) error {
		info := extractReservationInfo(reservation)
		if !info.Reserved {
			return nil
		}

		if getFatalErr() != nil {
			return nil
		}

		inFlightWG.Add(1)
		activeInFlight.Add(1)

		go func(rInfo supervisedReservationInfo) {
			defer inFlightWG.Done()
			defer activeInFlight.Add(-1)
			defer wakeGate()

			if cfg.OnRoleExecuting != nil {
				cfg.OnRoleExecuting(rInfo.Role, rInfo.RoleRunID)
			}

			// Bounded per-attempt deadline.
			now := clock().UTC()
			attemptDeadline := now.Add(cfg.CallTimeout)
			if attemptDeadline.After(cfg.HardDeadlineAt) {
				attemptDeadline = cfg.HardDeadlineAt
			}

			roleCtx, roleCancel := context.WithDeadline(roleParentCtx, attemptDeadline)
			defer roleCancel()

			execCfg := ExecuteConfig{
				Role:           rInfo.Role,
				Snapshot:       snapshot,
				Provider:       cfg.Provider,
				Budget:         cfg.Budget,
				Policy:         cfg.Policy,
				CallTimeout:    cfg.CallTimeout,
				HardDeadlineAt: cfg.HardDeadlineAt,
				BackoffMin:     cfg.BackoffMin,
				BackoffMax:     cfg.BackoffMax,
				Now:            clock,
				Sleep:          cfg.Sleep,
				Jitter:         cfg.Jitter,
			}

			outcome, execErr := Execute(roleCtx, execCfg)
			var errMsg string
			if execErr != nil {
				outcome = review.RoleOutcome{
					Role:          rInfo.Role,
					Status:        domain.RoleFailed,
					ErrorCategory: domain.ErrProviderRejected,
				}
				errMsg = execErr.Error()
			}

			if cfg.OnRoleExecuted != nil {
				cfg.OnRoleExecuted(rInfo.Role, rInfo.RoleRunID, outcome)
			}

			if getFatalErr() != nil {
				return
			}

			// Publish terminal outcome atomically.
			pubCtx, pubCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer pubCancel()

			var pubErr error
			pubNow := clock().UTC()
			if !rInfo.StartedAt.IsZero() && pubNow.Before(rInfo.StartedAt) {
				pubNow = rInfo.StartedAt
			}

			if outcome.Status == domain.RoleComplete {
				callCount := outcome.CallCount
				if callCount < 1 {
					callCount = 1
				}
				if callCount > domain.MaxProviderCallsPerRole {
					callCount = domain.MaxProviderCallsPerRole
				}
				successInput := PublishSuccessInput{
					ProjectID:   cfg.ProjectID,
					SessionID:   cfg.SessionID,
					RoleRunID:   rInfo.RoleRunID,
					Role:        rInfo.Role,
					Findings:    outcome.Findings,
					CallCount:   callCount,
					CompletedAt: pubNow,
				}
				var params TSuccessParams
				if cfg.BuildSuccessParams != nil {
					params = cfg.BuildSuccessParams(successInput)
				} else {
					params = defaultBuildSuccessParams[TSuccessParams](successInput)
				}
				_, pubErr = cfg.PublicationStore.PublishRoleSuccess(pubCtx, params)
			} else {
				callCount := outcome.CallCount
				if callCount < 0 {
					callCount = 0
				}
				if callCount > domain.MaxProviderCallsPerRole {
					callCount = domain.MaxProviderCallsPerRole
				}
				failureInput := PublishFailureInput{
					ProjectID:     cfg.ProjectID,
					SessionID:     cfg.SessionID,
					RoleRunID:     rInfo.RoleRunID,
					Role:          rInfo.Role,
					ErrorCategory: outcome.ErrorCategory,
					ErrorMessage:  errMsg,
					CallCount:     callCount,
					CompletedAt:   pubNow,
				}
				var params TFailureParams
				if cfg.BuildFailureParams != nil {
					params = cfg.BuildFailureParams(failureInput)
				} else {
					params = defaultBuildFailureParams[TFailureParams](failureInput)
				}
				_, pubErr = cfg.PublicationStore.PublishRoleFailure(pubCtx, params)
			}

			if cfg.OnRolePublished != nil {
				cfg.OnRolePublished(rInfo.Role, rInfo.RoleRunID, outcome, pubErr)
			}

			if pubErr != nil {
				isConflict := cfg.IsPublicationConflict
				if isConflict == nil {
					isConflict = isPublicationConflict
				}
				if isConflict(pubErr) {
					setFatalErr(fmt.Errorf("%w: %v", ErrPublicationConflict, pubErr))
				} else {
					setFatalErr(pubErr)
				}
				stopDispatcher()
				return
			}

			// Notify dispatcher strictly AFTER publication has returned.
			dispatcher.NotifyRoleCompleted(rInfo.Role)
		}(info)

		return nil
	}

	// Create and start dispatcher.
	dispCfg := DispatchConfig[TReservation, TStatus]{
		SessionID:    cfg.SessionID,
		Store:        cfg.ReservationStore,
		StatusReader: cfg.StatusReader,
		TickInterval: tickInterval,
		OnReserved:   onReserved,
		Clock:        clock,
	}
	var dispErr error
	dispatcher, dispErr = NewDispatcher(dispCfg)
	if dispErr != nil {
		return nil, dispErr
	}

	dispatchDoneCh := make(chan error, 1)
	go func() {
		dispatchDoneCh <- dispatcher.Run(ctx)
	}()

	// Setup deadline and cutoff timers.
	now := clock().UTC()
	var cutoffCh <-chan time.Time
	if !cfg.DispatchCutoffAt.IsZero() && cfg.DispatchCutoffAt.After(now) {
		cutoffTimer := time.NewTimer(cfg.DispatchCutoffAt.Sub(now))
		defer cutoffTimer.Stop()
		cutoffCh = cutoffTimer.C
	}

	var hardDeadlineCh <-chan time.Time
	if !cfg.HardDeadlineAt.IsZero() && cfg.HardDeadlineAt.After(now) {
		hardDeadlineTimer := time.NewTimer(cfg.HardDeadlineAt.Sub(now))
		defer hardDeadlineTimer.Stop()
		hardDeadlineCh = hardDeadlineTimer.C
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// Gate evaluation helper: check if all 4 roles are terminal and 0 in-flight.
	checkAndCompose := func() (*SuperviseResult[TComposeResult], error) {
		if fErr := getFatalErr(); fErr != nil {
			return nil, fErr
		}

		if isNil(cfg.StatusReader) {
			return nil, nil
		}

		st, err := cfg.StatusReader.ReadStatus(ctx, cfg.SessionID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}

		readiness := inspectReadiness(st)
		if readiness.CancelRequested && cfg.CancellationSweeper != nil {
			_ = cfg.CancellationSweeper(ctx, cfg.SessionID)
			st, err = cfg.StatusReader.ReadStatus(ctx, cfg.SessionID)
			if err == nil {
				readiness = inspectReadiness(st)
			}
		}

		if readiness.AllTerminal && readiness.InFlightCount == 0 && activeInFlight.Load() == 0 {
			stopDispatcher()
			inFlightWG.Wait()

			compRes, cErr := cfg.Composer.ComposeSession(ctx, cfg.SessionID)
			if cErr != nil {
				isNotReady := cfg.IsCompositionNotReady
				if isNotReady == nil {
					isNotReady = isCompositionNotReady
				}
				if isNotReady(cErr) {
					return nil, nil
				}
				if cfg.OnCompositionDone != nil {
					var zero TComposeResult
					cfg.OnCompositionDone(zero, cErr)
				}
				return nil, fmt.Errorf("%w: %v", ErrCompositionRefused, cErr)
			}

			if cfg.OnCompositionDone != nil {
				cfg.OnCompositionDone(compRes, nil)
			}

			var rep *review.Report
			if trg, ok := any(compRes).(interface{ TerminalReport() *review.Report }); ok {
				rep = trg.TerminalReport()
			}

			return &SuperviseResult[TComposeResult]{
				SessionID:     cfg.SessionID,
				Composed:      true,
				ComposeResult: compRes,
				Report:        rep,
			}, nil
		}

		return nil, nil
	}

	// Initial evaluation check (e.g. cancelled while queued).
	if initialRes, err := checkAndCompose(); err != nil {
		stopDispatcher()
		cancelRoleParent()
		inFlightWG.Wait()
		return nil, err
	} else if initialRes != nil {
		stopDispatcher()
		inFlightWG.Wait()
		return initialRes, nil
	}

	// Coordinator loop.
	for {
		select {
		case <-ctx.Done():
			stopDispatcher()
			cancelRoleParent()
			inFlightWG.Wait()
			return nil, ctx.Err()

		case dErr := <-dispatchDoneCh:
			if dErr != nil && !errors.Is(dErr, context.Canceled) {
				setFatalErr(dErr)
				stopDispatcher()
				cancelRoleParent()
				inFlightWG.Wait()
				return nil, dErr
			}

		case <-cutoffCh:
			now := clock().UTC()
			if cfg.CutoffSweeper != nil {
				_ = cfg.CutoffSweeper(ctx, cfg.SessionID, now)
			}
			dispatcher.Wake()
			wakeGate()

		case <-hardDeadlineCh:
			now := clock().UTC()
			if cfg.HardDeadlineSweeper != nil {
				_ = cfg.HardDeadlineSweeper(ctx, cfg.SessionID, now)
			}
			cancelRoleParent()
			dispatcher.Wake()
			wakeGate()

		case <-ticker.C:
			now := clock().UTC()
			if !cfg.DispatchCutoffAt.IsZero() && !now.Before(cfg.DispatchCutoffAt) {
				if cfg.CutoffSweeper != nil {
					_ = cfg.CutoffSweeper(ctx, cfg.SessionID, now)
				}
			}
			if !cfg.HardDeadlineAt.IsZero() && !now.Before(cfg.HardDeadlineAt) {
				if cfg.HardDeadlineSweeper != nil {
					_ = cfg.HardDeadlineSweeper(ctx, cfg.SessionID, now)
				}
				cancelRoleParent()
			}
			if !isNil(cfg.StatusReader) {
				if st, err := cfg.StatusReader.ReadStatus(ctx, cfg.SessionID); err == nil {
					if inspectReadiness(st).CancelRequested && cfg.CancellationSweeper != nil {
						_ = cfg.CancellationSweeper(ctx, cfg.SessionID)
					}
				}
			}
			dispatcher.Tick()
			wakeGate()

		case <-gateWakeCh:
			res, err := checkAndCompose()
			if err != nil {
				stopDispatcher()
				cancelRoleParent()
				inFlightWG.Wait()
				return nil, err
			}
			if res != nil {
				return res, nil
			}
		}
	}
}

// SuperviseWorkerConfig configures the full single-session worker lifecycle:
// ownership lock, restart recovery, FIFO claim, and session supervision.
type SuperviseWorkerConfig[
	TRecovery any,
	TPolicy TimingPolicyValidator,
	TClaim any,
	TReservation any,
	TStatus any,
	TSuccessParams any,
	TFailureParams any,
	TPublishResult any,
	TComposeResult any,
] struct {
	LockPath     string
	Store        any
	TimingPolicy TPolicy
	Cutoff       time.Time
	Provider     provider.Provider
	Budget       review.Budget
	Policy       review.Policy
	CallTimeout  time.Duration
	TickInterval time.Duration
	Clock        func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
	Jitter       func(min, max time.Duration) time.Duration

	// Param factories:
	BuildSuccessParams func(in PublishSuccessInput) TSuccessParams
	BuildFailureParams func(in PublishFailureInput) TFailureParams

	// Error predicates:
	IsPublicationConflict func(err error) bool
	IsCompositionNotReady func(err error) bool

	// Sweeper hooks:
	CancellationSweeper func(ctx context.Context, sessionID string) error
	CutoffSweeper       func(ctx context.Context, sessionID string, now time.Time) error
	HardDeadlineSweeper func(ctx context.Context, sessionID string, now time.Time) error

	// Test seams:
	OnRoleExecuting   func(role domain.Role, roleRunID string)
	OnRoleExecuted    func(role domain.Role, roleRunID string, outcome review.RoleOutcome)
	OnRolePublished   func(role domain.Role, roleRunID string, outcome review.RoleOutcome, err error)
	OnCompositionDone func(res TComposeResult, err error)
}

type claimInfo struct {
	Claimed          bool
	NoWorkReason     string
	SessionID        string
	ProjectID        string
	SnapshotID       string
	Status           domain.SessionStatus
	CancelRequested  bool
	DispatchCutoffAt time.Time
	HardDeadlineAt   time.Time
}

func inspectClaim(cl any) claimInfo {
	if cl == nil {
		return claimInfo{}
	}
	v := reflect.ValueOf(cl)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return claimInfo{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return claimInfo{}
	}

	var info claimInfo
	if f := v.FieldByName("Claimed"); f.IsValid() && f.Kind() == reflect.Bool {
		info.Claimed = f.Bool()
	}
	if f := v.FieldByName("NoWorkReason"); f.IsValid() {
		info.NoWorkReason = fmt.Sprint(f.Interface())
	}
	if f := v.FieldByName("SessionID"); f.IsValid() && f.Kind() == reflect.String {
		info.SessionID = f.String()
	}
	if f := v.FieldByName("ProjectID"); f.IsValid() && f.Kind() == reflect.String {
		info.ProjectID = f.String()
	}
	if f := v.FieldByName("SnapshotID"); f.IsValid() && f.Kind() == reflect.String {
		info.SnapshotID = f.String()
	}
	if f := v.FieldByName("Status"); f.IsValid() {
		if s, ok := f.Interface().(domain.SessionStatus); ok {
			info.Status = s
		}
	}
	if f := v.FieldByName("CancelRequested"); f.IsValid() && f.Kind() == reflect.Bool {
		info.CancelRequested = f.Bool()
	}
	if f := v.FieldByName("DispatchCutoffAt"); f.IsValid() {
		if t, ok := f.Interface().(time.Time); ok {
			info.DispatchCutoffAt = t
		}
	}
	if f := v.FieldByName("HardDeadlineAt"); f.IsValid() {
		if t, ok := f.Interface().(time.Time); ok {
			info.HardDeadlineAt = t
		}
	}
	return info
}

func extractCallTimeout(policy any) time.Duration {
	if policy == nil {
		return 0
	}
	v := reflect.ValueOf(policy)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName("CallTimeout"); f.IsValid() && f.Kind() == reflect.Int64 {
			return time.Duration(f.Int())
		}
	}
	return 0
}

// Supervise executes the full single-session worker lifecycle:
//  1. Validates configuration before acquiring ownership.
//  2. Acquires exclusive process lock, returning ErrAlreadyOwned promptly on contention.
//  3. Defers lock release across all paths, guaranteeing cleanup on success, no-work, cancellation,
//     publication conflict, publication error, composition refusal, composition failure, and abnormal worker return.
//  4. Executes exactly one restart recovery sweep before claiming work.
//  5. Claims at most one FIFO session (no-work returns cleanly with lock released).
//  6. Runs SuperviseSession on the claimed session.
//  7. Returns composition result and releases the process lock.
func Supervise[
	TRecovery any,
	TPolicy TimingPolicyValidator,
	TClaim any,
	TReservation any,
	TStatus any,
	TSuccessParams any,
	TFailureParams any,
	TPublishResult any,
	TComposeResult any,
](
	ctx context.Context,
	cfg SuperviseWorkerConfig[TRecovery, TPolicy, TClaim, TReservation, TStatus, TSuccessParams, TFailureParams, TPublishResult, TComposeResult],
) (res *SuperviseResult[TComposeResult], err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	if strings.TrimSpace(cfg.LockPath) == "" {
		return nil, &PathError{
			Op:   "validate_config",
			Path: cfg.LockPath,
			Err:  ErrEmptyPath,
		}
	}
	if isNil(cfg.Store) {
		return nil, ErrNilStore
	}
	if cfg.Cutoff.IsZero() {
		return nil, ErrZeroCutoff
	}
	if isNil(cfg.TimingPolicy) {
		return nil, ErrNilTimingPolicy
	}
	if valErr := cfg.TimingPolicy.Validate(); valErr != nil {
		return nil, valErr
	}
	if isNil(cfg.Provider) {
		return nil, ErrNilProvider
	}

	// 1. Acquire exclusive process ownership.
	lock, lockErr := AcquireProcessLock(ctx, cfg.LockPath)
	if lockErr != nil {
		return nil, lockErr
	}

	// 2. Defer lock release across ALL exit paths.
	defer func() {
		var relErr error
		if testReleaseHook != nil {
			relErr = testReleaseHook(lock)
		} else {
			relErr = lock.Release()
		}
		if relErr != nil {
			if err != nil {
				err = errors.Join(err, relErr)
			} else {
				err = relErr
			}
		}
	}()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	// 3. Restart recovery sweep before claim.
	sessionStore, ok := cfg.Store.(SessionStore[TRecovery, TPolicy, TClaim])
	if !ok {
		return nil, errors.New("worker: store does not satisfy SessionStore")
	}

	if testRecoveryHook != nil {
		if hookErr := testRecoveryHook(ctx); hookErr != nil {
			return nil, hookErr
		}
	}

	_, recErr := sessionStore.SweepRestartRecovery(ctx, cfg.Cutoff)
	if recErr != nil {
		return nil, recErr
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	// 4. Claim single session.
	if testClaimHook != nil {
		if hookErr := testClaimHook(ctx); hookErr != nil {
			return nil, hookErr
		}
	}

	claimRes, claimErr := sessionStore.ClaimSession(ctx, cfg.TimingPolicy)
	if claimErr != nil {
		return nil, claimErr
	}

	info := inspectClaim(claimRes)
	if !info.Claimed {
		// No-work is a successful outcome; lock released by defer.
		return nil, nil
	}

	// 5. Session claimed: supervise execution through composition.
	callTimeout := cfg.CallTimeout
	if callTimeout <= 0 {
		callTimeout = extractCallTimeout(cfg.TimingPolicy)
	}

	resStore, ok := cfg.Store.(RoleReservationStore[TReservation])
	if !ok {
		return nil, errors.New("worker: store does not satisfy RoleReservationStore")
	}

	pubStore, ok := cfg.Store.(SessionPublisher[TSuccessParams, TFailureParams, TPublishResult])
	if !ok {
		return nil, errors.New("worker: store does not satisfy SessionPublisher")
	}

	composer, ok := cfg.Store.(SessionComposer[TComposeResult])
	if !ok {
		return nil, errors.New("worker: store does not satisfy SessionComposer")
	}

	var statusReader SessionStatusReader[TStatus]
	if sr, ok := cfg.Store.(SessionStatusReader[TStatus]); ok {
		statusReader = sr
	}

	var snapReader SnapshotReader
	if snr, ok := cfg.Store.(SnapshotReader); ok {
		snapReader = snr
	}

	sessCfg := SuperviseSessionConfig[TReservation, TStatus, TSuccessParams, TFailureParams, TPublishResult, TComposeResult]{
		SessionID:             info.SessionID,
		ProjectID:             info.ProjectID,
		SnapshotID:            info.SnapshotID,
		Provider:              cfg.Provider,
		Budget:                cfg.Budget,
		Policy:                cfg.Policy,
		CallTimeout:           callTimeout,
		HardDeadlineAt:        info.HardDeadlineAt,
		DispatchCutoffAt:      info.DispatchCutoffAt,
		TickInterval:          cfg.TickInterval,
		Clock:                 cfg.Clock,
		Sleep:                 cfg.Sleep,
		Jitter:                cfg.Jitter,
		ReservationStore:      resStore,
		StatusReader:          statusReader,
		PublicationStore:      pubStore,
		Composer:              composer,
		SnapshotReader:        snapReader,
		CancellationSweeper:   cfg.CancellationSweeper,
		CutoffSweeper:         cfg.CutoffSweeper,
		HardDeadlineSweeper:   cfg.HardDeadlineSweeper,
		BuildSuccessParams:    cfg.BuildSuccessParams,
		BuildFailureParams:    cfg.BuildFailureParams,
		IsPublicationConflict: cfg.IsPublicationConflict,
		IsCompositionNotReady: cfg.IsCompositionNotReady,
		OnRoleExecuting:       cfg.OnRoleExecuting,
		OnRoleExecuted:        cfg.OnRoleExecuted,
		OnRolePublished:       cfg.OnRolePublished,
		OnCompositionDone:     cfg.OnCompositionDone,
	}

	return SuperviseSession(ctx, sessCfg)
}
