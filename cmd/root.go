package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is overridden by release builds using -ldflags "-X github.com/artisan-build/capstan-cli/cmd.Version=<version>".
var Version = "dev"

func newRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "capstan",
		Short:         "Command-line client for the Capstan ecosystem server",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	rootCmd.AddCommand(newVersionCommand())

	return rootCmd
}

// Execute runs the root command and prints command errors to stderr.
func Execute() error {
	rootCmd := newRootCommand()
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		return err
	}

	return nil
}
