package runners

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  func(t *testing.T, inbox, state string) string
		want    []string
		notWant []string
		nonRoot bool
		prepare func(t *testing.T, inbox, state string) (string, string)
	}{
		{
			name: "missing inbox path",
			config: func(_ *testing.T, _, state string) string {
				return fmt.Sprintf("inbox: {}\nstate:\n  path: %q\n", state)
			},
			want: []string{"inbox.path", "required"},
		},
		{
			name: "inbox path does not exist",
			prepare: func(_ *testing.T, inbox, state string) (string, string) {
				return filepath.Join(inbox, "missing"), state
			},
			want: []string{"inbox.path", "does not exist"},
		},
		{
			name: "inbox path is not a directory",
			prepare: func(t *testing.T, inbox, state string) (string, string) {
				return writeNotDirectory(t, inbox), state
			},
			want:    []string{"inbox.path", "not a directory"},
			notWant: []string{"not writable"},
		},
		{
			name: "inbox path is not writable",
			prepare: func(t *testing.T, inbox, state string) (string, string) {
				makeNotWritable(t, inbox)

				return inbox, state
			},
			want:    []string{"inbox.path", "not writable"},
			nonRoot: true,
		},
		{
			name: "missing state path",
			config: func(_ *testing.T, inbox, _ string) string {
				return fmt.Sprintf("inbox:\n  path: %q\nstate: {}\n", inbox)
			},
			want: []string{"state.path", "required"},
		},
		{
			name: "state path does not exist",
			prepare: func(_ *testing.T, inbox, state string) (string, string) {
				return inbox, filepath.Join(state, "missing")
			},
			want: []string{"state.path", "does not exist"},
		},
		{
			name: "state path is not a directory",
			prepare: func(t *testing.T, inbox, state string) (string, string) {
				return inbox, writeNotDirectory(t, state)
			},
			want:    []string{"state.path", "not a directory"},
			notWant: []string{"not writable"},
		},
		{
			name: "state path is not writable",
			prepare: func(t *testing.T, inbox, state string) (string, string) {
				makeNotWritable(t, state)

				return inbox, state
			},
			want:    []string{"state.path", "not writable"},
			nonRoot: true,
		},
		{
			name: "runner missing name",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, `  - type: launchd
    label: test.runner`)
			},
			want: []string{"runner[0]", "name", "required"},
		},
		{
			name: "duplicate runner name is case insensitive",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, validRunnerYAML("Ballast", "test.ballast")+"\n"+validRunnerYAML("ballast", "test.other"))
			},
			want: []string{`runner "ballast"`, "name", `runner "Ballast"`, "case-insensitively"},
		},
		{
			name: "duplicate launchd label",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, validRunnerYAML("first", "test.shared")+"\n"+validRunnerYAML("second", "test.shared"))
			},
			want: []string{`runner "second"`, "label", `"test.shared"`, `runner "first"`},
		},
		{
			name: "runner missing type",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, `  - name: no-type
    label: test.runner`)
			},
			want: []string{`runner "no-type"`, "type", `value ""`, "launchd"},
		},
		{
			name: "runner type is unknown",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, `  - name: command-runner
    type: command`)
			},
			want: []string{`runner "command-runner"`, "type", `"command"`, "launchd"},
		},
		{
			name: "launchd runner missing label",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, `  - name: no-label
    type: launchd`)
			},
			want: []string{`runner "no-label"`, "label"},
		},
		{
			name: "staleness threshold without cadence",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, `  - name: inert-threshold
    type: launchd
    label: test.runner
    staleness_threshold: 1h`)
			},
			want: []string{`runner "inert-threshold"`, "staleness_threshold", "expected_cadence"},
		},
		{
			name: "launchd runner has zero expected cadence",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, `  - name: zero-cadence
    type: launchd
    label: test.runner
    expected_cadence: 0s`)
			},
			want: []string{`runner "zero-cadence"`, "expected_cadence", "greater than zero"},
		},
		{
			name: "runner duration is unparseable",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, `  - name: bad-cadence
    type: launchd
    label: test.runner
    expected_cadence: soon`)
			},
			want: []string{`runner "bad-cadence"`, "expected_cadence", `"soon"`},
		},
		{
			name: "runner duration is negative",
			config: func(_ *testing.T, inbox, state string) string {
				return configYAML(inbox, state, `  - name: negative-threshold
    type: launchd
    label: test.runner
    expected_cadence: 1h
    staleness_threshold: -1m`)
			},
			want: []string{`runner "negative-threshold"`, "staleness_threshold", "negative"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Root ignores permission bits, so it cannot exercise the not-writable failure.
			if tt.nonRoot && os.Geteuid() == 0 {
				t.Skip("root can write despite directory permission bits")
			}

			inbox := t.TempDir()
			state := t.TempDir()
			if tt.prepare != nil {
				inbox, state = tt.prepare(t, inbox, state)
			}

			contents := configYAML(inbox, state, "")
			if tt.config != nil {
				contents = tt.config(t, inbox, state)
			}
			path := writeConfig(t, t.TempDir(), contents)

			_, err := Load(path)
			if err == nil {
				t.Fatal("Load returned nil error, want validation failure")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load error %q does not contain %q", err, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(err.Error(), notWant) {
					t.Errorf("Load error %q unexpectedly contains %q", err, notWant)
				}
			}
		})
	}
}

