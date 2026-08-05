package cmd

import (
	"fmt"
	"os"

	"github.com/artisan-build/capstan-cli/internal/browser"
	"github.com/spf13/cobra"
)

// Version is overridden by release builds using -ldflags "-X github.com/artisan-build/capstan-cli/cmd.Version=<version>".
var Version = "dev"

var openBrowser = browser.Open

func newRootCommand() *cobra.Command {
	var server string

	rootCmd := &cobra.Command{
		Use:           "capstan",
		Short:         "Command-line client for the Capstan ecosystem server",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	rootCmd.PersistentFlags().StringVar(&server, "server", "", "Capstan server base URL (overrides CAPSTAN_SERVER and saved login server)")

	rootCmd.AddCommand(newArtifactCommand(&server))
	rootCmd.AddCommand(newLoginCommand(&server))
	rootCmd.AddCommand(newLogoutCommand())
	rootCmd.AddCommand(newVersionCommand())
	rootCmd.AddCommand(newWhoamiCommand(&server))

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
