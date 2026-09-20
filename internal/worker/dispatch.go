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
)

const (
	// MaxInFlight is the maximum number of roles that may be concurrently in_flight for a session.
	MaxInFlight = 2

	// Canonical dispatch no-work reasons matching persistence.
	DispatchNoWorkCancelled     = "cancelled"
	DispatchNoWorkCutoff        = "cutoff"
	DispatchNoWorkCapacityFull  = "capacity_full"
	DispatchNoWorkNoPendingRole = "no_pending_role"
	DispatchNoWorkNotReviewing  = "not_reviewing"
	DispatchNoWorkGuardConflict = "guard_conflict"
)

var (
	// ErrInvalidSessionID is returned when the session ID is empty or whitespace.
	ErrInvalidSessionID = errors.New("worker: invalid session ID")

	// ErrDispatcherClosed is returned when an operation is attempted on a closed dispatcher.
	ErrDispatcherClosed = errors.New("worker: dispatcher is closed")
)

// RoleReservationStore abstracts atomic role reservation against persistence.
// Directly satisfied by *sqlite.Store without requiring internal/worker to import storage packages.
type RoleReservationStore[TReservation any] interface {
	ReservePendingRole(ctx context.Context, sessionID string) (TReservation, error)
}

// SessionStatusReader abstracts optional fast-path read queries for session status.
type SessionStatusReader[TStatus any] interface {
	ReadStatus(ctx context.Context, sessionID string) (TStatus, error)
}

// DispatchConfig configures the serialized guarded dispatch loop.
type DispatchConfig[TReservation any, TStatus any] struct {
	SessionID    string
	Store        RoleReservationStore[TReservation]
	StatusReader SessionStatusReader[TStatus]
	TickInterval time.Duration
	OnReserved   func(ctx context.Context, reservation TReservation) error
	Clock        func() time.Time
}

// DispatchTraceEntry records an individual reservation decision or fast-path check.
type DispatchTraceEntry struct {
	Timestamp    time.Time
	FastPath     bool
	Reserved     bool
	Role         domain.Role
	RoleRunID    string
	NoWorkReason string
	InFlight     int
}

// Dispatcher coordinates the serialized guarded dispatch loop for a single reviewing session.
// All reservation decisions execute strictly on one owner goroutine running Run().
type Dispatcher[TReservation any, TStatus any] struct {
	cfg          DispatchConfig[TReservation, TStatus]
	clock        func() time.Time
	wakeCh       chan struct{}
	tickCh       chan struct{}
	compCh       chan domain.Role
	stopCh       chan struct{}
	stopped      atomic.Bool
	ownerRunning atomic.Bool

	mu            sync.Mutex
	localInFlight int
	trace         []DispatchTraceEntry
}

// NewDispatcher constructs a Dispatcher for the specified claimed reviewing session.
func NewDispatcher[TReservation any, TStatus any](cfg DispatchConfig[TReservation, TStatus]) (*Dispatcher[TReservation, TStatus], error) {
	if strings.TrimSpace(cfg.SessionID) == "" {
		return nil, ErrInvalidSessionID
	}
	if isNil(cfg.Store) {
		return nil, ErrNilStore
	}

	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	statusReader := cfg.StatusReader
	if isNil(statusReader) {
		if sr, ok := any(cfg.Store).(SessionStatusReader[TStatus]); ok {
			statusReader = sr
		}
	}

	cfg.StatusReader = statusReader

	return &Dispatcher[TReservation, TStatus]{
		cfg:    cfg,
		clock:  clock,
		wakeCh: make(chan struct{}, 1),
		tickCh: make(chan struct{}, 1),
		compCh: make(chan domain.Role, 8),
		stopCh: make(chan struct{}),
		trace:  make([]DispatchTraceEntry, 0, 16),
	}, nil
}

// Wake signals the owner loop to wake and evaluate role dispatch.
// It is non-blocking and never invokes ReservePendingRole directly.
func (d *Dispatcher[TReservation, TStatus]) Wake() {
	if d.stopped.Load() {
		return
	}
	select {
	case d.wakeCh <- struct{}{}:
	default:
	}
}

