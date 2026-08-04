package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DevicePrompt is safe to show to the user during device-code login.
type DevicePrompt struct {
	UserCode                string
	VerificationURIComplete string
}

// DeviceOptions configures the RFC-8628-style device-code login flow.
type DeviceOptions struct {
	Server  string
	Label   string
	Timeout time.Duration
	Prompt  func(DevicePrompt) error
	Sleep   func(context.Context, time.Duration) error
}

// DeviceLogin polls the Capstan device-code endpoints until a token is issued or the flow fails.
func DeviceLogin(ctx context.Context, opts DeviceOptions) (string, error) {
	if opts.Prompt == nil {
		return "", errors.New("device prompt is not configured")
	}

	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}

	device, err := createDevice(ctx, opts.Server, opts.Label)
	if err != nil {
		return "", err
	}

	if err := opts.Prompt(DevicePrompt{
		UserCode:                device.UserCode,
		VerificationURIComplete: device.VerificationURIComplete,
	}); err != nil {
		return "", err
	}

	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	deadline := time.Duration(device.ExpiresIn) * time.Second
	if opts.Timeout > 0 && (deadline <= 0 || opts.Timeout < deadline) {
		deadline = opts.Timeout
	}
	if deadline <= 0 {
		return "", errors.New("device login failed: invalid expiration")
	}

	elapsed := time.Duration(0)
	for {
		if elapsed >= deadline {
			return "", errors.New("device login expired waiting for approval")
		}

		token, pollErr := pollDeviceToken(ctx, opts.Server, device.DeviceCode)
		if pollErr == nil {
			return token, nil
		}

		var pendingErr devicePendingError
		if !errors.As(pollErr, &pendingErr) {
			return "", pollErr
		}

		if pendingErr.errCode == "slow_down" {
			interval += 5 * time.Second
		}

		if elapsed+interval > deadline {
			return "", errors.New("device login expired waiting for approval")
		}

		if err := sleep(ctx, interval); err != nil {
			return "", errors.New("device login cancelled while waiting for approval")
		}
		elapsed += interval
	}
}

type deviceCreateResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type deviceTokenResponse struct {
	Token string `json:"token"`
}

type deviceErrorResponse struct {
	Error string `json:"error"`
}

type devicePendingError struct {
	errCode string
}

func (e devicePendingError) Error() string {
	return e.errCode
}

func createDevice(ctx context.Context, server, label string) (deviceCreateResponse, error) {
	body, err := json.Marshal(struct {
		Label string `json:"label"`
	}{Label: label})
	if err != nil {
		return deviceCreateResponse{}, err
	}

	resp, err := postJSON(ctx, server+"/api/v1/cli/device", body)
	if err != nil {
		return deviceCreateResponse{}, err
	}
	defer closeBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return deviceCreateResponse{}, fmt.Errorf("device login failed: server returned status %d", resp.StatusCode)
	}

	var device deviceCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&device); err != nil {
		return deviceCreateResponse{}, fmt.Errorf("device login failed: decode response: %w", err)
	}

	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURIComplete == "" {
		return deviceCreateResponse{}, errors.New("device login failed: incomplete server response")
	}

	return device, nil
}

func pollDeviceToken(ctx context.Context, server, deviceCode string) (string, error) {
	body, err := json.Marshal(struct {
		DeviceCode string `json:"device_code"`
	}{DeviceCode: deviceCode})
	if err != nil {
		return "", err
	}

	resp, err := postJSON(ctx, server+"/api/v1/cli/device/token", body)
	if err != nil {
		return "", err
	}
	defer closeBody(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var tokenResp deviceTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
			return "", fmt.Errorf("device login failed: decode token response: %w", err)
		}
		if tokenResp.Token == "" {
			return "", errors.New("device login failed: token response was empty")
		}

		return tokenResp.Token, nil
	}

	if resp.StatusCode == http.StatusBadRequest {
		var errorResp deviceErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errorResp); err != nil {
			return "", fmt.Errorf("device login failed: decode error response: %w", err)
		}

		switch errorResp.Error {
		case "authorization_pending", "slow_down":
			return "", devicePendingError{errCode: errorResp.Error}
		case "expired_token":
			return "", errors.New("device login failed: code expired")
		case "access_denied":
			return "", errors.New("device login failed: access denied")
		case "invalid_grant":
			return "", errors.New("device login failed: invalid grant")
		default:
			return "", errors.New("device login failed: authorization failed")
		}
	}

	return "", fmt.Errorf("device login failed: server returned status %d", resp.StatusCode)
}

func postJSON(ctx context.Context, endpoint string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device login failed: request failed")
	}

	return resp, nil
}

func closeBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
