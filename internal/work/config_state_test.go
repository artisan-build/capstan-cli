package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadConfigStrictPrivateAndXDGDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	if got, err := DefaultConfigPath(); err != nil || got != filepath.Join(root, "capstan", "work.json") {
		t.Fatalf("DefaultConfigPath() = %q, %v", got, err)
	}
	prompt := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("Read {links_file}."), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "work.json")
	raw := validRawConfig(prompt)
	writeRawConfig(t, path, raw, 0o600)
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.MaxConcurrent != 2 || config.OrchestratorCapacity != 1 || config.RequestTimeout.String() != "15s" {
		t.Fatalf("loaded config = %#v", config)
	}

	raw["unknown_key"] = true
	writeRawConfig(t, path, raw, 0o600)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "strict JSON") {
		t.Fatalf("unknown key error = %v", err)
	}
	delete(raw, "unknown_key")
	if runtime.GOOS != "windows" {
		writeRawConfig(t, path, raw, 0o644)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("public config mode error = %v", err)
		}
	}
}

func TestStateAtomicPrivateRoundTripAndMalformedFailClosed(t *testing.T) {
	config := testConfig(t)
	item := &ItemState{
		TaskID: testTaskID, Role: "builder", Fence: 1, Phase: "routed",
		WorkDir: filepath.Join(config.WorkDir, testItemID+"-f1"), LaunchNonce: testNonce, SessionRef: "fake:session",
		Directive: &DirectiveState{ID: testDirectiveID, Kind: "send_enter", Status: "succeeded", ReportPending: true},
	}
	if err := os.MkdirAll(item.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := NewStateStore(config.StateFile, config.WorkDir)
	state := State{Version: stateVersion, Items: map[string]*ItemState{testItemID: item}}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	assertMode(t, config.StateFile, 0o600)
	data, err := os.ReadFile(config.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"bearer-secret", "signature=", "private prompt"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("state contains %q", forbidden)
		}
	}
	state.Items[testItemID].Directive.ReportPending = false
	if err := store.Save(state); err != nil {
		t.Fatalf("second Save() replacement error = %v", err)
	}
	loaded, err := store.Load()
	if err != nil || !reflectState(state, loaded) {
		t.Fatalf("Load() = %#v, %v", loaded, err)
	}

	malformed := []byte(`{"version":1,"items":{"bad":{"fence":1}}}`)
	if err := os.WriteFile(config.StateFile, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "left unchanged") {
		t.Fatalf("malformed state error = %v", err)
	}
	after, _ := os.ReadFile(config.StateFile)
	if string(after) != string(malformed) {
		t.Fatalf("malformed state changed: %q", after)
	}
}

func TestOverlapLockAndCredentialReadOrdering(t *testing.T) {
	config := testConfig(t)
	prompt := config.PromptTemplate
	raw := validRawConfig(prompt)
	raw["state_file"] = config.StateFile
	raw["work_dir"] = config.WorkDir
	raw["lock_file"] = config.LockFile
	raw["credential_file"] = filepath.Join(filepath.Dir(config.Path), "missing-credential")
	writeRawConfig(t, config.Path, raw, 0o600)
	lock, err := AcquireLock(config.LockFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	if _, err := AcquireLock(config.LockFile); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second AcquireLock() error = %v", err)
	}
	var logs []string
	if err := Execute(context.Background(), config.Path, &fakeLauncher{}, staticHealth{}, func(message string) { logs = append(logs, message) }); err != nil {
		t.Fatalf("Execute() while locked read missing credential: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "holds the lock") {
		t.Fatalf("overlap logs = %#v", logs)
	}
}

func TestCredentialRequiresPrivateModeAndNeverAppearsInErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes do not represent ACL privacy")
	}
	path := filepath.Join(t.TempDir(), "credential")
	const secret = "credential-value-that-must-not-leak"
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readCredential(path); err != nil || got != secret {
		t.Fatalf("readCredential(private) = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readCredential(path)
	if err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("readCredential(public) error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("credential error leaked secret: %v", err)
	}
}

func validRawConfig(prompt string) map[string]any {
	return map[string]any{
		"version": 1, "capstan_url": "https://capstan.example", "team": "team", "actor_id": "42",
		"credential_file": "credential", "max_concurrent": 2, "orchestrator_capacity": 1,
		"max_load_1m": 10, "max_memory_percent": 90, "capabilities": []string{"git"},
		"roles": []string{"builder", "orchestrator"}, "role_providers": map[string]string{"builder": "opencode", "orchestrator": "claude"},
		"state_file": "state/work.json", "work_dir": "work", "lock_file": "state/work.lock",
		"prompt_template": prompt, "orchestrator_prompt_template": prompt,
		"role_commands":           map[string][]string{"builder": {"worker", "{links_file}"}, "orchestrator": {"orchestrator", "{prompt_file}"}},
		"request_timeout_seconds": 15,
	}
}

func writeRawConfig(t *testing.T, path string, raw map[string]any, mode os.FileMode) {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func reflectState(left, right State) bool {
	leftData, _ := json.Marshal(left)
	rightData, _ := json.Marshal(right)
	return string(leftData) == string(rightData)
}
