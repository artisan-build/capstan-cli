package runners

import (
	"context"
	"time"
)

// Health is one runner's real health signal, read from the platform. It is never read from a stdout log.
type Health struct {
	Present        bool
	LastExitStatus int
	LastRun        time.Time
}

// HealthReader reads real platform health without modifying the runner.
//
// CI is Linux and has no launchd, so every test must inject FakeReader. Any test
// that touches real launchd must skip when launchd is absent, never hard-fail.
type HealthReader interface {
	Read(ctx context.Context, runner Runner) (Health, error)
}
