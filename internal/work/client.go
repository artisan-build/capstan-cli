package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxResponseBytes = 1 << 20

// APIError is safe to log: it contains no response body, URL, bearer, prompt, or signed link.
type APIError struct {
	Status int
	Code   string
}

func (e APIError) Error() string {
	status := "network"
	if e.Status != 0 {
		status = fmt.Sprintf("%d", e.Status)
	}
	code := e.Code
	if code == "" {
		code = "request_failed"
	}

	return fmt.Sprintf("work API request failed (%s %s)", status, code)
}

// HeldItem is one fenced local holding reported on a claim tick.
type HeldItem struct {
	ItemID           string `json:"item_id"`
	Fence            int    `json:"fence"`
	DirectiveID      string `json:"directive_id,omitempty"`
	DirectiveOutcome string `json:"directive_outcome,omitempty"`
	LaunchNonce      string `json:"launch_nonce,omitempty"`
	SessionRef       string `json:"session_ref,omitempty"`
}

// ClaimRequest is the complete split-capacity heartbeat sent every claim tick.
type ClaimRequest struct {
	Capacity                int               `json:"capacity"`
	OrchestratorCapacity    int               `json:"orchestrator_capacity"`
	Roles                   []string          `json:"roles"`
	Capabilities            []string          `json:"capabilities"`
	RoleProviders           map[string]string `json:"role_providers"`
	CurrentClaimedItemCount int               `json:"current_claimed_item_count"`
	HeldItems               []HeldItem        `json:"held_items"`
}

// ClaimItem is one server-fenced item returned by claim.
type ClaimItem struct {
	ID          string            `json:"id"`
	TaskID      string            `json:"task_id"`
	Role        string            `json:"role"`
	Fence       int               `json:"fence"`
	LeaseUntil  string            `json:"lease_until"`
	Links       map[string]string `json:"links"`
	SessionRef  string            `json:"session_ref,omitempty"`
	SeatFence   int               `json:"seat_fence,omitempty"`
	LaunchNonce string            `json:"launch_nonce"`
}

// Directive is one server-selected session repair.
type Directive struct {
	ID       string            `json:"id"`
	ItemID   string            `json:"item_id"`
	Fence    int               `json:"fence"`
	Sequence int               `json:"sequence"`
	Kind     string            `json:"kind"`
	Reason   string            `json:"reason"`
	Payload  DirectivePayload  `json:"payload"`
	Links    map[string]string `json:"links,omitempty"`
	IssuedAt string            `json:"issued_at"`
}

// DirectivePayload is the bounded directive payload currently defined by the server.
type DirectivePayload struct {
	LaunchNonce string `json:"launch_nonce,omitempty"`
}

// ClaimResponse is one tick response.
type ClaimResponse struct {
	Items      []ClaimItem `json:"items"`
	Directives []Directive `json:"directives"`
	SpokeID    *int64      `json:"spoke_id"`
}

// RenewRequestItem is one fenced item in a bounded renewal batch.
type RenewRequestItem struct {
	ItemID string `json:"item_id"`
	Fence  int    `json:"fence"`
}

// RenewedItem is the current server view returned with fresh signed links.
type RenewedItem struct {
	ID         string            `json:"id"`
	TaskID     string            `json:"task_id"`
	Role       string            `json:"role"`
	State      string            `json:"state"`
	Fence      int               `json:"fence"`
	LeaseUntil string            `json:"lease_until,omitempty"`
	Links      map[string]string `json:"links"`
}

// RenewResult is either renewed with an item or lost without one.
type RenewResult struct {
	ItemID string       `json:"item_id"`
	Status string       `json:"status"`
	Item   *RenewedItem `json:"item,omitempty"`
}

// TransitionRequest moves one fenced item and carries launch metadata only for delivered.
type TransitionRequest struct {
	ItemID      string `json:"item_id"`
	Fence       int    `json:"fence"`
	State       string `json:"state"`
	LaunchNonce string `json:"launch_nonce,omitempty"`
	SessionRef  string `json:"session_ref,omitempty"`
}

