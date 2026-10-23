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
