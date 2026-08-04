package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/artisan-build/capstan-cli/internal/config"
)

const testToken = "test-token-123"
const testDeviceCode = "test-device-code-123"
const testAuthCode = "test-auth-code-123"

func TestLoginSuccessfulFlowStoresCredentials(t *testing.T) {
	setTestConfigHome(t)

	server, recorder := newAuthTestServer(t, authServerOptions{})
	defer server.Close()

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "login", "--label", "test-host"}, func(authorizeURL string) error {
		recorder.recordURL(authorizeURL)

		return openerGET(t)(authorizeURL)
	})
	if err != nil {
		t.Fatalf("login returned error: %v", err)
	}
	assertNoTokenLeak(t, stdout, stderr)

	if got := recorder.exchangeCalls.Load(); got != 1 {
		t.Fatalf("exchange endpoint called %d times, want 1", got)
	}
	for _, handled := range recorder.handledURLs() {
		if strings.Contains(handled, testToken) {
			t.Fatalf("URL leaked token: %q", handled)
		}
	}

	creds, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if creds.Token != testToken {
		t.Fatal("stored token did not match captured token")
	}
	if creds.Server != server.URL {
		t.Fatalf("stored server = %q, want %q", creds.Server, server.URL)
	}

	path, err := config.Path()
	if err != nil {
		t.Fatalf("Path returned error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credentials file mode = %v, want 0600", got)
	}
}

func TestLoginStateMismatchFailsWithoutCredentials(t *testing.T) {
	setTestConfigHome(t)

	server, recorder := newAuthTestServer(t, authServerOptions{AuthorizeState: "wrong-state"})
	defer server.Close()

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "login"}, openerGET(t))
	if err == nil {
		t.Fatal("login succeeded with state mismatch")
	}
	if !strings.Contains(stderr, "state mismatch") {
		t.Fatalf("stderr = %q, want state mismatch message", stderr)
	}
	if got := recorder.exchangeCalls.Load(); got != 0 {
		t.Fatalf("exchange endpoint called %d times after state mismatch, want 0", got)
	}
	assertNoTokenLeak(t, stdout, stderr)
	assertNoCredentialsFile(t)
}

func TestLoginAccessDeniedFailsWithoutCredentials(t *testing.T) {
	setTestConfigHome(t)

	server, recorder := newAuthTestServer(t, authServerOptions{AuthorizeError: "access_denied"})
	defer server.Close()

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "login"}, openerGET(t))
	if err == nil {
		t.Fatal("login succeeded after access_denied callback")
	}
	if !strings.Contains(stderr, "access_denied") {
		t.Fatalf("stderr = %q, want access_denied message", stderr)
	}
	if got := recorder.exchangeCalls.Load(); got != 0 {
		t.Fatalf("exchange endpoint called %d times after access_denied, want 0", got)
	}
	assertNoTokenLeak(t, stdout, stderr)
	assertNoCredentialsFile(t)
}

func TestLoginExchangeErrorsFailWithoutCredentials(t *testing.T) {
	for errorCode, wantMessage := range map[string]string{
		"expired_token": "expired",
		"invalid_grant": "invalid",
	} {
		t.Run(errorCode, func(t *testing.T) {
			setTestConfigHome(t)

			server, recorder := newAuthTestServer(t, authServerOptions{TokenError: errorCode})
			defer server.Close()

			stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "login"}, openerGET(t))
			if err == nil {
				t.Fatalf("login succeeded after exchange returned %s", errorCode)
			}
			if !strings.Contains(stderr, wantMessage) {
				t.Fatalf("stderr = %q, want message containing %q", stderr, wantMessage)
			}
			if got := recorder.exchangeCalls.Load(); got != 1 {
				t.Fatalf("exchange endpoint called %d times, want 1", got)
			}
			assertNoTokenLeak(t, stdout, stderr)
			assertNoCredentialsFile(t)
		})
	}
}

func TestLoginMeFailureFailsClosedWithoutCredentials(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("status %d", statusCode), func(t *testing.T) {
			setTestConfigHome(t)

			server, _ := newAuthTestServer(t, authServerOptions{MeStatus: statusCode})
			defer server.Close()

			stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "login"}, openerGET(t))
			if err == nil {
				t.Fatalf("login succeeded with /me status %d", statusCode)
			}
			assertNoTokenLeak(t, stdout, stderr)
			assertNoCredentialsFile(t)
		})
	}
}

