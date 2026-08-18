package runners

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFingerprintIsStable(t *testing.T) {
	t.Parallel()

	const want = "29a971336c5736e5f49511bde0395968e7e230bc0db0a3bbe0ca79ec380da5e7"
	if got := Fingerprint("ballast-rescore", FailureStale); got != want {
		t.Fatalf("Fingerprint() = %q, want durable value %q", got, want)
	}
}

func TestInboxWriterStaleFileFormat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 8, 18, 11, 14, 22, 0, time.UTC)
	lastActivity := time.Date(2026, 8, 18, 8, 14, 22, 0, time.UTC)
	runner := Runner{
		Name:               "ballast-rescore",
		Type:               RunnerTypeLaunchd,
		Label:              "build.artisan.ballast-rescore",
		ExpectedCadence:    time.Hour,
		StalenessThreshold: 30 * time.Minute,
	}
	health := Health{Present: true, HasRun: true, Runs: 12}
	state := RunnerState{Runs: 12, RunsChangedAt: lastActivity, FirstSeenAt: lastActivity}
	classification := Classification{Runner: runner.Name, Kind: FailureStale}

	changes, err := NewInboxWriter(dir).Reconcile(runner, classification, health, state, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantChanges := []InboxChange{{Action: "written", Filename: "ballast-rescore-stale-29a97133.md"}}
	if !reflect.DeepEqual(changes, wantChanges) {
		t.Fatalf("Reconcile() changes = %#v, want %#v", changes, wantChanges)
	}

	data, err := os.ReadFile(filepath.Join(dir, wantChanges[0].Filename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	want := `---
fingerprint: 29a971336c5736e5f49511bde0395968e7e230bc0db0a3bbe0ca79ec380da5e7
runner: ballast-rescore
failure_kind: stale
status: open
assignee: ""
first_seen: "2026-08-18T11:14:22Z"
last_seen: "2026-08-18T11:14:22Z"
count: 1
resolved_at: ""
evidence:
    checked: last activity against expected cadence plus staleness threshold
    freshness_source: runs
    run_count: 12
    last_activity: "2026-08-18T08:14:22Z"
    expected_cadence: 1h0m0s
    staleness_threshold: 30m0s
    stale_after: "2026-08-18T09:44:22Z"
---

# Runner ballast-rescore is stale

The runner has not shown real activity within its expected cadence and staleness threshold.

## Evidence
- Checked: last activity against expected cadence plus staleness threshold
- Freshness source: runs
- Run count: 12
- Last activity: 2026-08-18T08:14:22Z
- Expected cadence: 1h0m0s
- Staleness threshold: 30m0s
- Stale after: 2026-08-18T09:44:22Z
`
	if string(data) != want {
		t.Fatalf("inbox file mismatch\n--- got ---\n%s--- want ---\n%s", data, want)
	}
}

func TestInboxWriterDedupeLifecyclePreservesBody(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writer := NewInboxWriter(dir)
	runner := Runner{Name: "worker", Type: RunnerTypeLaunchd, Label: "test.worker"}
	failure := Classification{Runner: runner.Name, Kind: FailureNonzeroExit}
	health := Health{Present: true, HasRun: true, LastExitStatus: 17, Runs: 3}
	state := RunnerState{Runs: 3}
	firstSeen := time.Date(2026, 8, 18, 9, 14, 22, 0, time.UTC)
	secondSeen := firstSeen.Add(2 * time.Hour)
	recoveredAt := secondSeen.Add(time.Hour)
	recurredAt := recoveredAt.Add(time.Hour)
	filename := "worker-nonzero_exit-" + Fingerprint(runner.Name, FailureNonzeroExit)[:8] + ".md"
	path := filepath.Join(dir, filename)

	changes, err := writer.Reconcile(runner, failure, health, state, firstSeen)
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if want := []InboxChange{{Action: "written", Filename: filename}}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("first changes = %#v, want %#v", changes, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read initial item: %v", err)
	}
	frontmatter, _, err := parseInboxFile(data)
	if err != nil {
		t.Fatalf("parse initial item: %v", err)
	}
	humanBody := "\n# Runner worker exited unsuccessfully\n\nHuman triage notes must survive.\n"
	if err := writeInboxFile(dir, path, frontmatter, humanBody); err != nil {
		t.Fatalf("write human-edited body: %v", err)
	}

	health.LastExitStatus = 23
	health.Runs = 4
	changes, err = writer.Reconcile(runner, failure, health, RunnerState{Runs: 4}, secondSeen)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if want := []InboxChange{{Action: "updated", Filename: filename}}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("second changes = %#v, want %#v", changes, want)
	}
	assertInboxState(t, path, inboxFrontmatter{
		Fingerprint: Fingerprint(runner.Name, FailureNonzeroExit),
		Runner:      runner.Name,
		FailureKind: FailureNonzeroExit,
		Status:      inboxStatusOpen,
		FirstSeen:   firstSeen.Format(time.RFC3339),
		LastSeen:    secondSeen.Format(time.RFC3339),
		Count:       2,
		Evidence: inboxEvidence{
			Checked:         "launchd last exit status, exit reason, and run count",
			FreshnessSource: "runs",
			LastExitStatus:  intPointer(23),
			RunCount:        intPointer(4),
		},
	}, humanBody)
	assertOnlyInboxFile(t, dir, filename)

	healthy := Classification{Runner: runner.Name, Kind: FailureHealthy}
	changes, err = writer.Reconcile(runner, healthy, Health{Present: true, HasRun: true, Runs: 5}, RunnerState{Runs: 5}, recoveredAt)
	if err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	if want := []InboxChange{{Action: "resolved", Filename: filename}}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("recovery changes = %#v, want %#v", changes, want)
	}
	wantResolved := inboxFrontmatter{
		Fingerprint: Fingerprint(runner.Name, FailureNonzeroExit),
		Runner:      runner.Name,
		FailureKind: FailureNonzeroExit,
		Status:      inboxStatusResolved,
		FirstSeen:   firstSeen.Format(time.RFC3339),
		LastSeen:    secondSeen.Format(time.RFC3339),
		Count:       2,
		ResolvedAt:  recoveredAt.Format(time.RFC3339),
		Evidence: inboxEvidence{
			Checked:         "launchd last exit status, exit reason, and run count",
			FreshnessSource: "runs",
			LastExitStatus:  intPointer(23),
			RunCount:        intPointer(4),
		},
	}
	assertInboxState(t, path, wantResolved, humanBody)
	assertOnlyInboxFile(t, dir, filename)

	health.LastExitStatus = 9
	health.Runs = 6
	changes, err = writer.Reconcile(runner, failure, health, RunnerState{Runs: 6}, recurredAt)
	if err != nil {
		t.Fatalf("recurrence Reconcile() error = %v", err)
	}
	if want := []InboxChange{{Action: "reopened", Filename: filename}}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("recurrence changes = %#v, want %#v", changes, want)
	}
	wantReopened := wantResolved
	wantReopened.Status = inboxStatusOpen
	wantReopened.LastSeen = recurredAt.Format(time.RFC3339)
	wantReopened.Count = 3
	wantReopened.ResolvedAt = ""
	wantReopened.Evidence.LastExitStatus = intPointer(9)
	wantReopened.Evidence.RunCount = intPointer(6)
	assertInboxState(t, path, wantReopened, humanBody)
	assertOnlyInboxFile(t, dir, filename)
}