// Tick signals a periodic timer tick to the owner loop.
// It is non-blocking and never invokes ReservePendingRole directly.
func (d *Dispatcher[TReservation, TStatus]) Tick() {
	if d.stopped.Load() {
		return
	}
	select {
	case d.tickCh <- struct{}{}:
	default:
	}
}

// NotifyRoleCompleted informs the dispatcher that an in-flight role completed,
// decrements the local execution slot count, and signals the owner loop.
// It never invokes ReservePendingRole directly.
func (d *Dispatcher[TReservation, TStatus]) NotifyRoleCompleted(role domain.Role) {
	d.mu.Lock()
	if d.localInFlight > 0 {
		d.localInFlight--
	}
	d.mu.Unlock()

	if d.stopped.Load() {
		return
	}
	select {
	case d.compCh <- role:
	default:
	}
}

// Stop requests the dispatch loop to shut down cleanly.
func (d *Dispatcher[TReservation, TStatus]) Stop() {
	if d.stopped.CompareAndSwap(false, true) {
		close(d.stopCh)
	}
}

// LocalInFlight returns the currently tracked local in-flight role count.
func (d *Dispatcher[TReservation, TStatus]) LocalInFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.localInFlight
}

// Trace returns a thread-safe copy of all dispatch events and decisions.
func (d *Dispatcher[TReservation, TStatus]) Trace() []DispatchTraceEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	copied := make([]DispatchTraceEntry, len(d.trace))
	copy(copied, d.trace)
	return copied
}

func (d *Dispatcher[TReservation, TStatus]) recordTrace(entry DispatchTraceEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.trace = append(d.trace, entry)
}

type reservationInfo struct {
	Reserved      bool
	NoWorkReason  string
	Role          domain.Role
	RoleRunID     string
	InFlightCount int
}

func inspectReservation(res any) reservationInfo {
	if res == nil {
		return reservationInfo{}
	}
	v := reflect.ValueOf(res)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reservationInfo{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reservationInfo{}
	}

	var info reservationInfo
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
	return info
}

type fastStatusInfo struct {
	Valid           bool
	CancelRequested bool
	CutoffAt        *time.Time
	HasPendingRole  bool
}

func inspectStatus(st any) fastStatusInfo {
	if st == nil {
		return fastStatusInfo{}
	}
	v := reflect.ValueOf(st)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return fastStatusInfo{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return fastStatusInfo{}
	}

	info := fastStatusInfo{Valid: true, HasPendingRole: true}
	if f := v.FieldByName("CancelRequested"); f.IsValid() && f.Kind() == reflect.Bool {
		info.CancelRequested = f.Bool()
	}
	if f := v.FieldByName("DispatchCutoffAt"); f.IsValid() {
		if !f.IsNil() {
			if t, ok := f.Interface().(*time.Time); ok {
				info.CutoffAt = t
			}
		}
	}
	if f := v.FieldByName("Roles"); f.IsValid() && f.Kind() == reflect.Slice {
		hasPending := false
		for i := 0; i < f.Len(); i++ {
			elem := f.Index(i)
			if elem.Kind() == reflect.Struct {
				if sf := elem.FieldByName("Status"); sf.IsValid() {
					if rStatus, ok := sf.Interface().(domain.RoleStatus); ok && rStatus == domain.RolePending {
						hasPending = true
						break
					}
				}
			}
		}
		info.HasPendingRole = hasPending
	}
	return info
}

