package api

import (
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