func TestLoadRejectsInvalidRunnerNames(t *testing.T) {
	tests := []struct {
		name       string
		runnerName string
	}{
		{name: "path traversal", runnerName: "../../tmp/pwned"},
		{name: "path separator", runnerName: "a/b"},
		{name: "dot", runnerName: "."},
		{name: "dot dot", runnerName: ".."},
		{name: "surrounding whitespace", runnerName: " ballast "},
		{name: "newline", runnerName: "bad\nname"},
		{name: "leading punctuation", runnerName: "-runner"},
		{name: "over 64 characters", runnerName: strings.Repeat("a", 65)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inbox := t.TempDir()
			state := t.TempDir()
			runner := fmt.Sprintf(`  - name: %q
    type: launchd
    label: test.runner`, tt.runnerName)

			_, err := Load(writeConfig(t, t.TempDir(), configYAML(inbox, state, runner)))
			if err == nil {
				t.Fatal("Load returned nil error, want invalid runner name")
			}
			for _, want := range []string{fmt.Sprintf("runner %q", tt.runnerName), "field name", "1-64"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestLoadStrictlyDecodesKnownSections(t *testing.T) {
	tests := []struct {
		name   string
		config func(inbox, state string) string
		want   []string
	}{
		{
			name: "numeric runner name",
			config: func(inbox, state string) string {
				return configYAML(inbox, state, `  - name: 0755
    type: launchd
    label: test.runner`)
			},
			want: []string{"runners", "runner[0]", "name"},
		},
		{
			name: "date runner name",
			config: func(inbox, state string) string {
				return configYAML(inbox, state, `  - name: 2024-01-01
    type: launchd
    label: test.runner`)
			},
			want: []string{"runners", "runner[0]", "name"},
		},
		{
			name: "boolean runner label",
			config: func(inbox, state string) string {
				return configYAML(inbox, state, `  - name: strict-runner
    type: launchd
    label: true`)
			},
			want: []string{"runners", "runner[0]", "label"},
		},
		{
			name: "unknown inbox key",
			config: func(_, state string) string {
				return fmt.Sprintf("inbox:\n  pth: /tmp\nstate:\n  path: %q\n", state)
			},
			want: []string{"inbox", "pth"},
		},
		{
			name: "unknown state key",
			config: func(inbox, _ string) string {
				return fmt.Sprintf("inbox:\n  path: %q\nstate:\n  pth: /tmp\n", inbox)
			},
			want: []string{"state", "pth"},
		},
		{
			name: "unknown runner key",
			config: func(inbox, state string) string {
				return configYAML(inbox, state, `  - name: typo-runner
    type: launchd
    label: test.runner
    expected_cadnce: 1h`)
			},
			want: []string{"runners", "runner[0]", "expected_cadnce"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inbox := t.TempDir()
			state := t.TempDir()

			_, err := Load(writeConfig(t, t.TempDir(), tt.config(inbox, state)))
			if err == nil {
				t.Fatal("Load returned nil error, want strict decode failure")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestLoadExpandsPathsAndRepresentsCadence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name           string
		inboxPath      string
		statePath      string
		activitySource string
		runnerFields   string
		wantInbox      string
		wantState      string
		wantActivity   string
		wantCadence    time.Duration
		wantStaleness  time.Duration
	}{
		{
			name:           "tilde alone with cadence default",
			inboxPath:      "~",
			statePath:      "~",
			activitySource: "~",
			runnerFields:   "    expected_cadence: 1h\n",
			wantInbox:      home,
			wantState:      home,
			wantActivity:   home,
			wantCadence:    time.Hour,
			wantStaleness:  time.Hour,
		},
		{
			name:           "tilde slash with explicit threshold",
			inboxPath:      "~/inbox",
			statePath:      "~/.local/state/capstan",
			activitySource: "~/.runner-last-run",
			runnerFields:   "    expected_cadence: 1h\n    staleness_threshold: 30m\n",
			wantInbox:      filepath.Join(home, "inbox"),
			wantState:      filepath.Join(home, ".local", "state", "capstan"),
			wantActivity:   filepath.Join(home, ".runner-last-run"),
			wantCadence:    time.Hour,
			wantStaleness:  30 * time.Minute,
		},
		{
			name:           "absolute paths and on demand runner",
			inboxPath:      filepath.Join(home, "absolute-inbox"),
			statePath:      filepath.Join(home, "absolute-state"),
			activitySource: filepath.Join(home, "absolute-activity"),
			wantInbox:      filepath.Join(home, "absolute-inbox"),
			wantState:      filepath.Join(home, "absolute-state"),
			wantActivity:   filepath.Join(home, "absolute-activity"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, dir := range []string{tt.wantInbox, tt.wantState} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("create configured directory: %v", err)
				}
			}

			contents := fmt.Sprintf(`inbox:
  path: %q
state:
  path: %q
runners:
  - name: test-runner
    type: launchd
    label: test.runner
%s    activity_source: %q
`, tt.inboxPath, tt.statePath, tt.runnerFields, tt.activitySource)
			cfg, err := Load(writeConfig(t, t.TempDir(), contents))
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}

			if cfg.Inbox.Path != tt.wantInbox {
				t.Errorf("Inbox.Path = %q, want %q", cfg.Inbox.Path, tt.wantInbox)
			}
			if cfg.State.Path != tt.wantState {
				t.Errorf("State.Path = %q, want %q", cfg.State.Path, tt.wantState)
			}
			runner := cfg.Runners[0]
			if runner.ActivitySource != tt.wantActivity {
				t.Errorf("ActivitySource = %q, want %q", runner.ActivitySource, tt.wantActivity)
			}
			if runner.ExpectedCadence != tt.wantCadence {
				t.Errorf("ExpectedCadence = %s, want %s", runner.ExpectedCadence, tt.wantCadence)
			}
			if runner.StalenessThreshold != tt.wantStaleness {
				t.Errorf("StalenessThreshold = %s, want %s", runner.StalenessThreshold, tt.wantStaleness)
			}
		})
	}
}

func TestLoadAcceptsUnknownTopLevelSections(t *testing.T) {
	inbox := t.TempDir()
	state := t.TempDir()
	contents := configYAML(inbox, state, validRunnerYAML("known-runner", "test.known-runner")) + `policy:
  authority: server-resolved
`

	cfg, err := Load(writeConfig(t, t.TempDir(), contents))
	if err != nil {
		t.Fatalf("Load returned error for unknown top-level section: %v", err)
	}
	if len(cfg.Runners) != 1 || cfg.Runners[0].Name != "known-runner" {
		t.Fatalf("Runners = %#v, want known-runner", cfg.Runners)
	}
}

func TestLoadDefaultUsesXDGConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", t.TempDir())
	inbox := t.TempDir()
	state := t.TempDir()

	path := writeConfig(t, filepath.Join(configHome, "capstan"), configYAML(inbox, state, validRunnerYAML("default-runner", "test.default")))
	if want := filepath.Join(configHome, "capstan", configFilename); path != want {
		t.Fatalf("test config path = %q, want %q", path, want)
	}

	cfg, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault returned error: %v", err)
	}
	if len(cfg.Runners) != 1 || cfg.Runners[0].Name != "default-runner" {
		t.Fatalf("Runners = %#v, want default-runner", cfg.Runners)
	}
}

func TestLoadRemovesDirectoryWriteProbes(t *testing.T) {
	inbox := t.TempDir()
	state := t.TempDir()

	if _, err := Load(writeConfig(t, t.TempDir(), configYAML(inbox, state, ""))); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	for _, target := range []struct {
		name string
		path string
	}{
		{name: "inbox.path", path: inbox},
		{name: "state.path", path: state},
	} {
		entries, err := os.ReadDir(target.path)
		if err != nil {
			t.Fatalf("read %s: %v", target.name, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s contains %d write-probe files, want 0", target.name, len(entries))
		}
	}
}

func writeConfig(t *testing.T, dir, contents string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	path := filepath.Join(dir, configFilename)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

func configYAML(inbox, state, runners string) string {
	return fmt.Sprintf("inbox:\n  path: %q\nstate:\n  path: %q\nrunners:\n%s\n", inbox, state, runners)
}

func validRunnerYAML(name, label string) string {
	return fmt.Sprintf(`  - name: %q
    type: launchd
    label: %q`, name, label)
}

func writeNotDirectory(t *testing.T, parent string) string {
	t.Helper()

	path := filepath.Join(parent, "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write non-directory path: %v", err)
	}

	return path
}

func makeNotWritable(t *testing.T, path string) {
	t.Helper()

	if err := os.Chmod(path, 0o500); err != nil {
		t.Fatalf("chmod directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
}
