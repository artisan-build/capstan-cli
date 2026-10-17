//go:build unix

package runners

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
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
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
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