func TestLoginTimeoutFailsCleanly(t *testing.T) {
	setTestConfigHome(t)

	stdout, stderr, err := executeCLI(t, []string{"--server", "https://server.example.test", "login", "--timeout", "20ms"}, func(string) error {
		return nil
	})
	if err == nil {
		t.Fatal("login succeeded without callback")
	}
	if !strings.Contains(stderr, "timed out") {
		t.Fatalf("stderr = %q, want timeout message", stderr)
	}
	assertNoTokenLeak(t, stdout, stderr)
	assertNoCredentialsFile(t)
}

func TestDeviceLoginHappyPathStoresCredentials(t *testing.T) {
	setTestConfigHome(t)

	server := newDeviceTestServer(t, deviceServerOptions{
		TokenErrors: []string{"authorization_pending", "authorization_pending"},
		MeStatus:    http.StatusOK,
	})
	defer server.Close()

	stdout, stderr, err := executeCLIWithDeviceSleep(t, []string{"--server", server.URL, "login", "--device", "--label", "test-host"}, recordSleeps(nil))
	if err != nil {
		t.Fatalf("login --device returned error: %v", err)
	}
	if !strings.Contains(stdout, "USER-CODE") || !strings.Contains(stdout, "https://verify.example.test/complete") {
		t.Fatalf("stdout = %q, want user code and verification URL", stdout)
	}
	assertNoSecretLeak(t, stdout, stderr)

	creds, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if creds.Token != testToken {
		t.Fatal("stored token did not match issued token")
	}
	if creds.Server != server.URL {
		t.Fatalf("stored server = %q, want %q", creds.Server, server.URL)
	}

	path, err := config.Path()
	if err != nil {
		t.Fatalf("Path returned error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credentials file mode = %v, want 0600", got)
	}
}

func TestDeviceLoginSlowDownIncreasesPollInterval(t *testing.T) {
	setTestConfigHome(t)

	server := newDeviceTestServer(t, deviceServerOptions{
		TokenErrors: []string{"slow_down"},
		MeStatus:    http.StatusOK,
	})
	defer server.Close()

	var delays []time.Duration
	_, _, err := executeCLIWithDeviceSleep(t, []string{"--server", server.URL, "login", "--device"}, recordSleeps(&delays))
	if err != nil {
		t.Fatalf("login --device returned error: %v", err)
	}
	if len(delays) == 0 {
		t.Fatal("no polling delays were recorded")
	}
	if delays[0] <= time.Second {
		t.Fatalf("first delay after slow_down = %v, want > 1s", delays[0])
	}
}

func TestDeviceLoginTerminalErrorsFailWithoutCredentials(t *testing.T) {
	for _, errorCode := range []string{"expired_token", "access_denied", "invalid_grant"} {
		t.Run(errorCode, func(t *testing.T) {
			setTestConfigHome(t)

			server := newDeviceTestServer(t, deviceServerOptions{
				TokenErrors: []string{errorCode},
				MeStatus:    http.StatusOK,
			})
			defer server.Close()

			stdout, stderr, err := executeCLIWithDeviceSleep(t, []string{"--server", server.URL, "login", "--device"}, recordSleeps(nil))
			if err == nil {
				t.Fatalf("login --device succeeded after %s", errorCode)
			}
			assertNoSecretLeak(t, stdout, stderr)
			assertNoCredentialsFile(t)
		})
	}
}

func TestDeviceLoginExpiresWhilePending(t *testing.T) {
	setTestConfigHome(t)

	server := newDeviceTestServer(t, deviceServerOptions{
		TokenErrors: []string{"authorization_pending", "authorization_pending", "authorization_pending"},
		ExpiresIn:   2,
		MeStatus:    http.StatusOK,
	})
	defer server.Close()

	stdout, stderr, err := executeCLIWithDeviceSleep(t, []string{"--server", server.URL, "login", "--device"}, recordSleeps(nil))
	if err == nil {
		t.Fatal("login --device succeeded after expiration")
	}
	if !strings.Contains(stderr, "expired") {
		t.Fatalf("stderr = %q, want expiration message", stderr)
	}
	assertNoSecretLeak(t, stdout, stderr)
	assertNoCredentialsFile(t)
}

