package cmd

import (
	"context"
	"fmt"

	workprotocol "github.com/artisan-build/capstan-cli/internal/work"
	"github.com/spf13/cobra"
)

type workDrainRun func(context.Context, string, workprotocol.Launcher, workprotocol.HealthMeasurer, workprotocol.Logger) error

func newWorkCommand(run workDrainRun) *cobra.Command {
	command := &cobra.Command{
		Use:           "work",
		Short:         "Run Capstan Work machine operations",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	command.AddCommand(newWorkDrainCommand(run))

	return command
}

func newWorkDrainCommand(run workDrainRun) *cobra.Command {
	var configPath string
	command := &cobra.Command{
		Use:   "drain",
		Short: "Run one Work recovery and claim tick",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return run(command.Context(), configPath, nil, nil, func(message string) {
				_, _ = fmt.Fprintln(command.OutOrStdout(), message)
			})
		},
	}
	command.Flags().StringVar(&configPath, "config", "", "Work drain JSON configuration (defaults to the Capstan XDG config)")

	return command
}

func newWorkGateCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "__work-gate",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			return workprotocol.RunGate(args)
		},
	}
}
