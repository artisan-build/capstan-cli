package cmd

import (
	"bytes"
	"context"
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
