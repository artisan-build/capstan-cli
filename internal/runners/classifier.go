package runners

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const jetsamMemoryIdleExit = "JETSAM_REASON_MEMORY_IDLE_EXIT"

// FailureKind is the durable health verdict used to fingerprint runner failures.
type FailureKind string

const (
	FailureHealthy     FailureKind = "healthy"
	FailureNotLoaded   FailureKind = "not_loaded"
	FailureNonzeroExit FailureKind = "nonzero_exit"
	FailureStale       FailureKind = "stale"
)

// Classification is one runner's health verdict at a point in time.
type Classification struct {
	Runner string
	Kind   FailureKind
}

// CheckRunner reads and classifies one runner. Reader failures return no classification.
func CheckRunner(
	ctx context.Context,
	reader HealthReader,
	runner Runner,
	prior *RunnerState,
	now time.Time,
) (*Classification, RunnerState, error) {
	health, err := reader.Read(ctx, runner)
	if err != nil {
		return nil, RunnerState{}, fmt.Errorf("read health for runner %q: %w", runner.Name, err)
	}

	classification, updated, err := Classify(runner, health, prior, now)
	if err != nil {
		return nil, RunnerState{}, err
	}

	return &classification, updated, nil
}

// Classify applies exit and freshness rules to a health observation.
func Classify(
	runner Runner,
	health Health,
	prior *RunnerState,
	now time.Time,
) (Classification, RunnerState, error) {
	updated, coldStart := updateRunnerState(prior, health.Runs, now)
	classification := Classification{Runner: runner.Name, Kind: FailureHealthy}

	if !health.Present {
		classification.Kind = FailureNotLoaded

		return classification, updated, nil
	}

	// Exit failures precede staleness because they provide the more actionable diagnosis.
	if health.HasRun && health.LastExitStatus != 0 && health.ExitReason != jetsamMemoryIdleExit {
		classification.Kind = FailureNonzeroExit

		return classification, updated, nil
	}

	stale, err := isStale(runner, updated, coldStart, now)
	if err != nil {
		return Classification{}, RunnerState{}, err
	}
	if stale {
		classification.Kind = FailureStale
	}

	return classification, updated, nil
}

func updateRunnerState(prior *RunnerState, runs int, now time.Time) (RunnerState, bool) {
	if prior == nil {
		return RunnerState{
			Runs:          runs,
			RunsChangedAt: now,
			FirstSeenAt:   now,
		}, true
	}

	updated := *prior
	if updated.Runs != runs {
		updated.Runs = runs
		updated.RunsChangedAt = now
	}

	return updated, false
}

func isStale(runner Runner, state RunnerState, coldStart bool, now time.Time) (bool, error) {
	if runner.ExpectedCadence <= 0 || coldStart {
		return false, nil
	}

	baseline := state.RunsChangedAt
	if runner.ActivitySource != "" {
		info, err := os.Stat(runner.ActivitySource)
		if err == nil {
			baseline = info.ModTime()
		} else if errors.Is(err, os.ErrNotExist) {
			baseline = state.FirstSeenAt
		} else {
			return false, fmt.Errorf(
				"inspect activity source %q for runner %q: %w",
				runner.ActivitySource,
				runner.Name,
				err,
			)
		}
	}

	return now.Sub(baseline) > runner.ExpectedCadence+runner.StalenessThreshold, nil
}
