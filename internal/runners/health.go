package runners

import "context"

// Health is one runner's real health signal as reported by the platform.
// It is never derived from a stdout log.
//
// Launchd exposes no timestamp of any kind, so there is deliberately no
// last-run time here. Freshness is computed by the classifier from either the
// configured activity_source mtime or the delta in Runs between checks.
type Health struct {
	// Present is false when the label is not loaded in launchd at all.
	Present bool
	// HasRun is true when launchd reports either a numeric last exit code or a
	// last exit reason. When false, LastExitStatus is meaningless.
	HasRun bool
	// LastExitStatus is launchd's numeric last exit code when one is present.
	// Reason-only terminations do not carry an exit code.
	LastExitStatus int
	// ExitReason is a first-class termination signal. Memory idle exit is a
	// healthy lifecycle event; every other reason is a failure.
	ExitReason string
	// Runs is launchd's monotonic run counter. Comparing it across checks is the
	// only launchd-native way to detect a silently dead periodic job.
	Runs int
}

// HealthReader reads real platform health without modifying the runner.
//
// CI is Linux and has no launchd, so every test must inject FakeReader. Any test
// that touches real launchd must skip when launchd is absent, never hard-fail.
type HealthReader interface {
	Read(ctx context.Context, runner Runner) (Health, error)
}
