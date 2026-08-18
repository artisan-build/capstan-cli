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
	// HasRun is false when launchd reports "last exit code = (never exited)".
	// When false, LastExitStatus is meaningless and must not be interpreted.
	HasRun bool
	// LastExitStatus is launchd's wait status. A negative value means the job
	// was terminated by signal -LastExitStatus; it is not an exit code.
	LastExitStatus int
	// ExitReason must be considered with LastExitStatus. In particular,
	// JETSAM_REASON_MEMORY_IDLE_EXIT with status -9 is a healthy idle exit, while
	// a genuine signal termination or non-zero non-idle exit is a failure.
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
