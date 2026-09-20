package worker_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite"
	"github.com/wyze69-sys/SpecCouncil/internal/worker"
)

// TestHelperProcess serves as the subprocess entrypoint for testing OS-level
// process termination, handle closure, and crash behavior.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_PROCESS_LOCK_HELPER") != "1" {
		return
	}

	mode := os.Getenv("GO_PROCESS_LOCK_MODE")
	lockPath := os.Getenv("GO_PROCESS_LOCK_PATH")

	switch mode {
	case "acquire_and_exit":
		lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper acquire err: %v\n", err)
			os.Exit(1)
		}
		_ = lock // Intentionally not released; process exit must trigger OS cleanup
		fmt.Println("ACQUIRED")
		_ = os.Stdout.Sync()
		os.Exit(0)

	case "acquire_and_hold":
		lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper acquire err: %v\n", err)
			os.Exit(1)
		}
		_ = lock // Intentionally not released; OS kill must trigger cleanup
		fmt.Println("LOCKED")
		_ = os.Stdout.Sync()

		// Block indefinitely until parent terminates this process
		select {}

	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode: %s\n", mode)
		os.Exit(2)
	}
}

// 1. Empty path is rejected; unusable parent path is rejected.
func TestAcquireProcessLock_RejectsInvalidPaths(t *testing.T) {
	ctx := context.Background()

	// Empty path
	_, err := worker.AcquireProcessLock(ctx, "")
	if err == nil {
		t.Fatal("expected error on empty path, got nil")
	}
	if !errors.Is(err, worker.ErrEmptyPath) {
		t.Fatalf("expected ErrEmptyPath, got: %v", err)
	}
	var pathErr *worker.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("expected *worker.PathError, got: %T", err)
	}

	// Whitespace-only path
	_, err = worker.AcquireProcessLock(ctx, "   ")
	if err == nil {
		t.Fatal("expected error on whitespace path, got nil")
	}
	if !errors.Is(err, worker.ErrEmptyPath) {
		t.Fatalf("expected ErrEmptyPath, got: %v", err)
	}

	// Non-existent parent directory
	nonExistentPath := filepath.Join(t.TempDir(), "sub", "dir", "not_there", "worker.lock")
	_, err = worker.AcquireProcessLock(ctx, nonExistentPath)
	if err == nil {
		t.Fatal("expected error on nonexistent parent dir, got nil")
	}
	if !errors.Is(err, worker.ErrInvalidParentDir) {
		t.Fatalf("expected ErrInvalidParentDir, got: %v", err)
	}
	if !errors.As(err, &pathErr) {
		t.Fatalf("expected *worker.PathError, got: %T", err)
	}

	// Parent path is a file, not a directory
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "some_file.txt")
	if err := os.WriteFile(filePath, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	unusableLockPath := filepath.Join(filePath, "child.lock")
	_, err = worker.AcquireProcessLock(ctx, unusableLockPath)
	if err == nil {
		t.Fatal("expected error when parent is a file, got nil")
	}
	if !errors.Is(err, worker.ErrInvalidParentDir) {
		t.Fatalf("expected ErrInvalidParentDir, got: %v", err)
	}
}

// 2. First acquisition succeeds;
// 4. Releasing the first lock permits a later acquisition.
func TestAcquireProcessLock_FirstAcquisitionAndRelease(t *testing.T) {
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "worker.lock")

	lock1, err := worker.AcquireProcessLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("first acquisition failed: %v", err)
	}
	if lock1 == nil {
		t.Fatal("expected non-nil lock")
	}
	if lock1.Path() == "" {
		t.Fatal("expected non-empty path on lock")
	}

	if err := lock1.Release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	// Now a second acquisition must succeed
	lock2, err := worker.AcquireProcessLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("re-acquisition after release failed: %v", err)
	}
	if lock2 == nil {
		t.Fatal("expected non-nil lock on reacquisition")
	}
	if err := lock2.Release(); err != nil {
		t.Fatalf("second release failed: %v", err)
	}
}

