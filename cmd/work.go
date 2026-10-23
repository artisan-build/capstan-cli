package cmd

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	workprotocol "github.com/artisan-build/capstan-cli/internal/work"
	"github.com/spf13/cobra"
)

type workDrainRun func(context.Context, string, workprotocol.Launcher, workprotocol.HealthMeasurer, workprotocol.Logger) error
type workSetupRun func(context.Context, workprotocol.SetupOptions, io.Reader, io.Writer) error

func newWorkCommand(run workDrainRun) *cobra.Command {
	return newWorkCommandWithSetup(run, func(ctx context.Context, options workprotocol.SetupOptions, input io.Reader, output io.Writer) error {
		return workprotocol.RunSetup(ctx, options, input, output)
	})
}

func newWorkCommandWithSetup(run workDrainRun, setup workSetupRun) *cobra.Command {
	command := &cobra.Command{
		Use:           "work",
		Short:         "Run Capstan Work machine operations",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	command.AddCommand(newWorkDrainCommand(run))
	command.AddCommand(newWorkSetupCommand(setup))

	return command
}

func newWorkSetupCommand(run workSetupRun) *cobra.Command {
	var options workprotocol.SetupOptions
	var bindings []string
	command := &cobra.Command{
		Use: "setup", Short: "Configure Solo bindings, scheduling, and a live self-test", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			parsed, err := parseSoloBindings(bindings)
			if err != nil {
				return err
			}
			options.Bindings = parsed
			return run(command.Context(), options, command.InOrStdin(), command.OutOrStdout())
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.ConfigPath, "config", "", "Work JSON configuration path")
	flags.StringVar(&options.CapstanURL, "capstan-url", "", "Capstan server base URL")
	flags.StringVar(&options.Team, "team", "", "Capstan team id or key")
	flags.StringVar(&options.ActorID, "actor-id", "", "Capstan actor id")
	flags.StringVar(&options.CredentialFile, "credential-file", "", "Private bound Work credential file")
	flags.StringVar(&options.SoloMCPPath, "solo-mcp", "", "Solo stdio MCP executable path")
	flags.StringSliceVar(&bindings, "binding", nil, "Role binding role=project_id:agent_tool_id:harness (repeatable)")
	flags.IntVar(&options.MaxConcurrent, "max-concurrent", 2, "Leaf capacity")
	flags.IntVar(&options.OrchestratorCapacity, "orchestrator-capacity", 1, "Orchestrator capacity")
	flags.StringVar(&options.Schedule, "schedule", "print", "Schedule action: print or install")
	flags.BoolVar(&options.NonInteractive, "non-interactive", false, "Require all setup values from flags")
	flags.DurationVar(&options.SelfTestTimeout, "self-test-timeout", 5*time.Minute, "Maximum live self-test duration")
	return command
}

func parseSoloBindings(values []string) (map[string]workprotocol.SoloBinding, error) {
	bindings := make(map[string]workprotocol.SoloBinding, len(values))
	for _, value := range values {
		role, rest, ok := strings.Cut(value, "=")
		parts := strings.Split(rest, ":")
		if !ok || role == "" || len(parts) != 3 {
			return nil, fmt.Errorf("invalid Solo binding %q", value)
		}
		projectID, projectErr := strconv.ParseInt(parts[0], 10, 64)
		toolID, toolErr := strconv.ParseInt(parts[1], 10, 64)
		if projectErr != nil || toolErr != nil || projectID < 1 || toolID < 1 {
			return nil, fmt.Errorf("invalid Solo binding %q", value)
		}
		if _, duplicate := bindings[role]; duplicate {
			return nil, fmt.Errorf("duplicate Solo binding for role %q", role)
		}
		bindings[role] = workprotocol.SoloBinding{ProjectID: projectID, AgentToolID: toolID, Harness: parts[2], ExtraArgs: []string{}}
	}
	return bindings, nil
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
