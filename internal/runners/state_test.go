package runners

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStateStoreRoundTrip(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "state")
	store := NewStateStore(dir)
	want := State{Runners: map[string]RunnerState{
		"scheduled": {
			Runs:          19,
			RunsChangedAt: time.Date(2026, 8, 18, 9, 30, 0, 123, time.UTC),
			FirstSeenAt:   time.Date(2026, 8, 1, 10, 0, 0, 456, time.UTC),
		},
		"removed-from-config": {
			Runs:          2,
			RunsChangedAt: time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC),
			FirstSeenAt:   time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC),
		},
	}}

	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() = %#v, want %#v", got, want)
	}
}

func TestStateStoreMissingFileIsColdStart(t *testing.T) {
	t.Parallel()

	got, err := NewStateStore(t.TempDir()).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := State{Runners: map[string]RunnerState{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() = %#v, want %#v", got, want)
	}
}

func TestStateStoreAtomicWriteAndPermissions(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "state")
	store := NewStateStore(dir)
	state := State{Runners: map[string]RunnerState{
		"runner": {Runs: 1, RunsChangedAt: time.Now(), FirstSeenAt: time.Now()},
	}}

	if err := store.Save(state); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}
	state.Runners["runner"] = RunnerState{Runs: 2, RunsChangedAt: time.Now(), FirstSeenAt: time.Now()}
	if err := store.Save(state); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	wantEntries := []string{"runners.json"}
	gotEntries := make([]string, len(entries))
	for i, entry := range entries {
		gotEntries[i] = entry.Name()
	}
	if !reflect.DeepEqual(gotEntries, wantEntries) {
		t.Errorf("state directory entries = %#v, want %#v", gotEntries, wantEntries)
	}

	fileInfo, err := os.Stat(filepath.Join(dir, "runners.json"))
	if err != nil {
		t.Fatalf("Stat(state file) error = %v", err)
	}
	if got, want := fileInfo.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("state file mode = %o, want %o", got, want)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(state directory) error = %v", err)
	}
	if got, want := dirInfo.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Errorf("state directory mode = %o, want %o", got, want)
	}
}

func TestStateStorePreservesPreExistingDirectoryPermissions(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "shared-state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	if err := NewStateStore(dir).Save(emptyState()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o755); got != want {
		t.Errorf("state directory mode = %o, want %o", got, want)
	}
}

func TestStateStoreLoadInitializesMissingRunnersMap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFilename), []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := NewStateStore(dir).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := State{Runners: map[string]RunnerState{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() = %#v, want %#v", got, want)
	}
}

func TestStateStoreCleansTempFileAfterRenameFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	destination := filepath.Join(dir, stateFilename)
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatalf("Mkdir(destination) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "keep"), []byte("occupied"), 0o600); err != nil {
		t.Fatalf("WriteFile(destination) error = %v", err)
	}

	if err := NewStateStore(dir).Save(emptyState()); err == nil {
		t.Fatal("Save() error = nil, want rename failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	wantEntries := []string{stateFilename}
	gotEntries := make([]string, len(entries))
	for i, entry := range entries {
		gotEntries[i] = entry.Name()
	}
	if !reflect.DeepEqual(gotEntries, wantEntries) {
		t.Errorf("state directory entries = %#v, want %#v", gotEntries, wantEntries)
	}
}

func TestStateStoreIgnoresExtraFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data := []byte(`{
  "format_version": 99,
  "runners": {
    "runner": {
      "runs": 5,
      "runs_changed_at": "2026-08-18T09:30:00Z",
      "first_seen_at": "2026-08-01T10:00:00Z",
      "future_field": "ignored"
    }
  }
}`)
	if err := os.WriteFile(filepath.Join(dir, stateFilename), data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := NewStateStore(dir).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := State{Runners: map[string]RunnerState{
		"runner": {
			Runs:          5,
			RunsChangedAt: time.Date(2026, 8, 18, 9, 30, 0, 0, time.UTC),
			FirstSeenAt:   time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() = %#v, want %#v", got, want)
	}
}
