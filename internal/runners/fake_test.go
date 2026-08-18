package runners

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestFakeReaderReturnsConfiguredResultsAndRecordsReads(t *testing.T) {
	wantErr := errors.New("health unavailable")
	wantHealth := Health{
		Present:        true,
		HasRun:         true,
		LastExitStatus: -9,
		ExitReason:     "JETSAM_REASON_MEMORY_IDLE_EXIT",
		Runs:           713,
	}
	fake := &FakeReader{
		HealthByRunner: map[string]Health{"healthy": wantHealth},
		ErrorsByRunner: map[string]error{"broken": wantErr},
	}
	tests := []struct {
		name       string
		wantHealth Health
		wantErr    error
	}{
		{name: "healthy", wantHealth: wantHealth},
		{name: "broken", wantErr: wantErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fake.Read(context.Background(), Runner{Name: tt.name})
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Read error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.wantHealth {
				t.Errorf("Read health = %#v, want %#v", got, tt.wantHealth)
			}
		})
	}

	reads := fake.Reads()
	if len(reads) != len(tests) {
		t.Fatalf("Reads length = %d, want %d", len(reads), len(tests))
	}
	for i, tt := range tests {
		if reads[i].Name != tt.name {
			t.Errorf("Reads[%d].Name = %q, want %q", i, reads[i].Name, tt.name)
		}
	}
}

func TestFakeReaderConcurrentAccess(t *testing.T) {
	const (
		readWorkers     = 8
		snapshotWorkers = 8
		iterations      = 100
	)

	fake := &FakeReader{HealthByRunner: map[string]Health{"runner": {Present: true}}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(readWorkers + snapshotWorkers)

	for range readWorkers {
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				if _, err := fake.Read(context.Background(), Runner{Name: "runner"}); err != nil {
					t.Errorf("Read returned error: %v", err)
				}
			}
		}()
	}
	for range snapshotWorkers {
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				_ = fake.Reads()
			}
		}()
	}

	close(start)
	wg.Wait()

	if got, want := len(fake.Reads()), readWorkers*iterations; got != want {
		t.Fatalf("Reads length = %d, want %d", got, want)
	}
}
