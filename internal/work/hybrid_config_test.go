package work

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHybridConfigStrictEvolutionAndVersionTwoCompatibility(t *testing.T) {
	options := validSetupOptions(t.TempDir())
	plan, err := stageSetupConfig(options)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.cleanup()
	data, err := os.ReadFile(plan.stagedConfig.Path)
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := json.Unmarshal(data, &original); err != nil {
		t.Fatal(err)
	}
	if original["version"] != float64(2) || original["launcher"] != "solo" || original["amp_path"] != nil || original["amp_roles"] != nil {
		t.Fatal("setup silently changed existing Solo configs")
	}
	loaded, err := LoadConfig(plan.stagedConfig.Path)
	if err != nil || !reflect.DeepEqual(loaded.SoloBindings, options.Bindings) {
		t.Fatal("version 2 binding behavior changed")
	}
	base := func() map[string]any {
		var raw map[string]any
		_ = json.Unmarshal(data, &raw)
		raw["version"] = 3
		raw["launcher"] = "hybrid"
		raw["amp_path"] = "/opt/amp"
		raw["amp_roles"] = []string{"builder"}
		raw["roles"] = []string{"builder", "orchestrator"}
		raw["role_providers"] = map[string]string{"builder": "claude", "orchestrator": "claude"}
		raw["solo_bindings"] = map[string]SoloBinding{"orchestrator": options.Bindings["orchestrator"]}
		return raw
	}
	writeRawConfig(t, plan.stagedConfig.Path, base(), 0o600)
	hybrid, err := LoadConfig(plan.stagedConfig.Path)
	if err != nil || hybrid.Launcher != "hybrid" || hybrid.AmpPath != "/opt/amp" || !reflect.DeepEqual(hybrid.AmpRoles, []string{"builder"}) {
		t.Fatalf("hybrid: %#v %v", hybrid, err)
	}
	for name, change := range map[string]func(map[string]any){
		"missing path":    func(raw map[string]any) { delete(raw, "amp_path") },
		"relative path":   func(raw map[string]any) { raw["amp_path"] = "amp" },
		"role overlap":    func(raw map[string]any) { raw["solo_bindings"] = options.Bindings },
		"missing binding": func(raw map[string]any) { raw["solo_bindings"] = map[string]SoloBinding{} },
		"wrong provider": func(raw map[string]any) {
			raw["role_providers"] = map[string]string{"builder": "openai", "orchestrator": "claude"}
		},
		"orchestrator Amp":   func(raw map[string]any) { raw["amp_roles"] = []string{"orchestrator"} },
		"unknown Amp role":   func(raw map[string]any) { raw["amp_roles"] = []string{"unknown"} },
		"duplicate Amp role": func(raw map[string]any) { raw["amp_roles"] = []string{"builder", "builder"} },
		"version 2 with Amp": func(raw map[string]any) { raw["version"] = 2; raw["launcher"] = "solo" },
		"empty Amp list":     func(raw map[string]any) { raw["amp_roles"] = []string{} },
	} {
		t.Run(name, func(t *testing.T) {
			raw := base()
			change(raw)
			writeRawConfig(t, plan.stagedConfig.Path, raw, 0o600)
			if _, err := LoadConfig(plan.stagedConfig.Path); err == nil {
				t.Fatal("invalid hybrid accepted")
			}
		})
	}
}

func TestHybridSetupWritesOnlyExplicitAmpRolesAndAbsoluteDiscoveredPath(t *testing.T) {
	root := t.TempDir()
	options := validSetupOptions(root)
	delete(options.Bindings, "builder")
	options.AmpRoles = []string{"builder", "reviewer"}
	options.AmpPath = filepath.Join(root, "amp")
	all := SelfTestEvidence{Claim: true, Spawn: true, Fetch: true, Milestone: true, Done: true}
	var output bytes.Buffer
	if err := Setup(context.Background(), options, strings.NewReader(""), &output, setupSolo{}, setupTester{evidence: all}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Launcher != "hybrid" || len(c.SoloBindings) != 1 || c.SoloBindings["orchestrator"].ProjectID != 12 || c.RoleProviders["builder"] != "claude" || c.RoleProviders["reviewer"] != "claude" || c.AmpPath != options.AmpPath {
		t.Fatal("hybrid setup routing changed")
	}
	if !strings.Contains(output.String(), "Self-test passed") {
		t.Fatal("setup self-test not observed")
	}
	// Discovery resolves PATH to an absolute path without executing Amp or reading a token.
	dir := t.TempDir()
	binary := filepath.Join(dir, "amp")
	if err := os.WriteFile(binary, []byte("not executed"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	options.AmpPath = ""
	if err := completeSetupOptions(&options, strings.NewReader(""), io.Discard, []SoloProject{{ID: 12, Name: "fixture"}}, []SoloAgentTool{{ID: 34, Name: "fixture"}}); err != nil {
		t.Fatal(err)
	}
	if options.AmpPath != binary || !filepath.IsAbs(options.AmpPath) {
		t.Fatal("Amp discovery did not write an absolute path")
	}
}