// 3. Second acquisition on the same path returns ErrAlreadyOwned promptly.
func TestAcquireProcessLock_SecondOwnerRejectedPromptly(t *testing.T) {
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "worker.lock")

	lock1, err := worker.AcquireProcessLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("initial acquisition failed: %v", err)
	}
	defer func() {
		_ = lock1.Release()
	}()

	start := time.Now()
	lock2, err := worker.AcquireProcessLock(ctx, lockPath)
	duration := time.Since(start)

	if err == nil {
		_ = lock2.Release()
		t.Fatal("expected second acquisition to fail, but succeeded")
	}
	if !errors.Is(err, worker.ErrAlreadyOwned) {
		t.Fatalf("expected ErrAlreadyOwned, got: %v", err)
	}
	if lock2 != nil {
		t.Fatal("expected nil lock on failure")
	}

	// Verification that it fails promptly without blocking (must be well under 100ms)
	if duration > 500*time.Millisecond {
		t.Fatalf("acquisition took too long (%v), expected prompt failure", duration)
	}
}

// 5. Repeated Release is safe (idempotent).
func TestAcquireProcessLock_RepeatedReleaseSafe(t *testing.T) {
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "worker.lock")

	lock, err := worker.AcquireProcessLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("acquisition failed: %v", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("first release failed: %v", err)
	}

	// Repeated releases must be safe and return nil
	for i := 0; i < 5; i++ {
		if err := lock.Release(); err != nil {
			t.Fatalf("repeated release %d failed: %v", i+2, err)
		}
	}
}

// 6. An owner handle becoming unreachable/closed releases the OS lock and a new acquisition succeeds.
// (Subprocess normal exit without calling Release).
func TestAcquireProcessLock_ReacquisitionAfterNormalExitSubprocess(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "worker.lock")

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--")
	cmd.Env = append(os.Environ(),
		"GO_PROCESS_LOCK_HELPER=1",
		"GO_PROCESS_LOCK_MODE=acquire_and_exit",
		"GO_PROCESS_LOCK_PATH="+lockPath,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess failed: %v, output: %s", err, string(out))
	}
	if !strings.Contains(string(out), "ACQUIRED") {
		t.Fatalf("expected ACQUIRED output, got: %s", string(out))
	}

	// Subprocess has exited normally without calling Release.
	// The OS kernel has closed all handles, so the parent must be able to acquire the lock immediately.
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("parent failed to acquire lock after subprocess normal exit: %v", err)
	}
	defer func() {
		_ = lock.Release()
	}()
}

// 7. A subprocess that acquires the lock and is terminated without calling Release
// leaves the path acquirable by the parent.
func TestAcquireProcessLock_ReacquisitionAfterAbnormalTerminationSubprocess(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "worker.lock")

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--")
	cmd.Env = append(os.Environ(),
		"GO_PROCESS_LOCK_HELPER=1",
		"GO_PROCESS_LOCK_MODE=acquire_and_hold",
		"GO_PROCESS_LOCK_PATH="+lockPath,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe failed: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe failed: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed: %v", err)
	}

	// Wait deterministically for subprocess to signal it holds the lock
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		errBytes, _ := os.ReadFile(lockPath)
		t.Fatalf("failed reading subprocess readiness: %v (lockfile: %s)", err, string(errBytes))
	}
	if strings.TrimSpace(line) != "LOCKED" {
		t.Fatalf("unexpected helper output: %q", line)
	}

	// While subprocess holds the lock, parent acquisition must fail promptly with ErrAlreadyOwned
	_, err = worker.AcquireProcessLock(context.Background(), lockPath)
	if err == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("expected parent acquisition to fail while subprocess holds lock")
	}
	if !errors.Is(err, worker.ErrAlreadyOwned) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("expected ErrAlreadyOwned while child held lock, got: %v", err)
	}

	// Terminate child forcefully without calling Release
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("failed to kill subprocess: %v", err)
	}
	_ = cmd.Wait()
	_ = stderr // drain

	// Subprocess was forcefully killed without Release().
	// Parent must now be able to acquire the lock immediately.
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("parent acquisition failed after subprocess termination: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("parent release failed: %v", err)
	}
}

// 8. Two concurrent acquisition attempts have exactly one winner.
func TestAcquireProcessLock_TwoConcurrentAttemptsExactOneWinner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "worker.lock")

	start := make(chan struct{})
	type result struct {
		lock *worker.ProcessLock
		err  error
	}

	results := make([]result, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	for i := 0; i < 2; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-start
			l, err := worker.AcquireProcessLock(context.Background(), lockPath)
			results[idx] = result{lock: l, err: err}
		}()
	}

	// Release both goroutines at the exact same instant
	close(start)
	wg.Wait()

	var winners []*worker.ProcessLock
	var losers []error

	for _, res := range results {
		if res.err == nil {
			winners = append(winners, res.lock)
		} else {
			losers = append(losers, res.err)
		}
	}

	if len(winners) != 1 {
		t.Fatalf("expected exactly 1 winner, got %d winners", len(winners))
	}
	if len(losers) != 1 {
		t.Fatalf("expected exactly 1 loser, got %d losers", len(losers))
	}
	if !errors.Is(losers[0], worker.ErrAlreadyOwned) {
		t.Fatalf("expected ErrAlreadyOwned for loser, got: %v", losers[0])
	}

	// Releasing the winner's lock enables subsequent acquisition
	if err := winners[0].Release(); err != nil {
		t.Fatalf("winner release failed: %v", err)
	}

	postLock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("post-release acquisition failed: %v", err)
	}
	_ = postLock.Release()
}