// step evaluates fast-path guards, and if eligible, performs one authoritative reservation.
// Step evaluates fast-path guards, and if eligible, performs one authoritative reservation.
// Returns (true, nil) if a role was successfully reserved and committed.
func (d *Dispatcher[TReservation, TStatus]) Step(ctx context.Context) (bool, error) {
	now := d.clock().UTC()

	// 1. Fast-path state evaluation in canonical order:
	//    cancellation -> dispatch cutoff -> local execution capacity -> next pending role.
	if !isNil(d.cfg.StatusReader) {
		statusObj, err := d.cfg.StatusReader.ReadStatus(ctx, d.cfg.SessionID)
		if err == nil {
			statusInfo := inspectStatus(statusObj)
			if statusInfo.Valid {
				if statusInfo.CancelRequested {
					d.recordTrace(DispatchTraceEntry{
						Timestamp:    now,
						FastPath:     true,
						NoWorkReason: DispatchNoWorkCancelled,
					})
					return false, nil
				}
				if statusInfo.CutoffAt != nil && !now.Before(*statusInfo.CutoffAt) {
					d.recordTrace(DispatchTraceEntry{
						Timestamp:    now,
						FastPath:     true,
						NoWorkReason: DispatchNoWorkCutoff,
					})
					return false, nil
				}
				if !statusInfo.HasPendingRole {
					d.recordTrace(DispatchTraceEntry{
						Timestamp:    now,
						FastPath:     true,
						NoWorkReason: DispatchNoWorkNoPendingRole,
					})
					return false, nil
				}
			}
		}
	}

	// Fast-path local execution capacity check:
	d.mu.Lock()
	localInFlight := d.localInFlight
	d.mu.Unlock()

	if localInFlight >= MaxInFlight {
		d.recordTrace(DispatchTraceEntry{
			Timestamp:    now,
			FastPath:     true,
			NoWorkReason: DispatchNoWorkCapacityFull,
			InFlight:     localInFlight,
		})
		return false, nil
	}

	// 2. Authoritative guarded reservation in SQLite.
	res, err := d.cfg.Store.ReservePendingRole(ctx, d.cfg.SessionID)
	if err != nil {
		return false, err
	}

	info := inspectReservation(res)
	if !info.Reserved {
		d.recordTrace(DispatchTraceEntry{
			Timestamp:    now,
			FastPath:     false,
			Reserved:     false,
			NoWorkReason: info.NoWorkReason,
			InFlight:     info.InFlightCount,
		})
		return false, nil
	}

	// Reservation succeeded and committed.
	d.mu.Lock()
	d.localInFlight++
	currInFlight := d.localInFlight
	d.mu.Unlock()

	d.recordTrace(DispatchTraceEntry{
		Timestamp: now,
		FastPath:  false,
		Reserved:  true,
		Role:      info.Role,
		RoleRunID: info.RoleRunID,
		InFlight:  currInFlight,
	})

	// 3. Invoke post-commit callback AFTER reservation transaction has committed.
	if d.cfg.OnReserved != nil {
		_ = d.cfg.OnReserved(ctx, res)
	}

	return true, nil
}

// Run executes the serialized dispatch loop on the calling goroutine.
// Only this goroutine ever calls ReservePendingRole. It loops until context cancellation,
// an explicit Stop() call, or an unrecoverable persistence error.
func (d *Dispatcher[TReservation, TStatus]) Run(ctx context.Context) error {
	if !d.ownerRunning.CompareAndSwap(false, true) {
		return errors.New("worker: dispatcher owner loop is already running")
	}
	defer d.ownerRunning.Store(false)

	var ticker *time.Ticker
	var tickChannel <-chan time.Time
	if d.cfg.TickInterval > 0 {
		ticker = time.NewTicker(d.cfg.TickInterval)
		defer ticker.Stop()
		tickChannel = ticker.C
	}

	// Initial dispatch burst: fill available capacity immediately upon start
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.stopped.Load() {
			return nil
		}
		reserved, err := d.Step(ctx)
		if err != nil {
			return err
		}
		if !reserved {
			break
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-d.stopCh:
			return nil

		case <-tickChannel:
			// Timer tick: drain capacity
			for {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if d.stopped.Load() {
					return nil
				}
				reserved, err := d.Step(ctx)
				if err != nil {
					return err
				}
				if !reserved {
					break
				}
			}

		case <-d.tickCh:
			// Manual tick: drain capacity
			for {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if d.stopped.Load() {
					return nil
				}
				reserved, err := d.Step(ctx)
				if err != nil {
					return err
				}
				if !reserved {
					break
				}
			}

		case <-d.wakeCh:
			// External wake signal: drain capacity
			for {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if d.stopped.Load() {
					return nil
				}
				reserved, err := d.Step(ctx)
				if err != nil {
					return err
				}
				if !reserved {
					break
				}
			}

		case <-d.compCh:
			// Role completion notification: slot freed, drain capacity
			for {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if d.stopped.Load() {
					return nil
				}
				reserved, err := d.Step(ctx)
				if err != nil {
					return err
				}
				if !reserved {
					break
				}
			}
		}
	}
}
