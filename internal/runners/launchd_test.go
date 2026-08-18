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
			want: Health{Present: true, Runs: 1},
		},
		{
			name: "successful exit",
			file: "launchctl-exit-zero.txt",
			want: Health{Present: true, HasRun: true, Runs: 42},
		},
		{
			name: "nonzero exit",
			file: "launchctl-exit-nonzero.txt",
			want: Health{
				Present:        true,
				HasRun:         true,
				LastExitStatus: 17,
				ExitReason:     "EXIT_REASON_EXITED",
				Runs:           8,
			},
		},
		{
			name: "jetsam idle exit",
			file: "launchctl-jetsam-idle-exit.txt",
			want: Health{
				Present:        true,
				HasRun:         true,
				LastExitStatus: -9,
				ExitReason:     jetsamMemoryIdleExit,
				Runs:           713,
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
		{name: "missing runs", out: "last exit code = 0"},
		{name: "missing exit code", out: "runs = 2"},
		{name: "invalid runs", out: "runs = many\nlast exit code = 0"},
		{name: "negative runs", out: "runs = -1\nlast exit code = 0"},
		{name: "invalid exit code", out: "runs = 2\nlast exit code = bad"},
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

			return []byte("runs = 42\nlast exit code = 0\n"), nil
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

func TestLaunchdReaderTreatsMissingServiceAsNotPresent(t *testing.T) {
	t.Parallel()

	reader := &LaunchdReader{
		uid: 501,
		runCommand: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Could not find service \"com.example.missing\" in domain for user gui: 501"),
				errors.New("exit status 113")
		},
	}
	runner := Runner{Name: "missing", Type: RunnerTypeLaunchd, Label: "com.example.missing"}

	got, err := reader.Read(context.Background(), runner)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	want := Health{Present: false}
	if got != want {
		t.Errorf("Read() = %#v, want %#v", got, want)
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
