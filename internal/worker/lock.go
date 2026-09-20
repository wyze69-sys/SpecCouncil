package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	// ErrEmptyPath is returned when the provided lock path is empty.
	ErrEmptyPath = errors.New("worker: lock path cannot be empty")

	// ErrInvalidParentDir is returned when the parent directory of the lock file is invalid or does not exist.
	ErrInvalidParentDir = errors.New("worker: invalid lock parent directory")

	// ErrAlreadyOwned is returned promptly when another process or handle holds the exclusive lock.
	ErrAlreadyOwned = errors.New("worker: process lock is already held")
)

// PathError records an error and the operation and file path that caused it.
type PathError struct {
	Op   string
	Path string
	Err  error
}

func (e *PathError) Error() string {
	return fmt.Sprintf("worker: %s %q: %v", e.Op, e.Path, e.Err)
}

func (e *PathError) Unwrap() error {
	return e.Err
}

// ProcessLock represents an OS-backed exclusive process lock tied to an open OS file handle.
// When the owning process terminates normally or crashes, the operating system kernel
// automatically reclaims the handle and releases the lock.
type ProcessLock struct {
	path     string
	raw      *osLock
	mu       sync.Mutex
	released bool
}

// Path returns the canonical path of the lock file.
func (l *ProcessLock) Path() string {
	return l.path
}

// Release idempotently releases the OS-backed exclusive lock and closes the underlying OS handle.
// It does not remove unrelated files.
func (l *ProcessLock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.released {
		return nil
	}
	l.released = true

	if l.raw != nil {
		err := l.raw.close()
		l.raw = nil
		return err
	}
	return nil
}

// AcquireProcessLock acquires an exclusive OS-backed lock for the specified file path.
// It validates the path and its parent directory, honors context cancellation,
// and acquires the lock handle tied to the current process.
// If the lock is already held by another process or handle, it returns ErrAlreadyOwned promptly.
func AcquireProcessLock(ctx context.Context, path string) (*ProcessLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if strings.TrimSpace(path) == "" {
		return nil, &PathError{
			Op:   "validate_path",
			Path: path,
			Err:  ErrEmptyPath,
		}
	}

	cleanPath := filepath.Clean(path)
	absPath, err := filepath.Abs(cleanPath)
	if err != nil {
		return nil, &PathError{
			Op:   "abs_path",
			Path: cleanPath,
			Err:  err,
		}
	}

	parentDir := filepath.Dir(absPath)
	fi, err := os.Stat(parentDir)
	if err != nil {
		return nil, &PathError{
			Op:   "stat_parent_dir",
			Path: parentDir,
			Err:  fmt.Errorf("%w: %v", ErrInvalidParentDir, err),
		}
	}
	if !fi.IsDir() {
		return nil, &PathError{
			Op:   "validate_parent_dir",
			Path: parentDir,
			Err:  fmt.Errorf("%w: parent path is not a directory", ErrInvalidParentDir),
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rawLock, err := acquireOSLock(absPath)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		_ = rawLock.close()
		return nil, err
	}

	return &ProcessLock{
		path: absPath,
		raw:  rawLock,
	}, nil
}
