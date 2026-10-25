package work

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// Steer is the signed worker steer representation; only item-scoped text is delivered.
type Steer struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id"`
	ItemID      string `json:"item_id,omitempty"`
	Text        string `json:"text"`
	AuthorActor string `json:"author_actor"`
	CreatedAt   string `json:"created_at"`
	ReadAt      string `json:"read_at"`
}

// SteerReader fetches the current renewed signed endpoint without reporting progress.
type SteerReader interface {
	Steers(context.Context, string) ([]Steer, error)
}

func (c *Client) Steers(ctx context.Context, signedURL string) ([]Steer, error) {
	u, err := url.Parse(signedURL)
	base, baseErr := url.Parse(c.base)
	if err != nil || baseErr != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" || u.RawQuery == "" {
		return nil, APIError{Code: "invalid_steer_endpoint"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return nil, APIError{Code: "request_failed"}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", UserAgent)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, APIError{Code: "request_failed"}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, APIError{Status: response.StatusCode, Code: "request_failed"}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAmpJSON+1))
	var envelope struct {
		Steers           []Steer `json:"steers"`
		ReleaseRequested *struct {
			RequestedAt string `json:"requested_at"`
			Deadline    string `json:"deadline"`
			Reason      string `json:"reason"`
		} `json:"release_requested,omitempty"`
	}
	if err != nil || strictAmpJSON(data, &envelope) != nil || envelope.Steers == nil || len(envelope.Steers) > 256 {
		return nil, APIError{Code: "invalid_steers"}
	}
	return envelope.Steers, nil
}

func (r *Runner) deliverSteers(ctx context.Context, state *State, id, signedURL string) error {
	item := state.Items[id]
	reader, ok := r.api.(SteerReader)
	if !ok {
		return errors.New("work API cannot fetch correction steers")
	}
	launcher, ok := r.launcher.(MessageLauncher)
	if !ok {
		return ErrUnsupportedAction
	}
	steers, err := reader.Steers(ctx, signedURL)
	if err != nil {
		return err
	}
	delivered := make(map[string]bool, len(item.DeliveredSteers))
	for _, id := range item.DeliveredSteers {
		delivered[id] = true
	}
	seen := make(map[string]bool, len(steers))
	byID := make(map[string]Steer, len(steers))
	// Validate the entire response before delivering any text.
	for _, steer := range steers {
		if !validUUID(steer.ID) || steer.TaskID != item.TaskID || (steer.ItemID != "" && steer.ItemID != id) || !validSessionMessage(steer.Text) || seen[steer.ID] {
			return errors.New("work correction steers are invalid")
		}
		seen[steer.ID] = true
		byID[steer.ID] = steer
	}
	pending := make(map[string]bool, len(item.PendingSteers))
	for _, id := range item.PendingSteers {
		pending[id] = true
	}
	for _, steer := range steers {
		if steer.ItemID != id || delivered[steer.ID] || pending[steer.ID] {
			continue
		}
		if len(item.PendingSteers) >= 256 {
			return errors.New("work pending correction ledger is full")
		}
		item.PendingSteers = append(item.PendingSteers, steer.ID)
		pending[steer.ID] = true
	}
	if len(item.PendingSteers) == 0 {
		if item.CorrectionPending {
			item.CorrectionPending = false
			return r.store.Save(*state)
		}
		return nil
	}
	item.CorrectionPending = true
	if err := r.store.Save(*state); err != nil {
		return err
	}
	for _, steerID := range item.PendingSteers {
		if delivered[steerID] {
			continue
		}
		steer, ok := byID[steerID]
		if !ok || steer.ItemID != id {
			return errors.New("work pending correction steer is unavailable")
		}
		if len(item.DeliveredSteers) >= 256 {
			return errors.New("work correction delivery ledger is full")
		}
		// Pending is durable before execution; a crash after delivery retries rather than loses text.
		if err := launcher.Continue(ctx, item.SessionRef, steer.Text); err != nil {
			return err
		}
		item.DeliveredSteers = append(item.DeliveredSteers, steer.ID)
		if err := r.store.Save(*state); err != nil {
			return err
		}
	}
	item.CorrectionPending = false
	item.PendingSteers = nil
	return r.store.Save(*state)
}
