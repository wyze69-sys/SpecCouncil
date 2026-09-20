//go:build !windows

package worker

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type osLock struct {
	file *os.File
}

func acquireOSLock(path string) (*osLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrAlreadyOwned
		}
		return nil, err
	}

	return &osLock{file: f}, nil
}

func (l *osLock) close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
