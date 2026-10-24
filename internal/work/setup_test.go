package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type setupSolo struct{ fail string }

func (s setupSolo) Call(_ context.Context, name string, _ any, target any) error {
	if name == s.fail {
		return errors.New("discovery failure")
	}
	var value any
	if name == "list_projects" {
		value = []SoloProject{{ID: 12, Name: "Work"}}
	} else {
		enabled := true
		value = map[string]any{"agent_tools": []SoloAgentTool{{ID: 34, Name: "OpenCode", ToolType: "agent", Enabled: &enabled}}}
	}
	data, _ := json.Marshal(value)
	raw := target.(*json.RawMessage)
	*raw = data
	return nil
}
func (setupSolo) Close() error { return nil }

type setupTester struct {
	evidence SelfTestEvidence
	err      error
}

type setupTestFunc func(context.Context, Config, io.Writer) (SelfTestEvidence, error)

func (f setupTestFunc) Run(ctx context.Context, config Config, output io.Writer) (SelfTestEvidence, error) {
	return f(ctx, config, output)
}

func (s setupTester) Run(_ context.Context, _ Config, output io.Writer) (SelfTestEvidence, error) {
	_, _ = fmt.Fprintln(output, "test driver ran")
	return s.evidence, s.err
}

func TestSetupWritesStrictPrivateConfigAndPrintsSchedules(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "work.json")
	options := SetupOptions{ConfigPath: configPath, CapstanURL: "https://capstan.example", Team: "team", ActorID: "42", CredentialFile: filepath.Join(root, "credential"), SoloMCPPath: "/opt/solo/mcp", Bindings: map[string]SoloBinding{"builder": {ProjectID: 12, AgentToolID: 34, Harness: "opencode", ExtraArgs: []string{}}, "orchestrator": {ProjectID: 12, AgentToolID: 34, Harness: "opencode", ExtraArgs: []string{}}}, MaxConcurrent: 2, OrchestratorCapacity: 1, Schedule: "print", Executable: filepath.Join(root, "capstan"), Platform: "darwin", NonInteractive: true}
	var output bytes.Buffer
	all := SelfTestEvidence{Claim: true, Spawn: true, Fetch: true, Milestone: true, Done: true}
	if err := Setup(context.Background(), options, strings.NewReader(""), &output, setupSolo{}, setupTester{evidence: all}); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Launcher != "solo" || config.SoloBindings["builder"].ProjectID != 12 || config.SoloBindings["builder"].AgentToolID != 34 {
		t.Fatalf("config = %#v", config)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(configPath)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %04o", info.Mode().Perm())
		}
	}
	if !strings.Contains(output.String(), "com.artisan-build.capstan-work") || !strings.Contains(output.String(), "Self-test passed") {
		t.Fatalf("output = %s", output.String())
	}
	entries, _ := os.ReadDir(filepath.Dir(configPath))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".work.json-") {
			t.Fatalf("atomic temp residue: %s", entry.Name())
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["role_commands"] = map[string]any{"builder": []string{"ignored"}}
	writeRawConfig(t, configPath, raw, 0o600)
	if _, err := LoadConfig(configPath); err == nil || !strings.Contains(err.Error(), "cannot contain role_commands") {
		t.Fatalf("mixed launcher config error = %v", err)
	}

	options.Platform = "linux"
	options.ConfigPath = filepath.Join(root, "linux", "work.json")
	output.Reset()
	if err := Setup(context.Background(), options, strings.NewReader(""), &output, setupSolo{}, setupTester{evidence: all}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "OnUnitActiveSec=1min") || !strings.Contains(output.String(), "Type=oneshot") {
		t.Fatalf("systemd output = %s", output.String())
	}
}

func TestSetupRejectsNamesUnknownIDsDiscoveryErrorsAndMissingEvidence(t *testing.T) {
	base := SetupOptions{ConfigPath: filepath.Join(t.TempDir(), "work.json"), CapstanURL: "https://capstan.example", Team: "team", ActorID: "42", CredentialFile: "credential", SoloMCPPath: "/mcp", Bindings: map[string]SoloBinding{"orchestrator": {ProjectID: 12, AgentToolID: 34, Harness: "claude", ExtraArgs: []string{}}}, MaxConcurrent: 1, OrchestratorCapacity: 1, Schedule: "print", Executable: "/bin/capstan", Platform: "darwin", NonInteractive: true}
	if err := Setup(context.Background(), base, strings.NewReader(""), io.Discard, setupSolo{fail: "list_projects"}, setupTester{}); err == nil {
		t.Fatal("project discovery failure passed")
	}
	unknown := base
	unknown.Bindings = map[string]SoloBinding{"orchestrator": {ProjectID: 999, AgentToolID: 34, Harness: "claude", ExtraArgs: []string{}}}
	if err := Setup(context.Background(), unknown, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{}); err == nil {
		t.Fatal("display-name/unknown id binding passed")
	}
	partial := SelfTestEvidence{Claim: true, Spawn: true, Fetch: true, Milestone: true}
	if err := Setup(context.Background(), base, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{evidence: partial}); err == nil || !strings.Contains(err.Error(), "every required rung") {
		t.Fatalf("partial evidence error = %v", err)
	}
}

