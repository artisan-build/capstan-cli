package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/artisan-build/capstan-cli/internal/api"
	"github.com/artisan-build/capstan-cli/internal/config"
	"github.com/spf13/cobra"
)

func newWhoamiCommand(server *string) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Print the authenticated Capstan identity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWhoami(cmd, *server, jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print identity as JSON")

	return cmd
}

func runWhoami(cmd *cobra.Command, serverFlag string, jsonOutput bool) error {
	creds, err := config.Load()
	if err != nil {
		if errors.Is(err, config.ErrNotLoggedIn) {
			return errors.New("not logged in")
		}

		return err
	}

	server, err := config.ResolveServer(serverFlag)
	if err != nil {
		if errors.Is(err, config.ErrNoServer) {
			return errors.New("no server configured: run capstan login, pass --server, or set CAPSTAN_SERVER")
		}

		return err
	}
	if server != config.CleanServer(creds.Server) {
		return fmt.Errorf("not logged in to %s; run capstan login --server %s", server, server)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()

	identity, err := api.NewClient(server).Me(ctx, creds.Token)
	if err != nil {
		var statusErr api.StatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == 401 {
			return errors.New("authentication failed; run capstan login")
		}

		return fmt.Errorf("whoami failed: %w", err)
	}

	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(identity)
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s <%s>\n", identity.Name, identity.Email)

	return err
}
