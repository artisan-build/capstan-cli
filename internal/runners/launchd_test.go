package runners

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseLaunchctlPrintFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file string
		want Health
	}{
		{
			name: "running and never exited",
			file: "launchctl-running-never-exited.txt",
			want: Health{Present: true},
		},
		{
			name: "successful exit",
			file: "launchctl-exit-zero.txt",
			want: Health{Present: true, HasRun: true, Runs: 1},
		},
		{
			// A negative "last exit code" is a wait status: the job was terminated by
			// signal -LastExitStatus, which is a failure and must not be read as an exit code.
			name: "negative status from signal termination",
			file: "launchctl-signal-negative-status.txt",
			want: Health{
				Present:        true,
				HasRun:         true,
				LastExitStatus: -9,
				Runs:           12,
			},
		},
		{
			name: "nonzero exit",
			file: "launchctl-exit-nonzero.txt",
			want: Health{
				Present:        true,
				HasRun:         true,
				LastExitStatus: 1,
				Runs:           313460,
			},
		},
		{
			name: "jetsam idle exit",
			file: "launchctl-jetsam-idle-exit.txt",
			want: Health{
				Present:    true,
				HasRun:     true,
				ExitReason: jetsamMemoryIdleExit,
				Runs:       430,
			},
		},
		{
			name: "jetsam memory limit",
			file: "launchctl-jetsam-memory-limit.txt",
			want: Health{
				Present:    true,
				HasRun:     true,
				ExitReason: "JETSAM_REASON_MEMORY_PERPROCESSLIMIT",
				Runs:       69,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := os.ReadFile(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}

			got, err := parseLaunchctlPrint(string(out))
			if err != nil {
				t.Fatalf("parseLaunchctlPrint() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("parseLaunchctlPrint() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseLaunchctlPrintRejectsMalformedOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
	}{
		{name: "missing runs", out: "\tlast exit code = 0"},
		{name: "missing termination evidence", out: "\truns = 2"},
		{name: "invalid runs", out: "\truns = many\n\tlast exit code = 0"},
		{name: "negative runs", out: "\truns = -1\n\tlast exit code = 0"},
		{name: "invalid exit code", out: "\truns = 2\n\tlast exit code = bad"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := parseLaunchctlPrint(tt.out); err == nil {
				t.Fatal("parseLaunchctlPrint() error = nil, want non-nil")
			}
		})
	}
}

func TestLaunchdReaderUsesOnlyPrint(t *testing.T) {
	t.Parallel()

	wantHealth := Health{Present: true, HasRun: true, LastExitStatus: 0, Runs: 42}
	var gotName string
	var gotArgs []string
	reader := &LaunchdReader{
		uid: 501,
		runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			gotName = name
			gotArgs = append([]string(nil), args...)

			return []byte("\truns = 42\n\tlast exit code = 0\n"), nil
		},
	}
	runner := Runner{Name: "example", Type: RunnerTypeLaunchd, Label: "com.example.runner"}

	gotHealth, err := reader.Read(context.Background(), runner)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if gotHealth != wantHealth {
		t.Errorf("Read() health = %#v, want %#v", gotHealth, wantHealth)
	}
	if gotName != "launchctl" {
		t.Errorf("command name = %q, want %q", gotName, "launchctl")
	}
	wantArgs := []string{"print", "gui/501/com.example.runner"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("command args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestLaunchdReaderFallsBackToSystemDomain(t *testing.T) {
	t.Parallel()

	missingErr := errors.New("exit status 113")
	var calls [][]string
	reader := &LaunchdReader{
		uid: 501,
		runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			if args[1] == "gui/501/com.example.daemon" {
				return []byte("Could not find service in domain for user gui: 501"), missingErr
			}

			return []byte("\truns = 9\n\tlast exit code = 0\n"), nil
		},
	}
	runner := Runner{Name: "daemon", Type: RunnerTypeLaunchd, Label: "com.example.daemon"}

	got, err := reader.Read(context.Background(), runner)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	want := Health{Present: true, HasRun: true, Runs: 9}
	if got != want {
		t.Errorf("Read() = %#v, want %#v", got, want)
	}
	wantCalls := [][]string{
		{"launchctl", "print", "gui/501/com.example.daemon"},
		{"launchctl", "print", "system/com.example.daemon"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("commands = %#v, want %#v", calls, wantCalls)
	}
}

func TestLaunchdReaderTreatsBothDomainsMissingAsNotPresent(t *testing.T) {
	t.Parallel()

	missingErr := errors.New("exit status 113")
	var targets []string
	reader := &LaunchdReader{
		uid: 501,
		runCommand: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			targets = append(targets, args[1])

			return []byte("Could not find service"), missingErr
		},
	}

	got, err := reader.Read(context.Background(), Runner{
		Name: "missing", Type: RunnerTypeLaunchd, Label: "com.example.missing",
	})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got != (Health{Present: false}) {
		t.Errorf("Read() = %#v, want not present", got)
	}
	wantTargets := []string{"gui/501/com.example.missing", "system/com.example.missing"}
	if !reflect.DeepEqual(targets, wantTargets) {
		t.Errorf("targets = %#v, want %#v", targets, wantTargets)
	}
}

func TestLaunchdReaderSurfacesSystemDomainError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("permission denied")
	call := 0
	reader := &LaunchdReader{
		uid: 501,
		runCommand: func(context.Context, string, ...string) ([]byte, error) {
			call++
			if call == 1 {
				return []byte("Could not find service"), errors.New("exit status 113")
			}

			return []byte("not permitted"), wantErr
		},
	}

	_, err := reader.Read(context.Background(), Runner{
		Name: "daemon", Type: RunnerTypeLaunchd, Label: "com.example.daemon",
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("Read() error = %v, want error wrapping %v", err, wantErr)
	}
}

func TestParseLaunchctlPrintIgnoresNestedFields(t *testing.T) {
	t.Parallel()

	out := "\truns = 3\n\tlast exit code = 0\n\tendpoints = {\n" +
		"\t\truns = 999\n\t\tlast exit code = 9\n\t}\n"
	want := Health{Present: true, HasRun: true, Runs: 3}
	got, err := parseLaunchctlPrint(out)
	if err != nil {
		t.Fatalf("parseLaunchctlPrint() error = %v", err)
	}
	if got != want {
		t.Errorf("parseLaunchctlPrint() = %#v, want %#v", got, want)
	}
}

func TestLaunchdReaderSurfacesExecutionAndParseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		err  error
	}{
		{name: "execution", out: "permission denied", err: errors.New("exit status 1")},
		{name: "parse", out: "unexpected output"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &LaunchdReader{
				uid: 501,
				runCommand: func(context.Context, string, ...string) ([]byte, error) {
					return []byte(tt.out), tt.err
				},
			}

			if _, err := reader.Read(context.Background(), Runner{
				Name: "runner", Type: RunnerTypeLaunchd, Label: "com.example.runner",
			}); err == nil {
				t.Fatal("Read() error = nil, want non-nil")
			}
		})
	}
}

func TestLaunchdReaderRejectsUnsupportedTypeWithoutExecution(t *testing.T) {
	t.Parallel()

	called := false
	reader := &LaunchdReader{
		uid: 501,
		runCommand: func(context.Context, string, ...string) ([]byte, error) {
			called = true

			return nil, nil
		},
	}

	if _, err := reader.Read(context.Background(), Runner{Name: "runner", Type: "other"}); err == nil {
		t.Fatal("Read() error = nil, want non-nil")
	}
	if called {
		t.Fatal("runCommand called for unsupported runner type")
	}
}
