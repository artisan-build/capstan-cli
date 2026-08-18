package runners

import (
	"context"
	"sync"
)

// FakeReader is an injectable HealthReader for tests and non-launchd environments.
type FakeReader struct {
	HealthByRunner map[string]Health
	ErrorsByRunner map[string]error

	mu    sync.Mutex
	reads []Runner
}

// Read records runner and returns its configured error or health.
func (f *FakeReader) Read(_ context.Context, runner Runner) (Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reads = append(f.reads, runner)
	if err := f.ErrorsByRunner[runner.Name]; err != nil {
		return Health{}, err
	}

	return f.HealthByRunner[runner.Name], nil
}

// Reads returns a snapshot of runners passed to Read.
func (f *FakeReader) Reads() []Runner {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]Runner(nil), f.reads...)
}
