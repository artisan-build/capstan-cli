package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	workprotocol "github.com/artisan-build/capstan-cli/internal/work"
)

func TestWorkDrainCommandPassesExplicitConfigAndPrintsSafeLog(t *testing.T) {
	var gotPath string
	run := func(_ context.Context, path string, launcher workprotocol.Launcher, health workprotocol.HealthMeasurer, logger workprotocol.Logger) error {
		gotPath = path
		if launcher != nil || health != nil {
			t.Fatal("command unexpectedly supplied test seams")
		}
		logger("queue empty")
		return nil
	}
	command := newWorkCommand(run)
	stdout := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetArgs([]string{"drain", "--config", "/private/work.json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotPath != "/private/work.json" || stdout.String() != "queue empty\n" {
		t.Fatalf("path, stdout = %q, %q", gotPath, stdout.String())
	}
}

func TestWorkSetupCommandPassesNoninteractiveExplicitBindings(t *testing.T) {
	var got workprotocol.SetupOptions
	setup := func(_ context.Context, options workprotocol.SetupOptions, _ io.Reader, output io.Writer) error {
		got = options
		_, _ = fmt.Fprintln(output, "setup complete")
		return nil
	}
	command := newWorkCommandWithSetup(func(context.Context, string, workprotocol.Launcher, workprotocol.HealthMeasurer, workprotocol.Logger) error {
		return nil
	}, setup)
	stdout := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetArgs([]string{"setup", "--non-interactive", "--capstan-url", "https://capstan.example", "--team", "team", "--actor-id", "42", "--credential-file", "/private/credential", "--binding", "builder=12:34:opencode", "--binding", "orchestrator=12:35:claude"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !got.NonInteractive || got.Bindings["builder"].ProjectID != 12 || got.Bindings["builder"].AgentToolID != 34 || got.Bindings["orchestrator"].Harness != "claude" {
		t.Fatalf("setup options = %#v", got)
	}
	if stdout.String() != "setup complete\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestParseSoloBindingsRejectsNamesAndDuplicates(t *testing.T) {
	for _, values := range [][]string{{"builder=Project:Tool:opencode"}, {"builder=1:2:opencode", "builder=1:3:claude"}} {
		if _, err := parseSoloBindings(values); err == nil {
			t.Fatalf("bindings %#v passed", values)
		}
	}
}
