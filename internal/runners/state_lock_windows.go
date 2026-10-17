//go:build windows

package runners

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const (
	windowsLockLow  = 1
	windowsLockHigh = 0
)

// AcquireLock exclusively locks the state directory. Contention is an
// operational failure rather than a runner health finding.
func (s *StateStore) AcquireLock() (*StateLock, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create runner state directory %q: %w", s.dir, err)
	}

	path := filepath.Join(s.dir, lockFilename)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runner state lock %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("secure runner state lock %q: %w", path, err)
	}

	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		windowsLockLow,
		windowsLockHigh,
		&overlapped,
	)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("runner state lock %q is already held", path)
		}

		return nil, fmt.Errorf("acquire runner state lock %q: %w", path, err)
	}

	return &StateLock{file: file}, nil
}

// Release unlocks and closes an acquired state lock descriptor. The harmless
// lock file remains; process death also releases the descriptor automatically.
func (l *StateLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}

	path := l.file.Name()
	var overlapped windows.Overlapped
	unlockErr := windows.UnlockFileEx(
		windows.Handle(l.file.Fd()),
		0,
		windowsLockLow,
		windowsLockHigh,
		&overlapped,
	)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil || closeErr != nil {
		return errors.Join(
			wrapOptionalError(unlockErr, "unlock runner state lock %q", path),
			wrapOptionalError(closeErr, "close runner state lock %q", path),
		)
	}

	return nil
}