func TestDeviceLoginMeUnauthorizedFailsClosed(t *testing.T) {
	setTestConfigHome(t)

	server := newDeviceTestServer(t, deviceServerOptions{MeStatus: http.StatusUnauthorized})
	defer server.Close()

	stdout, stderr, err := executeCLIWithDeviceSleep(t, []string{"--server", server.URL, "login", "--device"}, recordSleeps(nil))
	if err == nil {
		t.Fatal("login --device succeeded with /me 401")
	}
	assertNoSecretLeak(t, stdout, stderr)
	assertNoCredentialsFile(t)
}

func TestDeviceLoginInvalidServerFails(t *testing.T) {
	setTestConfigHome(t)

	stdout, stderr, err := executeCLIWithDeviceSleep(t, []string{"--server", "garbage", "login", "--device"}, recordSleeps(nil))
	if err == nil {
		t.Fatal("login --device succeeded with invalid server")
	}
	if !strings.Contains(stderr, "invalid server URL") {
		t.Fatalf("stderr = %q, want invalid server message", stderr)
	}
	assertNoSecretLeak(t, stdout, stderr)
}

func TestWhoamiPrintsHumanAndJSONIdentity(t *testing.T) {
	setTestConfigHome(t)

	server := newWhoamiTestServer(t, http.StatusOK)
	defer server.Close()

	if err := config.Save(config.Credentials{Token: testToken, Server: server.URL}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	stdout, stderr, err := executeCLI(t, []string{"whoami"}, nil)
	if err != nil {
		t.Fatalf("whoami returned error: %v", err)
	}
	if !strings.Contains(stdout, "Ada Lovelace") || !strings.Contains(stdout, "ada@example.test") {
		t.Fatalf("stdout = %q, want name and email", stdout)
	}
	assertNoTokenLeak(t, stdout, stderr)

	stdout, stderr, err = executeCLI(t, []string{"whoami", "--json"}, nil)
	if err != nil {
		t.Fatalf("whoami --json returned error: %v", err)
	}

	var identity struct {
		ID    int    `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal([]byte(stdout), &identity); err != nil {
		t.Fatalf("stdout was not valid JSON: %v", err)
	}
	if identity.ID != 42 || identity.Name != "Ada Lovelace" || identity.Email != "ada@example.test" {
		t.Fatalf("identity = %#v, want fake identity", identity)
	}
	assertNoTokenLeak(t, stdout, stderr)
}

func TestWhoamiWithoutCredentialsFailsNotLoggedIn(t *testing.T) {
	setTestConfigHome(t)

	stdout, stderr, err := executeCLI(t, []string{"whoami"}, nil)
	if err == nil {
		t.Fatal("whoami succeeded without credentials")
	}
	if !strings.Contains(stderr, "not logged in") {
		t.Fatalf("stderr = %q, want not logged in", stderr)
	}
	assertNoTokenLeak(t, stdout, stderr)
}

func TestWhoamiUnauthorizedSuggestsLogin(t *testing.T) {
	setTestConfigHome(t)

	server := newWhoamiTestServer(t, http.StatusUnauthorized)
	defer server.Close()

	if err := config.Save(config.Credentials{Token: testToken, Server: server.URL}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	stdout, stderr, err := executeCLI(t, []string{"whoami"}, nil)
	if err == nil {
		t.Fatal("whoami succeeded with 401")
	}
	if !strings.Contains(stderr, "capstan login") {
		t.Fatalf("stderr = %q, want login suggestion", stderr)
	}
	assertNoTokenLeak(t, stdout, stderr)
}

func TestWhoamiDifferentServerDoesNotSendToken(t *testing.T) {
	setTestConfigHome(t)

	serverA := newWhoamiTestServer(t, http.StatusOK)
	defer serverA.Close()

	var requestsToB atomic.Int64
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsToB.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()

	if err := config.Save(config.Credentials{Token: testToken, Server: serverA.URL}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	stdout, stderr, err := executeCLI(t, []string{"--server", serverB.URL, "whoami"}, nil)
	if err == nil {
		t.Fatal("whoami succeeded for a different server")
	}
	if !strings.Contains(stderr, "capstan login --server") {
		t.Fatalf("stderr = %q, want login guidance", stderr)
	}
	if got := requestsToB.Load(); got != 0 {
		t.Fatalf("server B received %d requests, want 0", got)
	}
	assertNoTokenLeak(t, stdout, stderr)
}

func TestLogoutRemovesCredentialsAndIsIdempotent(t *testing.T) {
	setTestConfigHome(t)

	if err := config.Save(config.Credentials{Token: testToken, Server: "https://server.example.test"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	if _, _, err := executeCLI(t, []string{"logout"}, nil); err != nil {
		t.Fatalf("logout returned error: %v", err)
	}
	assertNoCredentialsFile(t)

	if _, _, err := executeCLI(t, []string{"logout"}, nil); err != nil {
		t.Fatalf("second logout returned error: %v", err)
	}
}

func executeCLI(t *testing.T, args []string, opener func(string) error) (string, string, error) {
	t.Helper()

	oldOpenBrowser := openBrowser
	if opener != nil {
		openBrowser = opener
	}
	t.Cleanup(func() { openBrowser = oldOpenBrowser })

	cmd := newRootCommand()
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs(args)

	err := cmd.Execute()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
	}

	return stdout.String(), stderr.String(), err
}

func executeCLIWithDeviceSleep(t *testing.T, args []string, sleep func(context.Context, time.Duration) error) (string, string, error) {
	t.Helper()

	oldDeviceSleep := deviceSleep
	deviceSleep = sleep
	t.Cleanup(func() { deviceSleep = oldDeviceSleep })

	return executeCLI(t, args, nil)
}

type authServerOptions struct {
	AuthorizeError string
	AuthorizeState string
	TokenError     string
	MeStatus       int
}

type authServerRecorder struct {
	exchangeCalls atomic.Int64

	mu   sync.Mutex
	urls []string
}

func (r *authServerRecorder) recordURL(handled string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, handled)
}

func (r *authServerRecorder) handledURLs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.urls...)
}

func newAuthTestServer(t *testing.T, opts authServerOptions) (*httptest.Server, *authServerRecorder) {
	t.Helper()

	if opts.MeStatus == 0 {
		opts.MeStatus = http.StatusOK
	}

	recorder := &authServerRecorder{}
	var issuedRedirectURI atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cli/authorize":
			recorder.recordURL(r.URL.String())

			redirectURI := r.URL.Query().Get("redirect_uri")
			state := r.URL.Query().Get("state")
			if state == "" {
				t.Fatal("authorize request missing state")
			}
			if len(state) < 43 {
				t.Fatalf("state length = %d, want base64url 32-byte state", len(state))
			}

			parsedRedirect, err := url.Parse(redirectURI)
			if err != nil {
				t.Fatalf("redirect_uri was invalid: %v", err)
			}
			if parsedRedirect.Hostname() != "127.0.0.1" {
				t.Fatalf("redirect_uri host = %q, want 127.0.0.1", parsedRedirect.Hostname())
			}
			if parsedRedirect.Path != "/callback" {
				t.Fatalf("redirect_uri path = %q, want /callback", parsedRedirect.Path)
			}

			issuedRedirectURI.Store(redirectURI)

			callbackState := state
			if opts.AuthorizeState != "" {
				callbackState = opts.AuthorizeState
			}

			var callback string
			if opts.AuthorizeError != "" {
				callback = redirectURI + "?error=" + url.QueryEscape(opts.AuthorizeError) + "&state=" + url.QueryEscape(callbackState)
			} else {
				callback = redirectURI + "?code=" + url.QueryEscape(testAuthCode) + "&state=" + url.QueryEscape(callbackState)
			}
			recorder.recordURL(callback)
			http.Redirect(w, r, callback, http.StatusFound)
		case "/api/v1/cli/authorize/token":
			recorder.recordURL(r.URL.String())
			recorder.exchangeCalls.Add(1)

			if r.Method != http.MethodPost {
				t.Fatalf("exchange method = %s, want POST", r.Method)
			}

			var body struct {
				Code        string `json:"code"`
				RedirectURI string `json:"redirect_uri"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode exchange body: %v", err)
			}
			if body.Code != testAuthCode {
				t.Fatal("exchange body did not include expected code")
			}

			expectedRedirect, _ := issuedRedirectURI.Load().(string)
			if expectedRedirect == "" || body.RedirectURI != expectedRedirect {
				t.Fatalf("exchange redirect_uri = %q, want %q", body.RedirectURI, expectedRedirect)
			}

			w.Header().Set("Content-Type", "application/json")
			if opts.TokenError != "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"error":%q}`, opts.TokenError)

				return
			}

			_, _ = fmt.Fprintf(w, `{"token":%q}`, testToken)
		case "/api/v1/me":
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				t.Fatal("/me request missing expected authorization header")
			}
			w.WriteHeader(opts.MeStatus)
			if opts.MeStatus == http.StatusOK {
				_, _ = io.WriteString(w, `{"id":42,"name":"Ada Lovelace","email":"ada@example.test"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))

	return server, recorder
}

func newWhoamiTestServer(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" {
			http.NotFound(w, r)

			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Fatal("/me request missing expected authorization header")
		}

		w.WriteHeader(statusCode)
		if statusCode == http.StatusOK {
			_, _ = io.WriteString(w, `{"id":42,"name":"Ada Lovelace","email":"ada@example.test"}`)
		}
	}))
}

type deviceServerOptions struct {
	TokenErrors []string
	ExpiresIn   int
	MeStatus    int
}

func newDeviceTestServer(t *testing.T, opts deviceServerOptions) *httptest.Server {
	t.Helper()

	if opts.ExpiresIn == 0 {
		opts.ExpiresIn = 60
	}
	if opts.MeStatus == 0 {
		opts.MeStatus = http.StatusOK
	}

	var pollCount atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cli/device":
			if r.Method != http.MethodPost {
				t.Fatalf("device create method = %s, want POST", r.Method)
			}

			var body struct {
				Label string `json:"label"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode device create body: %v", err)
			}
			if body.Label == "" {
				t.Fatal("device create body missing label")
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"device_code":%q,"user_code":"USER-CODE","verification_uri":"https://verify.example.test","verification_uri_complete":"https://verify.example.test/complete","interval":1,"expires_in":%d}`, testDeviceCode, opts.ExpiresIn)
		case "/api/v1/cli/device/token":
			if r.Method != http.MethodPost {
				t.Fatalf("device token method = %s, want POST", r.Method)
			}

			var body struct {
				DeviceCode string `json:"device_code"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode device token body: %v", err)
			}
			if body.DeviceCode != testDeviceCode {
				t.Fatal("device token body did not include expected device code")
			}

			idx := int(pollCount.Add(1)) - 1
			w.Header().Set("Content-Type", "application/json")
			if idx < len(opts.TokenErrors) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"error":%q}`, opts.TokenErrors[idx])

				return
			}

			_, _ = fmt.Fprintf(w, `{"token":%q}`, testToken)
		case "/api/v1/me":
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				t.Fatal("/me request missing expected authorization header")
			}
			w.WriteHeader(opts.MeStatus)
			if opts.MeStatus == http.StatusOK {
				_, _ = io.WriteString(w, `{"id":42,"name":"Ada Lovelace","email":"ada@example.test"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func recordSleeps(delays *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, delay time.Duration) error {
		if delays != nil {
			*delays = append(*delays, delay)
		}

		return nil
	}
}

func openerGET(t *testing.T) func(string) error {
	t.Helper()

	return func(authorizeURL string) error {
		resp, err := http.Get(authorizeURL)
		if err != nil {
			return err
		}

		return resp.Body.Close()
	}
}

func setTestConfigHome(t *testing.T) {
	t.Helper()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CAPSTAN_SERVER", "")
}

func assertNoCredentialsFile(t *testing.T) {
	t.Helper()

	path, err := config.Path()
	if err != nil {
		t.Fatalf("Path returned error: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials file exists or stat returned unexpected error: %v", err)
	}
}

func assertNoTokenLeak(t *testing.T, streams ...string) {
	t.Helper()

	for _, stream := range streams {
		if strings.Contains(stream, testToken) {
			t.Fatalf("stream leaked token: %q", stream)
		}
	}
}

func assertNoSecretLeak(t *testing.T, streams ...string) {
	t.Helper()

	for _, stream := range streams {
		if strings.Contains(stream, testToken) || strings.Contains(stream, testDeviceCode) {
			t.Fatalf("stream leaked token or device code: %q", stream)
		}
	}
}
