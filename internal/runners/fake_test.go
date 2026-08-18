package runners

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFakeReaderReturnsConfiguredResultsAndRecordsReads(t *testing.T) {
	wantErr := errors.New("health unavailable")
	wantHealth := Health{Present: true, LastExitStatus: 2, LastRun: time.Unix(123, 0)}
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
