package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/artisan-build/capstan-cli/internal/api"
	"github.com/artisan-build/capstan-cli/internal/auth"
	"github.com/artisan-build/capstan-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var deviceSleep func(context.Context, time.Duration) error

var errNoServerGuidance = errors.New("no server configured: pass --server or set CAPSTAN_SERVER")

var stdinIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

var readStdinLine = func() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if errors.Is(err, io.EOF) && line != "" {
		return line, nil
	}

	return line, err
}

func newLoginCommand(server *string) *cobra.Command {
	var device bool
	var label string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with a Capstan server and save its URL",
		Long:  "Authenticate with a Capstan server and save its URL. Provide the server with --server or CAPSTAN_SERVER, or enter it when prompted in an interactive terminal.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, *server, label, timeout, device)
		},
	}

	cmd.Flags().BoolVar(&device, "device", false, "Use device-code login for headless environments")
	cmd.Flags().StringVar(&label, "label", "", "Device label shown during authorization")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Login approval timeout")

	return cmd
}

func runLogin(cmd *cobra.Command, serverFlag, label string, timeout time.Duration, device bool) error {
	server, err := config.ResolveServer(serverFlag)
	if err != nil {
		if !errors.Is(err, config.ErrNoServer) {
			return err
		}

		server, err = promptForServer(cmd)
		if err != nil {
			return err
		}
	}

	if label == "" {
		label, err = os.Hostname()
		if err != nil || label == "" {
			label = "capstan-cli"
		}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	var token string
	if device {
		token, err = auth.DeviceLogin(ctx, auth.DeviceOptions{
			Server:  server,
			Label:   label,
			Timeout: timeout,
			Sleep:   deviceSleep,
			Prompt: func(prompt auth.DevicePrompt) error {
				_, promptErr := fmt.Fprintf(cmd.OutOrStdout(), "Open %s and approve the login with code %s.\n", prompt.VerificationURIComplete, prompt.UserCode)

				return promptErr
			},
		})
	} else {
		token, err = auth.LoopbackLogin(ctx, auth.LoopbackOptions{
			Server: server,
			Label:  label,
			Open:   openBrowser,
		})
	}
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

func promptForServer(cmd *cobra.Command) (string, error) {
	if !stdinIsTerminal() {
		return "", errNoServerGuidance
	}

	var lastErr error
	for range 3 {
		if _, err := fmt.Fprint(cmd.ErrOrStderr(), "Capstan server URL: "); err != nil {
			return "", err
		}

		line, err := readStdinLine()
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
				return "", errNoServerGuidance
			}

			return "", err
		}

		server, err := config.ValidateServer(config.CleanServer(line))
		if err == nil {
			return server, nil
		}

		lastErr = err
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err)
	}

	return "", fmt.Errorf("invalid server URL after 3 attempts: %w", lastErr)
}
