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
		config  func(t *testing.T, inbox string) string
		want    []string
		nonRoot bool
		prepare func(t *testing.T, inbox string) string
	}{
		{
			name: "missing inbox path",
			config: func(_ *testing.T, _ string) string {
				return "inbox: {}\n"
			},
			want: []string{"inbox.path", "required"},
		},
		{
			name: "inbox path does not exist",
			prepare: func(_ *testing.T, inbox string) string {
				return filepath.Join(inbox, "missing")
			},
			want: []string{"inbox.path", "does not exist"},
		},
		{
			name: "inbox path is not a directory",
			prepare: func(t *testing.T, inbox string) string {
				path := filepath.Join(inbox, "file")
				if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("write inbox file: %v", err)
				}

				return path
			},
			want: []string{"inbox.path", "not a directory"},
		},
		{
			name: "inbox path is not writable",
			prepare: func(t *testing.T, inbox string) string {
				if err := os.Chmod(inbox, 0o500); err != nil {
					t.Fatalf("chmod inbox: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(inbox, 0o700) })

				return inbox
			},
			want:    []string{"inbox.path", "not writable"},
			nonRoot: true,
		},
		{
			name: "runner missing name",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, `  - type: launchd
    label: test.runner
    expected_cadence: 1h`)
			},
			want: []string{"runner[0]", "name"},
		},
		{
			name: "duplicate runner name",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, validRunnerYAML("duplicate")+"\n"+validRunnerYAML("duplicate"))
			},
			want: []string{`runner "duplicate"`, "name", "duplicated"},
		},
		{
			name: "runner missing type",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, `  - name: no-type
    label: test.runner
    expected_cadence: 1h`)
			},
			want: []string{`runner "no-type"`, "type", `value ""`, "launchd"},
		},
		{
			name: "runner type is unknown",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, `  - name: command-runner
    type: command`)
			},
			want: []string{`runner "command-runner"`, "type", `"command"`, "launchd"},
		},
		{
			name: "launchd runner missing label",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, `  - name: no-label
    type: launchd
    expected_cadence: 1h`)
			},
			want: []string{`runner "no-label"`, "label"},
		},
		{
			name: "launchd runner missing expected cadence",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, `  - name: no-cadence
    type: launchd
    label: test.runner`)
			},
			want: []string{`runner "no-cadence"`, "expected_cadence"},
		},
		{
			name: "launchd runner has zero expected cadence",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, `  - name: zero-cadence
    type: launchd
    label: test.runner
    expected_cadence: 0s`)
			},
			want: []string{`runner "zero-cadence"`, "expected_cadence", "greater than zero"},
		},
		{
			name: "runner duration is unparseable",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, `  - name: bad-cadence
    type: launchd
    label: test.runner
    expected_cadence: soon`)
			},
			want: []string{`runner "bad-cadence"`, "expected_cadence", `"soon"`},
		},
		{
			name: "runner duration is negative",
			config: func(_ *testing.T, inbox string) string {
				return configYAML(inbox, `  - name: negative-threshold
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
			if tt.prepare != nil {
				inbox = tt.prepare(t, inbox)
			}

			contents := configYAML(inbox, "")
			if tt.config != nil {
				contents = tt.config(t, inbox)
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
		})
	}
}

func TestLoadExpandsPathsAndDefaultsThreshold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name           string
		inboxPath      string
		activitySource string
		wantInbox      string
		wantActivity   string
	}{
		{
			name:           "tilde alone",
			inboxPath:      "~",
			activitySource: "~",
			wantInbox:      home,
			wantActivity:   home,
		},
		{
			name:           "tilde slash",
			inboxPath:      "~/inbox",
			activitySource: "~/.runner-last-run",
			wantInbox:      filepath.Join(home, "inbox"),
			wantActivity:   filepath.Join(home, ".runner-last-run"),
		},
		{
			name:           "absolute paths untouched",
			inboxPath:      filepath.Join(home, "absolute-inbox"),
			activitySource: filepath.Join(home, "absolute-activity"),
			wantInbox:      filepath.Join(home, "absolute-inbox"),
			wantActivity:   filepath.Join(home, "absolute-activity"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.MkdirAll(tt.wantInbox, 0o700); err != nil {
				t.Fatalf("create inbox: %v", err)
			}

			contents := fmt.Sprintf(`inbox:
  path: %q
runners:
  - name: test-runner
    type: launchd
    label: test.runner
    expected_cadence: 1h
    activity_source: %q
`, tt.inboxPath, tt.activitySource)
			cfg, err := Load(writeConfig(t, t.TempDir(), contents))
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}

			if cfg.Inbox.Path != tt.wantInbox {
				t.Errorf("Inbox.Path = %q, want %q", cfg.Inbox.Path, tt.wantInbox)
			}
			if got := cfg.Runners[0].ActivitySource; got != tt.wantActivity {
				t.Errorf("ActivitySource = %q, want %q", got, tt.wantActivity)
			}
			if got := cfg.Runners[0].StalenessThreshold; got != time.Hour {
				t.Errorf("StalenessThreshold = %s, want %s", got, time.Hour)
			}
		})
	}
}

func TestLoadAcceptsUnknownTopLevelSections(t *testing.T) {
	inbox := t.TempDir()
	contents := configYAML(inbox, `  - name: known-runner
    type: launchd
    label: test.known-runner
    expected_cadence: 1h
    staleness_threshold: 30m`) + `policy:
  authority: server-resolved
`

	cfg, err := Load(writeConfig(t, t.TempDir(), contents))
	if err != nil {
		t.Fatalf("Load returned error for unknown top-level section: %v", err)
	}
	if len(cfg.Runners) != 1 {
		t.Fatalf("Runners length = %d, want 1", len(cfg.Runners))
	}
	want := Runner{
		Name:               "known-runner",
		Type:               RunnerTypeLaunchd,
		Label:              "test.known-runner",
		ExpectedCadence:    time.Hour,
		StalenessThreshold: 30 * time.Minute,
	}
	if cfg.Runners[0] != want {
		t.Fatalf("Runners = %#v, want known-runner", cfg.Runners)
	}
}

func TestLoadDefaultUsesXDGConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", t.TempDir())
	inbox := t.TempDir()

	path := writeConfig(t, filepath.Join(configHome, "capstan"), configYAML(inbox, validRunnerYAML("default-runner")))
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

func configYAML(inbox, runners string) string {
	return fmt.Sprintf("inbox:\n  path: %q\nrunners:\n%s\n", inbox, runners)
}

func validRunnerYAML(name string) string {
	return fmt.Sprintf(`  - name: %s
    type: launchd
    label: test.%s
    expected_cadence: 1h`, name, name)
}
