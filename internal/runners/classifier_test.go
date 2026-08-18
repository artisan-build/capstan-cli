package runners

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCheckRunnerClassifiesHealthAndUpdatesState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-3 * time.Hour)
	priorUnchanged := RunnerState{Runs: 4, RunsChangedAt: old, FirstSeenAt: old}
	priorZeroExit := RunnerState{Runs: 7, RunsChangedAt: now.Add(-time.Minute), FirstSeenAt: old}
	cadencedRunner := Runner{
		Name:               "runner",
		Type:               RunnerTypeLaunchd,
		Label:              "com.example.runner",
		ExpectedCadence:    time.Hour,
		StalenessThreshold: time.Hour,
	}

	tests := []struct {
		name        string
		runner      Runner
		health      Health
		prior       *RunnerState
		activityAge *time.Duration
		missingFile bool
		want        Classification
		wantState   RunnerState
	}{
		{
			name:      "not loaded",
			runner:    cadencedRunner,
			health:    Health{Present: false},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureNotLoaded},
			wantState: priorUnchanged,
		},
		{
			name:      "zero exit fresh",
			runner:    cadencedRunner,
			health:    Health{Present: true, HasRun: true, Runs: 7},
			prior:     &priorZeroExit,
			want:      Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: priorZeroExit,
		},
		{
			name:      "nonzero exit",
			runner:    cadencedRunner,
			health:    Health{Present: true, HasRun: true, LastExitStatus: 17, Runs: 4},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureNonzeroExit},
			wantState: priorUnchanged,
		},
		{
			// A negative status is a signal termination, not an exit code. It is a genuine
			// failure and must classify as nonzero_exit -- "status != 0" must not narrow to
			// "status > 0".
			name:      "negative status from signal termination",
			runner:    Runner{Name: "runner", Type: RunnerTypeLaunchd, Label: "com.example.runner"},
			health:    Health{Present: true, HasRun: true, LastExitStatus: -9, Runs: 4},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureNonzeroExit},
			wantState: priorUnchanged,
		},
		{
			// The same negative status accompanied by the jetsam idle-exit reason is the
			// NORMAL lifecycle of an on-demand agent and stays healthy: the reason decides,
			// not the sign of the status.
			name:   "negative status with jetsam idle exit reason stays healthy",
			runner: Runner{Name: "runner", Type: RunnerTypeLaunchd, Label: "com.example.runner"},
			health: Health{
				Present: true, HasRun: true, LastExitStatus: -9,
				ExitReason: "JETSAM_REASON_MEMORY_IDLE_EXIT", Runs: 4,
			},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: priorUnchanged,
		},
		{
			name:   "jetsam idle exit",
			runner: Runner{Name: "runner", Type: RunnerTypeLaunchd, Label: "com.example.runner"},
			health: Health{
				Present: true, HasRun: true, ExitReason: "JETSAM_REASON_MEMORY_IDLE_EXIT", Runs: 4,
			},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: priorUnchanged,
		},
		{
			name:   "jetsam memory limit reason",
			runner: cadencedRunner,
			health: Health{
				Present: true, HasRun: true, ExitReason: "JETSAM_REASON_MEMORY_PERPROCESSLIMIT", Runs: 4,
			},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureNonzeroExit},
			wantState: priorUnchanged,
		},
		{
			name:      "unknown exit reason",
			runner:    cadencedRunner,
			health:    Health{Present: true, HasRun: true, ExitReason: "UNKNOWN_REASON", Runs: 4},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureNonzeroExit},
			wantState: priorUnchanged,
		},
		{
			name:      "never exited is not an exit failure",
			runner:    Runner{Name: "runner", Type: RunnerTypeLaunchd, Label: "com.example.runner"},
			health:    Health{Present: true, HasRun: false, LastExitStatus: 99, Runs: 1},
			prior:     &RunnerState{Runs: 1, RunsChangedAt: old, FirstSeenAt: old},
			want:      Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: RunnerState{Runs: 1, RunsChangedAt: old, FirstSeenAt: old},
		},
		{
			name:      "cold start seeds baseline",
			runner:    cadencedRunner,
			health:    Health{Present: true, HasRun: true, Runs: 4},
			want:      Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: RunnerState{Runs: 4, RunsChangedAt: now, FirstSeenAt: now},
		},
		{
			name:      "not loaded cold start seeds freshness baseline",
			runner:    cadencedRunner,
			health:    Health{Present: false},
			want:      Classification{Runner: "runner", Kind: FailureNotLoaded},
			wantState: RunnerState{RunsChangedAt: now, FirstSeenAt: now},
		},
		{
			name:        "cold start ignores old activity source",
			runner:      cadencedRunner,
			health:      Health{Present: true, HasRun: true, Runs: 4},
			activityAge: durationPointer(30 * 24 * time.Hour),
			want:        Classification{Runner: "runner", Kind: FailureHealthy},
			wantState:   RunnerState{Runs: 4, RunsChangedAt: now, FirstSeenAt: now},
		},
		{
			name:      "unchanged runs are stale",
			runner:    cadencedRunner,
			health:    Health{Present: true, HasRun: true, Runs: 4},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureStale},
			wantState: priorUnchanged,
		},
		{
			name:   "zero runs changed falls back to first seen",
			runner: cadencedRunner,
			health: Health{Present: true, Runs: 0},
			prior: &RunnerState{
				Runs: 0, FirstSeenAt: now.Add(-time.Minute),
			},
			want: Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: RunnerState{
				Runs: 0, FirstSeenAt: now.Add(-time.Minute),
			},
		},
		{
			name:   "incremented runs refresh baseline",
			runner: cadencedRunner,
			health: Health{Present: true, HasRun: true, Runs: 5},
			prior:  &priorUnchanged,
			want:   Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: RunnerState{
				Runs: 5, RunsChangedAt: now, FirstSeenAt: old,
			},
		},
		{
			name:        "fresh activity source",
			runner:      cadencedRunner,
			health:      Health{Present: true, HasRun: true, Runs: 4},
			prior:       &priorUnchanged,
			activityAge: durationPointer(time.Minute),
			want:        Classification{Runner: "runner", Kind: FailureHealthy},
			wantState:   priorUnchanged,
		},
		{
			name:        "old activity source",
			runner:      cadencedRunner,
			health:      Health{Present: true, HasRun: true, Runs: 4},
			prior:       &priorUnchanged,
			activityAge: durationPointer(3 * time.Hour),
			want:        Classification{Runner: "runner", Kind: FailureStale},
			wantState:   priorUnchanged,
		},
		{
			name:        "missing activity source on first sight",
			runner:      cadencedRunner,
			health:      Health{Present: true, HasRun: true, Runs: 4},
			missingFile: true,
			want:        Classification{Runner: "runner", Kind: FailureHealthy},
			wantState:   RunnerState{Runs: 4, RunsChangedAt: now, FirstSeenAt: now},
		},
		{
			name:        "missing activity source uses first seen baseline",
			runner:      cadencedRunner,
			health:      Health{Present: true, HasRun: true, Runs: 4},
			prior:       &priorUnchanged,
			missingFile: true,
			want:        Classification{Runner: "runner", Kind: FailureStale},
			wantState:   priorUnchanged,
		},
		{
			name:   "missing activity source remains fresh within first seen baseline",
			runner: cadencedRunner,
			health: Health{Present: true, HasRun: true, Runs: 4},
			prior: &RunnerState{
				Runs: 4, RunsChangedAt: old, FirstSeenAt: now.Add(-time.Hour),
			},
			missingFile: true,
			want:        Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: RunnerState{
				Runs: 4, RunsChangedAt: old, FirstSeenAt: now.Add(-time.Hour),
			},
		},
		{
			name: "no cadence never becomes stale",
			runner: Runner{
				Name: "runner", Type: RunnerTypeLaunchd, Label: "com.example.runner",
			},
			health:    Health{Present: true, HasRun: true, Runs: 4},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: priorUnchanged,
		},
		{
			name:   "exact stale boundary is healthy",
			runner: cadencedRunner,
			health: Health{Present: true, HasRun: true, Runs: 4},
			prior: &RunnerState{
				Runs: 4, RunsChangedAt: now.Add(-2 * time.Hour), FirstSeenAt: old,
			},
			want: Classification{Runner: "runner", Kind: FailureHealthy},
			wantState: RunnerState{
				Runs: 4, RunsChangedAt: now.Add(-2 * time.Hour), FirstSeenAt: old,
			},
		},
		{
			name:   "one instant past stale boundary",
			runner: cadencedRunner,
			health: Health{Present: true, HasRun: true, Runs: 4},
			prior: &RunnerState{
				Runs: 4, RunsChangedAt: now.Add(-2*time.Hour - time.Nanosecond), FirstSeenAt: old,
			},
			want: Classification{Runner: "runner", Kind: FailureStale},
			wantState: RunnerState{
				Runs: 4, RunsChangedAt: now.Add(-2*time.Hour - time.Nanosecond), FirstSeenAt: old,
			},
		},
		{
			name:      "nonzero exit precedes stale",
			runner:    cadencedRunner,
			health:    Health{Present: true, HasRun: true, LastExitStatus: 2, Runs: 4},
			prior:     &priorUnchanged,
			want:      Classification{Runner: "runner", Kind: FailureNonzeroExit},
			wantState: priorUnchanged,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := tt.runner
			if tt.activityAge != nil {
				runner.ActivitySource = filepath.Join(t.TempDir(), "activity")
				if err := os.WriteFile(runner.ActivitySource, []byte("activity"), 0o600); err != nil {
					t.Fatalf("WriteFile(activity source) error = %v", err)
				}
				mtime := now.Add(-*tt.activityAge)
				if err := os.Chtimes(runner.ActivitySource, mtime, mtime); err != nil {
					t.Fatalf("Chtimes(activity source) error = %v", err)
				}
			} else if tt.missingFile {
				runner.ActivitySource = filepath.Join(t.TempDir(), "missing-activity")
			}

			fake := &FakeReader{HealthByRunner: map[string]Health{runner.Name: tt.health}}
			got, gotState, err := CheckRunner(context.Background(), fake, runner, tt.prior, now)
			if err != nil {
				t.Fatalf("CheckRunner() error = %v", err)
			}
			if got == nil {
				t.Fatal("CheckRunner() classification = nil, want non-nil")
			}
			if *got != tt.want {
				t.Errorf("CheckRunner() classification = %#v, want %#v", *got, tt.want)
			}
			if gotState != tt.wantState {
				t.Errorf("CheckRunner() state = %#v, want %#v", gotState, tt.wantState)
			}
			if gotReads := fake.Reads(); !reflect.DeepEqual(gotReads, []Runner{runner}) {
				t.Errorf("FakeReader.Reads() = %#v, want %#v", gotReads, []Runner{runner})
			}
		})
	}
}

