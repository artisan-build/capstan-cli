package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

func TestInstallScheduleRefusesUnrelatedJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capstan-work.service")
	if err := os.WriteFile(path, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := InstallWorkSchedule("linux", WorkSchedule{Files: map[string]string{path: "capstan-work"}})
	if err == nil {
		t.Fatal("unrelated schedule was overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "unrelated" {
		t.Fatalf("unrelated schedule changed: %q", data)
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

func TestScheduleOwnershipRequiresStructuralMarkerAndIdentity(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		file     string
		existing string
	}{
		{"launchd comment and path", "darwin", "com.artisan-build.capstan-work.plist", `<?xml version="1.0"?><plist><dict><key>Label</key><string>unrelated.job</string><key>ProgramArguments</key><array><string>/tmp/capstan-work</string></array><!-- com.artisan-build.capstan-work --></dict></plist>`},
		{"launchd marker with wrong label", "darwin", "com.artisan-build.capstan-work.plist", `<?xml version="1.0"?><plist><dict><key>Label</key><string>unrelated.job</string><key>CapstanManaged</key><string>capstan-work/v1</string></dict></plist>`},
		{"systemd service description and command", "linux", "capstan-work.service", "[Unit]\nDescription=Unrelated monitor for capstan-work\n\n[Service]\nExecStart=/tmp/capstan-work-helper\n"},
		{"systemd timer comment and target", "linux", "capstan-work.timer", "# capstan-work.timer\n[Unit]\nDescription=Unrelated\n\n[Timer]\nUnit=capstan-work.service\n"},
		{"systemd ambiguous markers", "linux", "capstan-work.service", "[Unit]\nX-Capstan-Managed=capstan-work/v1\nX-Capstan-Managed=capstan-work/v1\nX-Capstan-Unit=capstan-work.service\n\n[Service]\nExecStart=/tmp/capstan-work\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.file)
			if err := os.WriteFile(path, []byte(test.existing), 0o600); err != nil {
				t.Fatal(err)
			}
			err := InstallWorkSchedule(test.platform, WorkSchedule{Files: map[string]string{path: "replacement"}})
			if err == nil {
				t.Fatal("unrelated schedule was accepted")
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil || string(data) != test.existing {
				t.Fatalf("schedule changed: %q, %v", data, readErr)
			}
		})
	}
	for _, platform := range []string{"darwin", "linux"} {
		schedule, err := RenderWorkSchedule(platform, "/opt/capstan", "/private/work.json")
		if err != nil {
			t.Fatal(err)
		}
		for path, content := range schedule.Files {
			if !ownedScheduleFile(platform, path, []byte(content)) {
				t.Fatalf("generated %s schedule lacks structural ownership: %s", platform, path)
			}
		}
	}
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
