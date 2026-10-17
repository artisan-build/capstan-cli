package runners

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	stateFilename = "runners.json"
	lockFilename  = "runners.lock"
)

// State is the persisted observation baseline for configured runners.
type State struct {
	Runners map[string]RunnerState `json:"runners"`
}

// RunnerState records the last observed launchd activity for one runner.
type RunnerState struct {
	Runs          int       `json:"runs"`
	RunsChangedAt time.Time `json:"runs_changed_at"`
	FirstSeenAt   time.Time `json:"first_seen_at"`
}

// StateStore persists runner observation state beneath a configured state path.
type StateStore struct {
	dir string
}

// StateLock is an exclusive lease for a state load-modify-save cycle.
type StateLock struct {
	file *os.File
}

// NewStateStore returns a state store rooted at path.
func NewStateStore(path string) *StateStore {
	return &StateStore{dir: path}
}

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

// Load reads runner observation state. A missing state file is an empty cold start.
func (s *StateStore) Load() (State, error) {
	path := filepath.Join(s.dir, stateFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyState(), nil
		}

		return State{}, fmt.Errorf("read runner state %q: %w", path, err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode runner state %q: %w", path, err)
	}
	if state.Runners == nil {
		state.Runners = make(map[string]RunnerState)
	}

	return state, nil
}

// Save atomically writes runner observation state with user-only permissions.
func (s *StateStore) Save(state State) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create runner state directory %q: %w", s.dir, err)
	}
	if err := removeTemporaryFiles(s.dir, ".runners-*"); err != nil {
		return fmt.Errorf("clean stale runner state files: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runner state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dir, ".runners-*")
	if err != nil {
		return fmt.Errorf("create temporary runner state: %w", err)
	}

	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("secure temporary runner state: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write temporary runner state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary runner state: %w", err)
	}

	path := filepath.Join(s.dir, stateFilename)
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace runner state %q: %w", path, err)
	}
	committed = true

	return nil
}

func emptyState() State {
	return State{Runners: make(map[string]RunnerState)}
}

func removeTemporaryFiles(dir, pattern string) error {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return fmt.Errorf("match temporary files: %w", err)
	}
	for _, path := range matches {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect temporary file %q: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove temporary file %q: %w", path, err)
		}
	}

	return nil
}

func wrapOptionalError(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf(format+": %w", append(args, err)...)
}