func TestCheckRunnerAbsentThenLoadedWithoutRunsIsHealthy(t *testing.T) {
	t.Parallel()

	firstCheck := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	runner := Runner{
		Name:               "runner",
		Type:               RunnerTypeLaunchd,
		Label:              "com.example.runner",
		ExpectedCadence:    time.Hour,
		StalenessThreshold: time.Hour,
	}
	fake := &FakeReader{HealthByRunner: map[string]Health{
		runner.Name: {Present: false},
	}}

	firstClassification, firstState, err := CheckRunner(
		context.Background(), fake, runner, nil, firstCheck,
	)
	if err != nil {
		t.Fatalf("first CheckRunner() error = %v", err)
	}
	wantFirstClassification := Classification{Runner: runner.Name, Kind: FailureNotLoaded}
	if firstClassification == nil || *firstClassification != wantFirstClassification {
		t.Errorf("first classification = %#v, want %#v", firstClassification, wantFirstClassification)
	}
	wantState := RunnerState{RunsChangedAt: firstCheck, FirstSeenAt: firstCheck}
	if firstState != wantState {
		t.Errorf("first state = %#v, want %#v", firstState, wantState)
	}

	fake.HealthByRunner[runner.Name] = Health{Present: true, Runs: 0}
	secondClassification, secondState, err := CheckRunner(
		context.Background(), fake, runner, &firstState, firstCheck.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("second CheckRunner() error = %v", err)
	}
	wantSecondClassification := Classification{Runner: runner.Name, Kind: FailureHealthy}
	if secondClassification == nil || *secondClassification != wantSecondClassification {
		t.Errorf("second classification = %#v, want %#v", secondClassification, wantSecondClassification)
	}
	if secondState != wantState {
		t.Errorf("second state = %#v, want %#v", secondState, wantState)
	}
	wantReads := []Runner{runner, runner}
	if gotReads := fake.Reads(); !reflect.DeepEqual(gotReads, wantReads) {
		t.Errorf("FakeReader.Reads() = %#v, want %#v", gotReads, wantReads)
	}
}

func TestFailureKindDurableValues(t *testing.T) {
	t.Parallel()

	got := []FailureKind{FailureHealthy, FailureNotLoaded, FailureNonzeroExit, FailureStale}
	want := []FailureKind{"healthy", "not_loaded", "nonzero_exit", "stale"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("failure kinds = %#v, want %#v", got, want)
	}
}

func TestCheckRunnerReaderErrorReturnsNoClassification(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("launchctl unavailable")
	runner := Runner{Name: "runner", Type: RunnerTypeLaunchd, Label: "com.example.runner"}
	fake := &FakeReader{ErrorsByRunner: map[string]error{runner.Name: wantErr}}

	got, gotState, err := CheckRunner(context.Background(), fake, runner, nil, time.Now())
	if !errors.Is(err, wantErr) {
		t.Errorf("CheckRunner() error = %v, want error wrapping %v", err, wantErr)
	}
	if got != nil {
		t.Errorf("CheckRunner() classification = %#v, want nil", got)
	}
	if gotState != (RunnerState{}) {
		t.Errorf("CheckRunner() state = %#v, want zero value", gotState)
	}
}

func durationPointer(duration time.Duration) *time.Duration {
	return &duration
}
