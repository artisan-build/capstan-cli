package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/artisan-build/capstan-cli/internal/api"
	"github.com/artisan-build/capstan-cli/internal/auth"
	"github.com/artisan-build/capstan-cli/internal/config"
	"github.com/spf13/cobra"
)

func newLoginCommand(server *string) *cobra.Command {
	var label string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with a Capstan server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, *server, label, timeout)
		},
	}

	cmd.Flags().StringVar(&label, "label", "", "Device label shown during authorization")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Login approval timeout")

	return cmd
}

func runLogin(cmd *cobra.Command, serverFlag, label string, timeout time.Duration) error {
	server, err := config.ResolveServer(serverFlag)
	if err != nil {
		return err
	}

	if label == "" {
		label, err = os.Hostname()
		if err != nil || label == "" {
			label = "capstan-cli"
		}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	token, err := auth.LoopbackLogin(ctx, auth.LoopbackOptions{
		Server: server,
		Label:  label,
		Open:   openBrowser,
	})
	if err != nil {
		return err
	}

	identity, err := api.NewClient(server).Me(ctx, token)
	if err != nil {
		var statusErr api.StatusError
		if errors.As(err, &statusErr) {
			return fmt.Errorf("login failed: server rejected credentials with status %d", statusErr.StatusCode)
		}

		return fmt.Errorf("login failed: verify credentials: %w", err)
	}

	if err := config.Save(config.Credentials{Token: token, Server: server}); err != nil {
		return err
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "logged in to %s as %s <%s>\n", server, identity.Name, identity.Email)

	return err
}
