//go:build windows

package work

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// AcquireLock takes a non-blocking exclusive lock.
func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, errors.New("work lock directory could not be created")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errors.New("work lock could not be opened")
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLockHeld
		}
		return nil, errors.New("work lock could not be acquired")
	}

	return &Lock{file: file}, nil
}

// Release unlocks and closes the descriptor.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	var overlapped windows.Overlapped
	unlockErr := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &overlapped)
	closeErr := l.file.Close()
	l.file = nil

	return errors.Join(unlockErr, closeErr)
}
