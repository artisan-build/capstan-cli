package work

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertSoloCarrier(t *testing.T, prompt, links, role, nonce string) {
	t.Helper()
	for _, required := range []string{
		"First read the brief URL from the brief key in " + links,
		"fetch that signed brief with delivery nonce " + nonce,
		"Every other signed action URL", links + ", keyed by action",
		"renews links.json every minute", "Re-read " + links + " immediately before every signed call",
		"never cache a URL", "Do not re-fetch the brief as a link-refresh mechanism",
		"brief reads settle receipt/stall evidence",
	} {
		if !strings.Contains(prompt, required) {
			t.Errorf("%s prompt missing carrier instruction %q", role, required)
		}
	}
	actions := []string{"milestone", "steers", "evidence"}
	if role == "orchestrator" {
		actions = []string{"orchestration", "orchestrate", "verdicts and child actions", "The turn window is fixed at claim; finish each turn, and you will be woken again."}
	} else if strings.Contains(prompt, "turn window") || strings.Contains(prompt, "woken again") {
		t.Error("orchestrator turn instructions appeared in a leaf prompt")
	}
	for _, action := range actions {
		if !strings.Contains(prompt, action) {
			t.Errorf("%s prompt missing action %s", role, action)
		}
	}
}

func TestSoloRenderedCarrierAndDefaultTemplates(t *testing.T) {
	options := validSetupOptions(t.TempDir())
	all := SelfTestEvidence{Claim: true, Spawn: true, Fetch: true, Milestone: true, Done: true}
	if err := Setup(context.Background(), options, strings.NewReader(""), io.Discard, setupSolo{}, setupTester{evidence: all}); err != nil {
		t.Fatal(err)
	}
	defaults, err := LoadConfig(options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for role, path := range map[string]string{"builder": defaults.PromptTemplate, "orchestrator": defaults.OrchestratorPromptTemplate} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		links := filepath.Join(t.TempDir(), linksFilename)
		prompt, err := render(string(data), map[string]string{"links_file": links, "launch_nonce": testNonce})
		if err != nil {
			t.Fatal(err)
		}
		assertSoloCarrier(t, prompt, links, role, testNonce)
		assertMode(t, path, 0o600)
	}
	for _, role := range []string{"builder", "reviewer", "orchestrator"} {
		for _, source := range []string{"existing", "default"} {
			t.Run(role+"/"+source, func(t *testing.T) {
				config := testSoloConfig(t)
				if source == "default" {
					config.PromptTemplate = defaults.PromptTemplate
					config.OrchestratorPromptTemplate = defaults.OrchestratorPromptTemplate
				}
				item := seedItem(t, config, testItemID, role, "routed", "fake:session")
				runner := NewRunner(config, &fakeAPI{}, &fakeLauncher{}, staticHealth{}, nil)
				if err := runner.writePrompt(testItemID, item, ""); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(item.WorkDir, promptFilename)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				assertSoloCarrier(t, string(data), filepath.Join(item.WorkDir, linksFilename), role, testNonce)
				if strings.Count(string(data), "First read the brief URL") != 1 {
					t.Fatal("carrier was duplicated")
				}
				assertMode(t, path, 0o600)
				assertMode(t, item.WorkDir, 0o700)
			})
		}
	}
}

func TestSoloCarrierSecondReadObservesRenewal(t *testing.T) {
	for _, role := range []string{"builder", "orchestrator"} {
		t.Run(role, func(t *testing.T) {
			config := testSoloConfig(t)
			item := seedItem(t, config, testItemID, role, "routed", "fake:session")
			if err := writeLinksFile(item, testLinks("https://old.example")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(item.WorkDir, linksFilename)
			read := func() map[string]string {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var links map[string]string
				if err := json.Unmarshal(data, &links); err != nil {
					t.Fatal("invalid private link file")
				}
				return links
			}
			first := read()
			api := renewedAPI(role, "routed")
			if err := NewRunner(config, api, &fakeLauncher{}, staticHealth{}, nil).Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			second := read()
			for action, want := range testLinks("https://renew.example") {
				if second[action] != want || first[action] == second[action] {
					t.Errorf("second read did not observe renewed %s link", action)
				}
			}
			assertMode(t, path, 0o600)
			assertMode(t, item.WorkDir, 0o700)
		})
	}
}

func TestSoloPointerRejectsInvalidInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), linksFilename)
	for _, input := range []struct{ path, nonce string }{
		{"relative/links.json", testNonce}, {path + "\n", testNonce}, {path, "invalid"},
	} {
		if _, err := pointerFromLinks(input.path, input.nonce); err == nil || err.Error() != "work pointer inputs are invalid" {
			t.Fatal("invalid pointer inputs were accepted or exposed")
		}
	}
}

