package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/artisan-build/capstan-cli/internal/runners"
	"github.com/spf13/cobra"
)

type runnerObservation struct {
	runner         runners.Runner
	health         runners.Health
	classification runners.Classification
	state          runners.RunnerState
}

// NewRunnersCommand builds the runners command with an injectable health
// reader and clock. Production uses LaunchdReader; tests use FakeReader.
func NewRunnersCommand(reader runners.HealthReader, now func() time.Time) *cobra.Command {
	command := &cobra.Command{
		Use:           "runners",
		Short:         "Check local runner health",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	command.AddCommand(newRunnersCheckCommand(reader, now))

	return command
}

func newRunnersCheckCommand(reader runners.HealthReader, now func() time.Time) *cobra.Command {
	var configPath string

	command := &cobra.Command{
		Use:   "check",
		Short: "Check runners and reconcile durable inbox items",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRunnersCheck(cmd, reader, now, configPath)
		},
	}
	command.Flags().StringVar(&configPath, "config", "", "Runner configuration file (defaults to the Capstan XDG config)")

	return command
}

func runRunnersCheck(
	command *cobra.Command,
	reader runners.HealthReader,
	now func() time.Time,
	configPath string,
) (returnErr error) {
	if reader == nil {
		return errors.New("runner health reader is not configured")
	}
	if now == nil {
		return errors.New("runner check clock is not configured")
	}

	config, err := loadRunnersConfig(configPath)
	if err != nil {
		return err
	}

	store := runners.NewStateStore(config.State.Path)
	lock, err := store.AcquireLock()
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	state, err := store.Load()
	if err != nil {
		return err
	}
	checkedAt := now().UTC()
	observations := make([]runnerObservation, 0, len(config.Runners))
	for _, runner := range config.Runners {
		health, err := reader.Read(command.Context(), runner)
		if err != nil {
			return fmt.Errorf("read health for runner %q: %w", runner.Name, err)
		}

		prior, found := state.Runners[runner.Name]
		var priorPointer *runners.RunnerState
		if found {
			priorPointer = &prior
		}
		classification, updated, err := runners.Classify(runner, health, priorPointer, checkedAt)
		if err != nil {
			return err
		}
		observations = append(observations, runnerObservation{
			runner:         runner,
			health:         health,
			classification: classification,
			state:          updated,
		})
		state.Runners[runner.Name] = updated
	}

	writer := runners.NewInboxWriter(config.Inbox.Path)
	summaries := make([]string, 0, len(observations))
	for _, observation := range observations {
		changes, err := writer.Reconcile(
			observation.runner,
			observation.classification,
			observation.health,
			observation.state,
			checkedAt,
		)
		if err != nil {
			return err
		}
		summaries = append(summaries, formatRunnerSummary(observation.classification, changes))
	}

	if err := store.Save(state); err != nil {
		return err
	}
	for _, summary := range summaries {
		if _, err := fmt.Fprintln(command.OutOrStdout(), summary); err != nil {
			return err
		}
	}

	return nil
}

func loadRunnersConfig(path string) (runners.Config, error) {
	if path != "" {
		return runners.Load(path)
	}

	return runners.LoadDefault()
}

func formatRunnerSummary(classification runners.Classification, changes []runners.InboxChange) string {
	if len(changes) == 0 {
		return fmt.Sprintf("%s: %s (no inbox change)", classification.Runner, classification.Kind)
	}

	parts := make([]string, len(changes))
	for i, change := range changes {
		parts[i] = fmt.Sprintf("%s %s", change.Action, change.Filename)
	}

	return fmt.Sprintf("%s: %s (%s)", classification.Runner, classification.Kind, strings.Join(parts, ", "))
}
