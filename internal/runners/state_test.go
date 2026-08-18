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

// TestStateStoreSaveReplacesRatherThanTruncates pins ATOMICITY specifically, which
// TestStateStoreAtomicWriteAndPermissions does not: a plain os.WriteFile satisfies that
// test's "no stray files" and "mode 0600" assertions.
//
// A failed Save must leave the previous state byte-for-byte intact. os.WriteFile would
// truncate the existing file in place and lose it; temp-file-plus-rename cannot, because
// the original is only ever replaced by a completed file.
func TestStateStoreSaveReplacesRatherThanTruncates(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		// root writes into a 0500 directory regardless of the mode bits, so the Save below
		// would succeed and this test could not fail. Skip rather than assert nothing.
		t.Skip("root ignores directory permission bits, so Save cannot be made to fail")
	}

	dir := t.TempDir()
	store := NewStateStore(dir)

	original := State{Runners: map[string]RunnerState{
		"runner": {Runs: 41, RunsChangedAt: time.Unix(1700000000, 0).UTC(), FirstSeenAt: time.Unix(1600000000, 0).UTC()},
	}}
	if err := store.Save(original); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	before, err := os.ReadFile(filepath.Join(dir, "runners.json"))
	if err != nil {
		t.Fatalf("read seeded state: %v", err)
	}

	// Make the directory unwritable so the replace cannot complete.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	updated := State{Runners: map[string]RunnerState{
		"runner": {Runs: 99, RunsChangedAt: time.Unix(1800000000, 0).UTC(), FirstSeenAt: time.Unix(1600000000, 0).UTC()},
	}}
	if err := store.Save(updated); err == nil {
		t.Fatal("Save() into an unwritable directory returned nil error, want failure")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restore directory mode: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "runners.json"))
	if err != nil {
		t.Fatalf("read state after failed Save: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("failed Save() modified existing state\n got: %s\nwant: %s", after, before)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() after failed Save error = %v", err)
	}
	if got := loaded.Runners["runner"].Runs; got != 41 {
		t.Errorf("Runs after failed Save = %d, want 41 (previous state must survive)", got)
	}
}