func TestSoloCarrierLeavesAmpPromptUnchanged(t *testing.T) {
	config := testSoloConfig(t)
	config.Launcher = "hybrid"
	config.AmpRoles = []string{"builder"}
	item := seedItem(t, config, testItemID, "builder", "routed", "fake:session")
	runner := NewRunner(config, &fakeAPI{}, &fakeLauncher{}, staticHealth{}, nil)
	if err := runner.writePrompt(testItemID, item, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(item.WorkDir, promptFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "Read the current links from "+filepath.Join(item.WorkDir, linksFilename)+".\n" {
		t.Fatal("Solo carrier changed the Amp prompt")
	}
}

func TestSoloCarrierUnderHybrid(t *testing.T) {
	for _, role := range []string{"builder", "orchestrator"} {
		t.Run(role, func(t *testing.T) {
			config := testSoloConfig(t)
			config.Launcher = "hybrid"
			config.AmpRoles = []string{"reviewer"}
			item := seedItem(t, config, testItemID, role, "routed", "fake:session")
			runner := NewRunner(config, &fakeAPI{}, &fakeLauncher{}, staticHealth{}, nil)
			if err := runner.writePrompt(testItemID, item, ""); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(item.WorkDir, promptFilename))
			if err != nil {
				t.Fatal(err)
			}
			assertSoloCarrier(t, string(data), filepath.Join(item.WorkDir, linksFilename), role, testNonce)
		})
	}
}

func TestSoloCarrierReplacesOnlyExactLegacyDefaults(t *testing.T) {
	for _, role := range []string{"builder", "orchestrator"} {
		for _, custom := range []bool{false, true} {
			config := testSoloConfig(t)
			legacy := "Fetch the current signed brief with the launch nonce from the Solo pointer.\n"
			path := config.PromptTemplate
			if role == "orchestrator" {
				legacy = "Fetch the current signed orchestrator brief with the launch nonce from the Solo pointer.\n"
				path = config.OrchestratorPromptTemplate
			}
			text := legacy
			if custom {
				text += "Custom instructions for {role} and nonce {launch_nonce}.\n"
			}
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			item := seedItem(t, config, testItemID, role, "routed", "fake:session")
			runner := NewRunner(config, &fakeAPI{}, &fakeLauncher{}, staticHealth{}, nil)
			if err := runner.writePrompt(testItemID, item, ""); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(item.WorkDir, promptFilename))
			if err != nil {
				t.Fatal(err)
			}
			carrier, err := render(soloPromptTemplate(role), runner.values(testItemID, item, ""))
			if err != nil {
				t.Fatal(err)
			}
			want := carrier
			if custom {
				rendered, err := render(text, runner.values(testItemID, item, ""))
				if err != nil {
					t.Fatal(err)
				}
				want += "\n" + rendered
			}
			if string(data) != want {
				t.Errorf("legacy default replacement changed custom=%v template semantics for %s", custom, role)
			}
			original, err := os.ReadFile(path)
			if err != nil || string(original) != text {
				t.Fatal("source template was modified")
			}
		}
	}
}

// Check at both delivery boundaries, not just after the directive has finished.
type nonceCheckingLauncher struct {
	*fakeLauncher
	t     *testing.T
	path  string
	role  string
	nonce string
}

func (l *nonceCheckingLauncher) checkPrompt() {
	l.t.Helper()
	data, err := os.ReadFile(l.path)
	if err != nil {
		l.t.Fatal(err)
	}
	assertSoloCarrier(l.t, string(data), filepath.Join(filepath.Dir(l.path), linksFilename), l.role, l.nonce)
	if strings.Contains(string(data), testNonce) {
		l.t.Error("respawn prompt retained the pre-rotation nonce")
	}
}

func (l *nonceCheckingLauncher) Launch(ctx context.Context, spec LaunchSpec) (string, error) {
	l.checkPrompt()
	return l.fakeLauncher.Launch(ctx, spec)
}

