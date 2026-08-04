package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/artisan-build/capstan-cli/internal/config"
)

const testToken = "test-token-123"

func TestLoginSuccessfulFlowStoresCredentials(t *testing.T) {
	setTestConfigHome(t)

	server := newAuthTestServer(t, http.StatusOK)
	defer server.Close()

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "login", "--label", "test-host"}, openerGET(t))
	if err != nil {
		t.Fatalf("login returned error: %v", err)
	}
	assertNoTokenLeak(t, stdout, stderr)

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

	stdout, stderr, err := executeCLI(t, []string{"--server", "https://server.example.test", "login"}, func(authorizeURL string) error {
		parsed, err := url.Parse(authorizeURL)
		if err != nil {
			return err
		}

		redirectURL := parsed.Query().Get("redirect_uri") + "?token=" + url.QueryEscape(testToken) + "&state=wrong-state"
		resp, err := http.Get(redirectURL)
		if err != nil {
			return err
		}

		return resp.Body.Close()
	})
	if err == nil {
		t.Fatal("login succeeded with state mismatch")
	}
	assertNoTokenLeak(t, stdout, stderr)
	assertNoCredentialsFile(t)
}

func TestLoginAccessDeniedFailsWithoutCredentials(t *testing.T) {
	setTestConfigHome(t)

	stdout, stderr, err := executeCLI(t, []string{"--server", "https://server.example.test", "login"}, func(authorizeURL string) error {
		parsed, err := url.Parse(authorizeURL)
		if err != nil {
			return err
		}

		state := parsed.Query().Get("state")
		redirectURL := parsed.Query().Get("redirect_uri") + "?error=access_denied&state=" + url.QueryEscape(state)
		resp, err := http.Get(redirectURL)
		if err != nil {
			return err
		}

		return resp.Body.Close()
	})
	if err == nil {
		t.Fatal("login succeeded after access_denied callback")
	}
	if !strings.Contains(stderr, "access_denied") {
		t.Fatalf("stderr = %q, want access_denied message", stderr)
	}
	assertNoTokenLeak(t, stdout, stderr)
	assertNoCredentialsFile(t)
}

func TestLoginMeFailureFailsClosedWithoutCredentials(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("status %d", statusCode), func(t *testing.T) {
			setTestConfigHome(t)

			server := newAuthTestServer(t, statusCode)
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

func newAuthTestServer(t *testing.T, meStatus int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cli/authorize":
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

			callback := redirectURI + "?token=" + url.QueryEscape(testToken) + "&state=" + url.QueryEscape(state)
			http.Redirect(w, r, callback, http.StatusFound)
		case "/api/v1/me":
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				t.Fatal("/me request missing expected authorization header")
			}
			w.WriteHeader(meStatus)
			if meStatus == http.StatusOK {
				_, _ = io.WriteString(w, `{"id":42,"name":"Ada Lovelace","email":"ada@example.test"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))

	return server
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