func TestSetupAlwaysReplacesCapstanScheduleFilesAndReloads(t *testing.T) {
	originalCommand := scheduleCommand
	t.Cleanup(func() { scheduleCommand = originalCommand })
	all := SelfTestEvidence{Claim: true, Spawn: true, Fetch: true, Milestone: true, Done: true}

	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			options := validSetupOptions(root)
			options.Platform = platform
			options.Schedule = "install"
			options.Executable = "/opt/capstan"
			schedule, err := RenderWorkSchedule(platform, options.Executable, options.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			for path := range schedule.Files {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("arbitrary existing content"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var calls []string
			scheduleCommand = func(name string, args ...string) *exec.Cmd {
				calls = append(calls, strings.Join(append([]string{name}, args...), " "))
				return exec.Command("true")
			}
			if err := Setup(context.Background(), options, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{evidence: all}); err != nil {
				t.Fatal(err)
			}
			for path, want := range schedule.Files {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Fatalf("generated schedule %s was not installed: %q, %v", path, data, err)
				}
			}
			joined := strings.Join(calls, "\n")
			if platform == "darwin" {
				if len(calls) != 2 || !strings.Contains(joined, "launchctl bootout gui/") || !strings.Contains(joined, "launchctl bootstrap gui/") {
					t.Fatalf("launchd activation calls = %v", calls)
				}
			} else if len(calls) != 2 || calls[0] != "systemctl --user daemon-reload" || calls[1] != "systemctl --user enable --now capstan-work.timer" {
				t.Fatalf("systemd activation calls = %v", calls)
			}
		})
	}
}

func TestSetupFailuresPreserveExistingConfigAndPrompts(t *testing.T) {
	root := t.TempDir()
	options := validSetupOptions(root)
	all := SelfTestEvidence{Claim: true, Spawn: true, Fetch: true, Milestone: true, Done: true}
	if err := Setup(context.Background(), options, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{evidence: all}); err != nil {
		t.Fatal(err)
	}
	paths := []string{options.ConfigPath, filepath.Join(filepath.Dir(options.ConfigPath), "work-prompt.md"), filepath.Join(filepath.Dir(options.ConfigPath), "orchestrator-prompt.md")}
	before := readFiles(t, paths)

	t.Run("invalid reconfiguration", func(t *testing.T) {
		invalid := options
		invalid.Team = "invalid team"
		if err := Setup(context.Background(), invalid, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{evidence: all}); err == nil {
			t.Fatal("invalid generated configuration passed")
		}
		assertFilesEqual(t, paths, before)
	})

	t.Run("held old overlap lock", func(t *testing.T) {
		config, err := LoadConfig(options.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		lock, err := AcquireLock(config.LockFile)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Release() }()
		if err := Setup(context.Background(), options, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{evidence: all}); err == nil || !strings.Contains(err.Error(), "overlap lock") {
			t.Fatalf("held-lock error = %v", err)
		}
		assertFilesEqual(t, paths, before)
	})

	t.Run("failed self-test does not install", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		oldInstaller := setupInstallWorkSchedule
		called := false
		setupInstallWorkSchedule = func(string, WorkSchedule) error {
			called = true
			return nil
		}
		defer func() { setupInstallWorkSchedule = oldInstaller }()
		install := options
		install.Schedule = "install"
		tester := setupTestFunc(func(_ context.Context, config Config, _ io.Writer) (SelfTestEvidence, error) {
			if config.Path == options.ConfigPath {
				t.Fatal("self-test used production configuration")
			}
			assertFilesEqual(t, paths, before)
			return SelfTestEvidence{}, errors.New("self-test failure")
		})
		if err := Setup(context.Background(), install, strings.NewReader(""), io.Discard, setupSolo{}, tester); err == nil {
			t.Fatal("failing self-test passed")
		}
		if called {
			t.Fatal("schedule installer was called before self-test passed")
		}
		assertFilesEqual(t, paths, before)
	})
}

func TestSetupRefusesActiveClaimsWithoutChangingProductionFiles(t *testing.T) {
	root := t.TempDir()
	options := validSetupOptions(root)
	all := SelfTestEvidence{Claim: true, Spawn: true, Fetch: true, Milestone: true, Done: true}
	if err := Setup(context.Background(), options, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{evidence: all}); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(config.WorkDir, testItemID+"-f1")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := State{Version: stateVersion, Items: map[string]*ItemState{testItemID: {
		TaskID: testTaskID, Role: "orchestrator", Fence: 1, Phase: "claimed", WorkDir: workDir,
		LaunchNonce: testNonce, SpokeID: 1, PendingLaunchKey: "initial:" + testNonce,
	}}}
	if err := NewStateStore(config.StateFile, config.WorkDir).Save(state); err != nil {
		t.Fatal(err)
	}
	paths := []string{options.ConfigPath, config.PromptTemplate, config.OrchestratorPromptTemplate}
	before := readFiles(t, paths)
	if err := Setup(context.Background(), options, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{evidence: all}); err == nil || !strings.Contains(err.Error(), "local claims") {
		t.Fatalf("active-claim error = %v", err)
	}
	assertFilesEqual(t, paths, before)
}

func validSetupOptions(root string) SetupOptions {
	return SetupOptions{ConfigPath: filepath.Join(root, "config", "work.json"), CapstanURL: "https://capstan.example", Team: "team", ActorID: "42", CredentialFile: filepath.Join(root, "credential"), SoloMCPPath: "/opt/solo/mcp", Bindings: map[string]SoloBinding{"builder": {ProjectID: 12, AgentToolID: 34, Harness: "opencode", ExtraArgs: []string{}}, "orchestrator": {ProjectID: 12, AgentToolID: 34, Harness: "opencode", ExtraArgs: []string{}}}, MaxConcurrent: 2, OrchestratorCapacity: 1, Schedule: "print", Executable: filepath.Join(root, "capstan"), Platform: "darwin", NonInteractive: true}
}

func readFiles(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[path] = data
	}
	return result
}

func assertFilesEqual(t *testing.T, paths []string, want map[string][]byte) {
	t.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, want[path]) {
			t.Fatalf("%s changed: %v", path, err)
		}
	}
}