func (l *nonceCheckingLauncher) OpenGate(ctx context.Context, ref, path, nonce string) error {
	l.checkPrompt()
	if nonce != l.nonce {
		l.t.Error("respawn gate used the pre-rotation nonce")
	}
	return l.fakeLauncher.OpenGate(ctx, ref, path, nonce)
}

func TestSoloRespawnRendersRotatedNonceBeforeLaunchAndGate(t *testing.T) {
	for _, mode := range []string{"solo", "hybrid"} {
		for _, role := range []string{"builder", "orchestrator"} {
			t.Run(mode+"/"+role, func(t *testing.T) {
				config := testSoloConfig(t)
				config.Launcher = mode
				config.AmpRoles = []string{"reviewer"}
				item := seedItem(t, config, testItemID, role, "routed", "fake:old")
				launcher := &nonceCheckingLauncher{fakeLauncher: &fakeLauncher{}, t: t, path: filepath.Join(item.WorkDir, promptFilename), role: role, nonce: testRespawnNonce}
				runner := NewRunner(config, &fakeAPI{}, launcher, staticHealth{}, nil)
				if err := runner.writePrompt(testItemID, item, "fake:old"); err != nil {
					t.Fatal(err)
				}
				state, err := runner.store.Load()
				if err != nil {
					t.Fatal(err)
				}
				if err := runner.handleDirectives(context.Background(), &state, []Directive{respawnDirective(testDirectiveID, testRespawnNonce)}); err != nil {
					t.Fatal(err)
				}
				launcher.checkPrompt()
				held := state.Items[testItemID]
				if held.LaunchNonce != testRespawnNonce || held.Directive.Status != "succeeded" || len(launcher.launches) != 1 || len(launcher.gates) != 1 {
					t.Fatal("respawn did not launch and deliver under the rotated nonce")
				}
				assertMode(t, launcher.path, 0o600)
				assertMode(t, item.WorkDir, 0o700)
			})
		}
	}
}

func TestSoloRespawnPromptFailureStaysUnlaunched(t *testing.T) {
	for _, role := range []string{"builder", "orchestrator"} {
		t.Run(role, func(t *testing.T) {
			config := testSoloConfig(t)
			item := seedItem(t, config, testItemID, role, "routed", "fake:old")
			launcher := &fakeLauncher{}
			runner := NewRunner(config, renewedAPI(role, "claimed"), launcher, staticHealth{}, nil)
			if err := runner.writePrompt(testItemID, item, "fake:old"); err != nil {
				t.Fatal(err)
			}
			path := config.PromptTemplate
			if role == "orchestrator" {
				path = config.OrchestratorPromptTemplate
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			state, err := runner.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.handleDirectives(context.Background(), &state, []Directive{respawnDirective(testDirectiveID, testRespawnNonce)}); err != nil {
				t.Fatal(err)
			}
			persisted, err := runner.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			held := persisted.Items[testItemID]
			if held.LaunchNonce != testRespawnNonce || held.SessionRef != "" || held.Phase != "claimed" || held.PendingLaunchKey != "directive:"+testDirectiveID || held.Directive.Status != "failed" || !held.Directive.ReportPending {
				t.Fatal("prompt failure did not retain the rotated, unlaunched, failed directive state")
			}
			if err := runner.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(launcher.launches) != 0 || len(launcher.gates) != 0 || len(launcher.terminated) != 1 {
				t.Fatal("prompt failure or subsequent recovery launched or delivered a stale prompt")
			}
		})
	}
}

func TestSoloRespawnDoesNotRerenderAmpPrompt(t *testing.T) {
	config := testSoloConfig(t)
	config.Launcher = "hybrid"
	config.AmpRoles = []string{"builder"}
	item := seedItem(t, config, testItemID, "builder", "routed", "fake:old")
	launcher := &fakeLauncher{}
	runner := NewRunner(config, &fakeAPI{}, launcher, staticHealth{}, nil)
	if err := runner.writePrompt(testItemID, item, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.WorkDir, promptFilename)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config.PromptTemplate); err != nil {
		t.Fatal(err)
	}
	state, err := runner.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.handleDirectives(context.Background(), &state, []Directive{respawnDirective(testDirectiveID, testRespawnNonce)}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("respawn changed the Amp prompt")
	}
	if state.Items[testItemID].Directive.Status != "succeeded" || len(launcher.launches) != 1 || len(launcher.gates) != 1 {
		t.Fatal("Amp respawn acquired a dependency on the local prompt template")
	}
}