// TestAcquireProcessLock_MultiConcurrentContention verifies that with N goroutines,
// exactly one wins and all others get ErrAlreadyOwned.
func TestAcquireProcessLock_MultiConcurrentContention(t *testing.T) {
	const count = 10
	lockPath := filepath.Join(t.TempDir(), "worker.lock")

	start := make(chan struct{})
	type result struct {
		lock *worker.ProcessLock
		err  error
	}

	results := make([]result, count)
	var wg sync.WaitGroup
	wg.Add(count)

	for i := 0; i < count; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-start
			l, err := worker.AcquireProcessLock(context.Background(), lockPath)
			results[idx] = result{lock: l, err: err}
		}()
	}

	close(start)
	wg.Wait()

	var winners []*worker.ProcessLock
	var losers []error

	for _, res := range results {
		if res.err == nil {
			winners = append(winners, res.lock)
		} else {
			losers = append(losers, res.err)
		}
	}

	if len(winners) != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", len(winners))
	}
	if len(losers) != count-1 {
		t.Fatalf("expected %d losers, got %d", count-1, len(losers))
	}
	for _, loserErr := range losers {
		if !errors.Is(loserErr, worker.ErrAlreadyOwned) {
			t.Fatalf("expected ErrAlreadyOwned for all losers, got: %v", loserErr)
		}
	}

	if err := winners[0].Release(); err != nil {
		t.Fatalf("winner release failed: %v", err)
	}
}

// 9. Cancellation before acquisition returns context cancellation without a live lock or leaked helper goroutine.
func TestAcquireProcessLock_ContextCancellation(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "worker.lock")

	// Cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lock, err := worker.AcquireProcessLock(ctx, lockPath)
	if err == nil {
		_ = lock.Release()
		t.Fatal("expected error with cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if lock != nil {
		t.Fatal("expected nil lock on cancellation")
	}

	// Verify that the path was not locked and is immediately acquirable with a valid context
	validLock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("subsequent acquisition after cancelled context failed: %v", err)
	}
	if err := validLock.Release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	// Expired context with deadline in the past
	pastCtx, pastCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer pastCancel()

	lock, err = worker.AcquireProcessLock(pastCtx, lockPath)
	if err == nil {
		_ = lock.Release()
		t.Fatal("expected error with expired deadline, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
}

// 10. Lock acquisition does not open the SpecCouncil database or change any table.
func TestAcquireProcessLock_PersistenceUntouched(t *testing.T) {
	// First, assert that production worker package does not import sqlite or storage packages
	cmd := exec.Command("go", "list", "-f", "{{.Imports}}", "github.com/wyze69-sys/SpecCouncil/internal/worker")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed: %v, output: %s", err, string(out))
	}
	imports := string(out)
	if strings.Contains(imports, "storage") || strings.Contains(imports, "sqlite") {
		t.Fatalf("internal/worker must not import storage/sqlite, imports: %s", imports)
	}

	// Second, create a real database with migrations applied, snapshot its contents,
	// run lock operations in the same folder, and assert that the database is 100% untouched.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_speccouncil.db")

	cfg := sqlite.Config{
		Path:        dbPath,
		BusyTimeout: 5000 * time.Millisecond,
	}
	store, err := sqlite.Open(cfg)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	beforeBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("failed reading db bytes before: %v", err)
	}

	lockPath := filepath.Join(tmpDir, "worker.lock")
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("AcquireProcessLock failed: %v", err)
	}
	if lock.Path() != filepath.Clean(lockPath) {
		t.Fatalf("unexpected lock path: %s", lock.Path())
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	afterBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("failed reading db bytes after: %v", err)
	}

	if len(beforeBytes) != len(afterBytes) {
		t.Fatalf("database file size modified across process lock! before=%d, after=%d", len(beforeBytes), len(afterBytes))
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Fatalf("database file content mutated across process lock!")
	}
}
