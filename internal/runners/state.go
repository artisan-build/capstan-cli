package runners

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	path string
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
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("runner state lock %q is already held", path)
		}

		return nil, fmt.Errorf("acquire runner state lock %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)

		return nil, fmt.Errorf("close runner state lock %q: %w", path, err)
	}

	return &StateLock{path: path}, nil
}

// Release removes an acquired state lock.
func (l *StateLock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	if err := os.Remove(l.path); err != nil {
		return fmt.Errorf("release runner state lock %q: %w", l.path, err)
	}
	l.path = ""

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