// WorkAPI is the protocol seam used by Runner and its fake-server tests.
type WorkAPI interface {
	Claim(context.Context, ClaimRequest) (ClaimResponse, error)
	Renew(context.Context, []RenewRequestItem) ([]RenewResult, error)
	Transition(context.Context, TransitionRequest) error
}

// Client is a bounded, no-redirect Work API client.
type Client struct {
	base       string
	actorID    string
	bearer     string
	httpClient *http.Client
}

// NewClient constructs a client for a validated configuration and bound bearer.
func NewClient(config Config, bearer string) *Client {
	return &Client{
		base:    config.CapstanURL + "/api/v1/work/" + url.PathEscape(config.Team),
		actorID: config.ActorID,
		bearer:  bearer,
		httpClient: &http.Client{
			Timeout: config.RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Claim sends one explicit split-capacity tick.
func (c *Client) Claim(ctx context.Context, request ClaimRequest) (ClaimResponse, error) {
	if request.Roles == nil {
		request.Roles = []string{}
	}
	if request.Capabilities == nil {
		request.Capabilities = []string{}
	}
	if request.RoleProviders == nil {
		request.RoleProviders = map[string]string{}
	}
	if request.HeldItems == nil {
		request.HeldItems = []HeldItem{}
	}
	var response ClaimResponse
	if err := c.post(ctx, "claim", request, &response); err != nil {
		return ClaimResponse{}, err
	}
	if response.Items == nil || response.Directives == nil {
		return ClaimResponse{}, APIError{Code: "invalid_claim_response"}
	}

	return response, nil
}

// Renew renews at most 25 fenced items.
func (c *Client) Renew(ctx context.Context, items []RenewRequestItem) ([]RenewResult, error) {
	if len(items) == 0 || len(items) > MaxBatch {
		return nil, errors.New("renew batch must contain from 1 through 25 items")
	}
	var response struct {
		Results []RenewResult `json:"results"`
	}
	if err := c.post(ctx, "renew", struct {
		Items []RenewRequestItem `json:"items"`
	}{Items: items}, &response); err != nil {
		return nil, err
	}
	if response.Results == nil {
		return nil, APIError{Code: "invalid_renew_response"}
	}

	return response.Results, nil
}

// Transition sends one fenced state transition.
func (c *Client) Transition(ctx context.Context, request TransitionRequest) error {
	var response struct {
		Item struct {
			ID         string `json:"id"`
			TaskID     string `json:"task_id"`
			Role       string `json:"role"`
			State      string `json:"state"`
			Fence      int    `json:"fence"`
			LeaseUntil string `json:"lease_until,omitempty"`
		} `json:"item"`
	}

	return c.post(ctx, "transition", request, &response)
}

func (c *Client) post(ctx context.Context, action string, payload, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return APIError{Code: "request_failed"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+action, bytes.NewReader(body))
	if err != nil {
		return APIError{Code: "request_failed"}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.bearer)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", UserAgent)
	request.Header.Set("X-Capstan-Actor-ID", c.actorID)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return APIError{Code: "request_failed"}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))

		return APIError{Status: response.StatusCode, Code: "request_failed"}
	}

	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > maxResponseBytes || validateUniqueJSON(data) != nil {
		return APIError{Code: "invalid_response"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || expectJSONEOF(decoder) != nil {
		return APIError{Code: "invalid_response"}
	}

	return nil
}

func validateLinks(role string, links map[string]string) error {
	required := []string{"brief", "milestone", "steers", "evidence"}
	if role == "orchestrator" {
		required = append(required, "orchestration", "orchestrate")
	}
	for _, key := range required {
		value, ok := links[key]
		if !ok {
			return errors.New("signed link set is incomplete")
		}
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery == "" {
			return errors.New("signed link set is invalid")
		}
	}
	for key := range links {
		if strings.TrimSpace(key) == "" {
			return errors.New("signed link set is invalid")
		}
	}

	return nil
}