func TestInboxWriterEvidenceByFailureKind(t *testing.T) {
	t.Parallel()

	activityPath := filepath.Join(t.TempDir(), "activity")
	activityTime := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	if err := os.WriteFile(activityPath, []byte("activity"), 0o600); err != nil {
		t.Fatalf("write activity source: %v", err)
	}
	if err := os.Chtimes(activityPath, activityTime, activityTime); err != nil {
		t.Fatalf("set activity source mtime: %v", err)
	}

	tests := []struct {
		name   string
		runner Runner
		kind   FailureKind
		health Health
		state  RunnerState
		want   inboxEvidence
	}{
		{
			name:   "nonzero exit",
			runner: Runner{Name: "failed", Label: "test.failed"},
			kind:   FailureNonzeroExit,
			health: Health{Present: true, HasRun: true, LastExitStatus: -9, ExitReason: "SIGNAL", Runs: 7},
			want: inboxEvidence{
				Checked:         "launchd last exit status, exit reason, and run count",
				FreshnessSource: "runs",
				LastExitStatus:  intPointer(-9),
				ExitReason:      "SIGNAL",
				RunCount:        intPointer(7),
			},
		},
		{
			name:   "not loaded",
			runner: Runner{Name: "missing", Label: "test.missing", ActivitySource: activityPath},
			kind:   FailureNotLoaded,
			health: Health{Present: false},
			want: inboxEvidence{
				Checked:         "launchd job presence",
				FreshnessSource: "not_checked",
				Label:           "test.missing",
				DomainsProbed:   []string{"gui/" + strconv.Itoa(os.Getuid()), "system"},
			},
		},
		{
			name: "stale activity source",
			runner: Runner{
				Name:               "stale",
				Label:              "test.stale",
				ActivitySource:     activityPath,
				ExpectedCadence:    time.Hour,
				StalenessThreshold: 30 * time.Minute,
			},
			kind:   FailureStale,
			health: Health{Present: true, HasRun: true, Runs: 11},
			state:  RunnerState{Runs: 11, RunsChangedAt: activityTime.Add(-time.Hour)},
			want: inboxEvidence{
				Checked:             "last activity against expected cadence plus staleness threshold",
				FreshnessSource:     "activity_source",
				RunCount:            intPointer(11),
				ActivitySource:      activityPath,
				ActivitySourceState: "present",
				LastActivity:        activityTime.Format(time.RFC3339),
				ExpectedCadence:     "1h0m0s",
				StalenessThreshold:  "30m0s",
				StaleAfter:          activityTime.Add(90 * time.Minute).Format(time.RFC3339),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := buildInboxEvidence(test.runner, test.kind, test.health, test.state)
			if err != nil {
				t.Fatalf("buildInboxEvidence() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("buildInboxEvidence() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestInboxWriterHealthyFromStartCreatesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := Runner{Name: "healthy", Type: RunnerTypeLaunchd, Label: "test.healthy"}
	changes, err := NewInboxWriter(dir).Reconcile(
		runner,
		Classification{Runner: runner.Name, Kind: FailureHealthy},
		Health{Present: true, HasRun: true, Runs: 1},
		RunnerState{Runs: 1},
		time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("Reconcile() changes = %#v, want none", changes)
	}
	assertOnlyInboxFile(t, dir)
}

func TestInboxWriterRejectsUnsafeRunnerName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := Runner{Name: "../../escape", Type: RunnerTypeLaunchd, Label: "test.escape"}
	_, err := NewInboxWriter(dir).Reconcile(
		runner,
		Classification{Runner: runner.Name, Kind: FailureNotLoaded},
		Health{Present: false},
		RunnerState{},
		time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	)
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("Reconcile() error = %v, want explicit unsafe-name error", err)
	}
	assertOnlyInboxFile(t, dir)
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape-not_loaded-"+Fingerprint(runner.Name, FailureNotLoaded)[:8]+".md")); !os.IsNotExist(err) {
		t.Fatalf("path outside inbox exists or returned unexpected error: %v", err)
	}
}

func assertInboxState(t *testing.T, path string, wantFrontmatter inboxFrontmatter, wantBody string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	gotFrontmatter, gotBody, err := parseInboxFile(data)
	if err != nil {
		t.Fatalf("parseInboxFile() error = %v", err)
	}
	if !reflect.DeepEqual(gotFrontmatter, wantFrontmatter) {
		t.Errorf("frontmatter = %#v, want %#v", gotFrontmatter, wantFrontmatter)
	}
	if gotBody != wantBody {
		t.Errorf("body = %q, want exact human-edited body %q", gotBody, wantBody)
	}
}

func assertOnlyInboxFile(t *testing.T, dir string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", dir, err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inbox entries = %#v, want %#v", got, want)
	}
}

func intPointer(value int) *int {
	return &value
}
