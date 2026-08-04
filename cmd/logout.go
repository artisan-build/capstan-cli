package cmd

import (
	"fmt"

	"github.com/artisan-build/capstan-cli/internal/config"
	"github.com/spf13/cobra"
)

func newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove saved Capstan credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := config.Delete(); err != nil {
				return err
			}

			_, err := fmt.Fprintln(cmd.OutOrStdout(), "logged out")

			return err
		},
	}
}
