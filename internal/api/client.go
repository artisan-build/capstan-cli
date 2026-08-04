package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Identity is the authenticated Capstan user returned by /api/v1/me.
type Identity struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ArtifactCreateRequest is the JSON body sent to the artifact ingest API.
type ArtifactCreateRequest struct {
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
	Visibility  string `json:"visibility,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

// ArtifactCreateResponse is the artifact ingest success response.
type ArtifactCreateResponse struct {
	Artifact struct {
		ID          int    `json:"id"`
		AuthorID    int    `json:"author_id"`
		Visibility  string `json:"visibility"`
		ExpiresAt   string `json:"expires_at"`
		ContentType string `json:"content_type"`
		SizeBytes   int    `json:"size_bytes"`
		ContentHash string `json:"content_hash"`
		ShareURL    string `json:"share_url"`
		CreatedAt   string `json:"created_at"`
	} `json:"artifact"`
	ShareURL string `json:"share_url"`
}

// ArtifactError reports a non-201 artifact API response without including credential material.
type ArtifactError struct {
	StatusCode int
	Message    string
}

func (e ArtifactError) Error() string {
	if e.Message != "" {
		return e.Message
	}

	return fmt.Sprintf("artifact upload failed with status %d", e.StatusCode)
}

// StatusError reports a non-200 API response without including any credential material.
type StatusError struct {
	StatusCode int
}

func (e StatusError) Error() string {
	return fmt.Sprintf("server returned status %d", e.StatusCode)
}

// Client is a small Capstan API client.
type Client struct {
	server     string
	httpClient *http.Client
}

// NewClient returns a Capstan API client for a clean server base URL.
func NewClient(server string) Client {
	return Client{
		server: server,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Me fetches the identity for token.
func (c Client) Me(ctx context.Context, token string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+"/api/v1/me", http.NoBody)
	if err != nil {
		return Identity{}, err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return Identity{}, StatusError{StatusCode: resp.StatusCode}
	}

	var identity Identity
	if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
		return Identity{}, fmt.Errorf("decode identity: %w", err)
	}

	return identity, nil
}

// CreateArtifact uploads an artifact to the Capstan artifact ingest API.
func (c Client) CreateArtifact(ctx context.Context, token string, request ArtifactCreateRequest) (ArtifactCreateResponse, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return ArtifactCreateResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server+"/api/v1/artifacts", bytes.NewReader(body))
	if err != nil {
		return ArtifactCreateResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ArtifactCreateResponse{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusCreated {
		return ArtifactCreateResponse{}, ArtifactError{StatusCode: resp.StatusCode, Message: artifactErrorMessage(resp.StatusCode, resp.Body)}
	}

	var response ArtifactCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return ArtifactCreateResponse{}, fmt.Errorf("decode artifact response: %w", err)
	}

	return response, nil
}

func artifactErrorMessage(statusCode int, body io.Reader) string {
	switch statusCode {
	case http.StatusUnauthorized:
		return "authentication failed; run capstan login"
	case http.StatusNotFound:
		return "artifact upload failed: artifacts are disabled on this server"
	case http.StatusUnprocessableEntity:
		return validationErrorMessage(body)
	default:
		return fmt.Sprintf("artifact upload failed with status %d", statusCode)
	}
}

func validationErrorMessage(body io.Reader) string {
	var response struct {
		Error  string              `json:"error"`
		Errors map[string][]string `json:"errors"`
	}
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		return "artifact upload failed: validation error"
	}

	message := response.Error
	for field, messages := range response.Errors {
		for _, fieldMessage := range messages {
			if message != "" {
				message += "; "
			}
			message += field + ": " + fieldMessage
		}
	}
	if message == "" {
		return "artifact upload failed: validation error"
	}

	return message
}
