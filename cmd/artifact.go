package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/artisan-build/capstan-cli/internal/api"
	"github.com/artisan-build/capstan-cli/internal/config"
	"github.com/spf13/cobra"
)

const maxArtifactBytes = 1_048_576

func newArtifactCommand(server *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifact",
		Short: "Manage Capstan artifacts",
	}

	cmd.AddCommand(newArtifactCreateCommand(server))

	return cmd
}

func newArtifactCreateCommand(server *string) *cobra.Command {
	var filePath string
	var visibility string
	var expires string
	var contentType string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Upload an HTML artifact",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runArtifactCreate(cmd, *server, artifactCreateOptions{
				FilePath:    filePath,
				Visibility:  visibility,
				Expires:     expires,
				ContentType: contentType,
				JSON:        jsonOutput,
			})
		},
	}

	cmd.Flags().StringVar(&filePath, "file", "", "HTML file to upload")
	cmd.Flags().StringVar(&visibility, "visibility", "", "Artifact visibility: org or signed")
	cmd.Flags().StringVar(&expires, "expires", "", "Artifact expiration duration, for example 24h or 7d")
	cmd.Flags().StringVar(&contentType, "content-type", "", "Artifact content type")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print the full JSON response")
	_ = cmd.MarkFlagRequired("file")

	return cmd
}

type artifactCreateOptions struct {
	FilePath    string
	Visibility  string
	Expires     string
	ContentType string
	JSON        bool
}

func runArtifactCreate(cmd *cobra.Command, serverFlag string, opts artifactCreateOptions) error {
	creds, err := config.Load()
	if err != nil {
		if errors.Is(err, config.ErrNotLoggedIn) {
			return errors.New("not logged in")
		}

		return err
	}

	server, err := config.ResolveServer(serverFlag)
	if err != nil {
		return err
	}
	if server != config.CleanServer(creds.Server) {
		return fmt.Errorf("not logged in to %s; run capstan login --server %s", server, server)
	}

	request, err := buildArtifactCreateRequest(opts)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	response, err := api.NewClient(server).CreateArtifact(ctx, creds.Token, request)
	if err != nil {
		return err
	}

	if opts.JSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
	}

	_, err = fmt.Fprintln(cmd.OutOrStdout(), response.ShareURL)

	return err
}

func buildArtifactCreateRequest(opts artifactCreateOptions) (api.ArtifactCreateRequest, error) {
	content, err := readArtifactFile(opts.FilePath)
	if err != nil {
		return api.ArtifactCreateRequest{}, err
	}

	contentType, err := resolveArtifactContentType(opts.FilePath, opts.ContentType)
	if err != nil {
		return api.ArtifactCreateRequest{}, err
	}

	visibility, err := resolveArtifactVisibility(opts.Visibility)
	if err != nil {
		return api.ArtifactCreateRequest{}, err
	}

	expiresAt, err := resolveArtifactExpiresAt(opts.Expires)
	if err != nil {
		return api.ArtifactCreateRequest{}, err
	}

	return api.ArtifactCreateRequest{
		Content:     content,
		ContentType: contentType,
		Visibility:  visibility,
		ExpiresAt:   expiresAt,
	}, nil
}

func readArtifactFile(filePath string) (string, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return "", err
	}
	if info.Size() > maxArtifactBytes {
		return "", fmt.Errorf("artifact file is %d bytes; limit is %d bytes", info.Size(), maxArtifactBytes)
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	return string(content), nil
}

func resolveArtifactContentType(filePath, contentType string) (string, error) {
	if contentType == "" {
		if strings.EqualFold(filepath.Ext(filePath), ".xhtml") {
			return "application/xhtml+xml", nil
		}

		return "text/html", nil
	}

	if contentType == "text/html" || contentType == "application/xhtml+xml" {
		return contentType, nil
	}

	return "", fmt.Errorf("invalid content type %q: use text/html or application/xhtml+xml", contentType)
}

func resolveArtifactVisibility(visibility string) (string, error) {
	switch visibility {
	case "":
		return "", nil
	case "org":
		return "org_auth", nil
	case "signed":
		return "signed_url", nil
	default:
		return "", fmt.Errorf("invalid visibility %q: use org or signed", visibility)
	}
}

func resolveArtifactExpiresAt(expires string) (string, error) {
	if expires == "" {
		return "", nil
	}

	duration, err := parseArtifactDuration(expires)
	if err != nil {
		return "", err
	}
	if duration <= 0 {
		return "", errors.New("expires duration must be greater than zero")
	}

	return time.Now().UTC().Add(duration).Format(time.RFC3339), nil
}

func parseArtifactDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid expires duration %q", value)
		}

		return time.Duration(days) * 24 * time.Hour, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid expires duration %q", value)
	}

	return duration, nil
}
