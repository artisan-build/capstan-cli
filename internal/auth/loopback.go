package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
		Handler:           callbackHandler(state, result),
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

func callbackHandler(state string, result chan<- loginResult) http.Handler {
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

		token := query.Get("token")
		if token == "" {
			writeCallbackPage(w, "Login failed. You may close this tab.")
			result <- loginResult{err: errors.New("login failed: callback did not include a token")}

			return
		}

		writeCallbackPage(w, "Login complete. You may close this tab.")
		result <- loginResult{token: token}
	})
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
