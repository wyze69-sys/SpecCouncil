//go:build windows

package worker

import (
	"errors"

	"golang.org/x/sys/windows"
)

type osLock struct {
	handle windows.Handle
}

func acquireOSLock(path string) (*osLock, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	// Open or create the lock file with shareMode = 0 (exclusive access).
	// This prevents any other process or handle from opening the file for read, write, or delete.
	h, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // dwShareMode: 0 = exclusive
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrAlreadyOwned
		}
		return nil, err
	}

	// Additionally take an explicit exclusive byte-range lock via LockFileEx.
	var ol windows.Overlapped
	if err := windows.LockFileEx(
		h,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1, 0,
		&ol,
	); err != nil {
		_ = windows.CloseHandle(h)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, ErrAlreadyOwned
		}
		return nil, err
	}

	return &osLock{handle: h}, nil
}

func (l *osLock) close() error {
	if l == nil || l.handle == windows.InvalidHandle {
		return nil
	}
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(l.handle, 0, 1, 0, &ol)
	err := windows.CloseHandle(l.handle)
	l.handle = windows.InvalidHandle
	return err
}
