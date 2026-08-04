package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// BrowserOpener opens an authorization URL in the user's browser.
type BrowserOpener func(string) error

// LoopbackOptions configures the loopback browser login flow.
type LoopbackOptions struct {
	Server string
	Label  string
	Open   BrowserOpener
}

// LoopbackLogin captures a PAT from the Capstan loopback authorization flow.
// The browser callback carries a one-time code; the PAT itself is only ever
// received in the body of the code-exchange API response, never in a URL.
func LoopbackLogin(ctx context.Context, opts LoopbackOptions) (string, error) {
	if opts.Open == nil {
		return "", errors.New("browser opener is not configured")
	}

	state, err := randomState()
	if err != nil {
		return "", err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	redirectURL := "http://" + listener.Addr().String() + "/callback"
	result := make(chan loginResult, 1)
	server := &http.Server{
		Handler:           callbackHandler(ctx, opts.Server, redirectURL, state, result),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			result <- loginResult{err: errors.New("login listener failed")}
		}
	}()

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-serveDone
	}()

	authorizeURL, err := buildAuthorizeURL(opts.Server, redirectURL, state, opts.Label)
	if err != nil {
		return "", err
	}

	if err := opts.Open(authorizeURL); err != nil {
		return "", fmt.Errorf("open browser: %w", err)
	}

	select {
	case res := <-result:
		if res.err != nil {
			return "", res.err
		}

		return res.token, nil
	case <-ctx.Done():
		return "", errors.New("login timed out waiting for browser approval")
	}
}

type loginResult struct {
	token string
	err   error
}

func callbackHandler(ctx context.Context, server, redirectURL, state string, result chan<- loginResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)

			return
		}

		query := r.URL.Query()
		if query.Get("state") != state {
			writeCallbackPage(w, "Login failed. You may close this tab.")
			result <- loginResult{err: errors.New("login failed: state mismatch")}

			return
		}

		if accessError := query.Get("error"); accessError != "" {
			writeCallbackPage(w, "Login was not approved. You may close this tab.")
			result <- loginResult{err: fmt.Errorf("login failed: %s", accessError)}

			return
		}

		code := query.Get("code")
		if code == "" {
			writeCallbackPage(w, "Login failed. You may close this tab.")
			result <- loginResult{err: errors.New("login failed: callback did not include a code")}

			return
		}

		token, err := exchangeAuthorizationCode(ctx, server, code, redirectURL)
		if err != nil {
			writeCallbackPage(w, "Login failed. You may close this tab.")
			result <- loginResult{err: err}

			return
		}

		writeCallbackPage(w, "Login complete. You may close this tab.")
		result <- loginResult{token: token}
	})
}

type authorizeTokenResponse struct {
	Token string `json:"token"`
}

type authorizeErrorResponse struct {
	Error string `json:"error"`
}

// exchangeAuthorizationCode trades the one-time callback code for a PAT over
// the API. The redirect URI must be byte-identical to the one sent in the
// authorize request.
func exchangeAuthorizationCode(ctx context.Context, server, code, redirectURI string) (string, error) {
	body, err := json.Marshal(struct {
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
	}{Code: code, RedirectURI: redirectURI})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/api/v1/cli/authorize/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("login failed: code exchange request failed")
	}
	defer closeBody(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var tokenResp authorizeTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
			return "", fmt.Errorf("login failed: decode token response: %w", err)
		}
		if tokenResp.Token == "" {
			return "", errors.New("login failed: token response was empty")
		}

		return tokenResp.Token, nil
	}

	if resp.StatusCode == http.StatusBadRequest {
		var errorResp authorizeErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errorResp); err != nil {
			return "", fmt.Errorf("login failed: decode error response: %w", err)
		}

		switch errorResp.Error {
		case "expired_token":
			return "", errors.New("login failed: authorization code expired")
		case "invalid_grant":
			return "", errors.New("login failed: invalid authorization code")
		default:
			return "", errors.New("login failed: code exchange was rejected")
		}
	}

	return "", fmt.Errorf("login failed: code exchange returned status %d", resp.StatusCode)
}

func buildAuthorizeURL(server, redirectURL, state, label string) (string, error) {
	authorizeURL, err := url.Parse(server + "/cli/authorize")
	if err != nil {
		return "", err
	}

	query := authorizeURL.Query()
	query.Set("redirect_uri", redirectURL)
	query.Set("state", state)
	query.Set("label", label)
	authorizeURL.RawQuery = query.Encode()

	return authorizeURL.String(), nil
}

func randomState() (string, error) {
	state := make([]byte, 32)
	if _, err := rand.Read(state); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(state), nil
}

func writeCallbackPage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, "<!doctype html><html><body><p>%s</p></body></html>", message)
}
