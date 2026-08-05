package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/artisan-build/capstan-cli/internal/config"
)

const testShareURL = "https://artifact.example.test/share/abc123"

func TestArtifactCreateHappyPathPrintsShareURL(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "artifact.html", "<html><body>Hello</body></html>")
	server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer server.Close()
	saveTestCreds(t, server.URL)

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath}, nil)
	if err != nil {
		t.Fatalf("artifact create returned error: %v", err)
	}
	if stdout != testShareURL+"\n" {
		t.Fatalf("stdout = %q, want share URL only", stdout)
	}
	assertNoTokenLeak(t, stdout, stderr)

	if got := server.requestCount.Load(); got != 1 {
		t.Fatalf("server received %d requests, want 1", got)
	}
	body := server.lastBody(t)
	if body.Content != "<html><body>Hello</body></html>" {
		t.Fatalf("content = %q, want exact file content", body.Content)
	}
	if body.ContentType != "text/html" {
		t.Fatalf("content_type = %q, want text/html", body.ContentType)
	}
	if body.Visibility != "" || body.ExpiresAt != "" {
		t.Fatalf("optional fields were not omitted: %#v", body)
	}
	var rawBody map[string]any
	if err := json.Unmarshal(server.lastRawBody(t), &rawBody); err != nil {
		t.Fatalf("decode raw request body: %v", err)
	}
	if _, ok := rawBody["visibility"]; ok {
		t.Fatalf("raw request body contained visibility key: %#v", rawBody)
	}
	if _, ok := rawBody["expires_at"]; ok {
		t.Fatalf("raw request body contained expires_at key: %#v", rawBody)
	}
	if got := server.lastAuthorization(t); got != "Bearer "+testToken {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
}

func TestArtifactCreateJSONOutput(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
	server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer server.Close()
	saveTestCreds(t, server.URL)

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath, "--json"}, nil)
	if err != nil {
		t.Fatalf("artifact create --json returned error: %v", err)
	}
	assertNoTokenLeak(t, stdout, stderr)

	var response struct {
		Artifact struct {
			ID int `json:"id"`
		} `json:"artifact"`
		ShareURL string `json:"share_url"`
	}
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("stdout was not valid JSON: %v", err)
	}
	if response.Artifact.ID != 123 || response.ShareURL != testShareURL {
		t.Fatalf("response = %#v, want artifact id and share URL", response)
	}
}

func TestArtifactCreateRejectsOversizedFileBeforeRequest(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "large.html", strings.Repeat("a", maxArtifactBytes+1))
	server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer server.Close()
	saveTestCreds(t, server.URL)

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath}, nil)
	if err == nil {
		t.Fatal("artifact create succeeded with oversized file")
	}
	if !strings.Contains(stderr, fmt.Sprint(maxArtifactBytes+1)) || !strings.Contains(stderr, fmt.Sprint(maxArtifactBytes)) {
		t.Fatalf("stderr = %q, want actual size and limit", stderr)
	}
	if got := server.requestCount.Load(); got != 0 {
		t.Fatalf("server received %d requests, want 0", got)
	}
	assertNoTokenLeak(t, stdout, stderr)
}

func TestArtifactCreateVisibilityMappingAndValidation(t *testing.T) {
	for _, tt := range []struct {
		flag string
		want string
	}{
		{flag: "org", want: "org_auth"},
		{flag: "signed", want: "signed_url"},
	} {
		t.Run(tt.flag, func(t *testing.T) {
			setTestConfigHome(t)
			filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
			server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
			defer server.Close()
			saveTestCreds(t, server.URL)

			_, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath, "--visibility", tt.flag}, nil)
			if err != nil {
				t.Fatalf("artifact create returned error: %v", err)
			}
			assertNoTokenLeak(t, stderr)
			if got := server.lastBody(t).Visibility; got != tt.want {
				t.Fatalf("visibility = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("invalid", func(t *testing.T) {
		setTestConfigHome(t)
		filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
		server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
		defer server.Close()
		saveTestCreds(t, server.URL)

		stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath, "--visibility", "public"}, nil)
		if err == nil {
			t.Fatal("artifact create succeeded with invalid visibility")
		}
		if got := server.requestCount.Load(); got != 0 {
			t.Fatalf("server received %d requests, want 0", got)
		}
		assertNoTokenLeak(t, stdout, stderr)
	})
}

func TestArtifactCreateExpiresDuration(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
	server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer server.Close()
	saveTestCreds(t, server.URL)

	before := time.Now().UTC()
	_, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath, "--expires", "7d"}, nil)
	if err != nil {
		t.Fatalf("artifact create returned error: %v", err)
	}
	assertNoTokenLeak(t, stderr)

	expiresAt, err := time.Parse(time.RFC3339, server.lastBody(t).ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at was not RFC3339: %v", err)
	}
	want := before.Add(168 * time.Hour)
	if expiresAt.Before(want.Add(-time.Minute)) || expiresAt.After(want.Add(time.Minute)) {
		t.Fatalf("expires_at = %s, want about 168h in future from %s", expiresAt, want)
	}
}

func TestArtifactCreateRejectsInvalidExpiresBeforeRequest(t *testing.T) {
	for _, expires := range []string{"garbage", "-1h", "0h", "0d"} {
		t.Run(expires, func(t *testing.T) {
			setTestConfigHome(t)
			filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
			server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
			defer server.Close()
			saveTestCreds(t, server.URL)

			stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath, "--expires", expires}, nil)
			if err == nil {
				t.Fatalf("artifact create succeeded with expires %q", expires)
			}
			if got := server.requestCount.Load(); got != 0 {
				t.Fatalf("server received %d requests, want 0", got)
			}
			assertNoTokenLeak(t, stdout, stderr)
		})
	}
}

func TestArtifactCreateServerErrors(t *testing.T) {
	for _, tt := range []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{name: "validation", statusCode: http.StatusUnprocessableEntity, body: `{"error":"invalid","errors":{"content":["content is required"]}}`, want: "content is required"},
		{name: "disabled", statusCode: http.StatusNotFound, body: `{}`, want: "artifacts are disabled"},
		{name: "unauthorized", statusCode: http.StatusUnauthorized, body: `{}`, want: "capstan login"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setTestConfigHome(t)
			filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
			server := newArtifactTestServer(t, artifactServerOptions{StatusCode: tt.statusCode, Body: tt.body})
			defer server.Close()
			saveTestCreds(t, server.URL)

			stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath}, nil)
			if err == nil {
				t.Fatalf("artifact create succeeded with status %d", tt.statusCode)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.want)
			}
			assertNoTokenLeak(t, stdout, stderr)
		})
	}
}

func TestArtifactCreateRequiresLoginBeforeRequest(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
	server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer server.Close()

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath}, nil)
	if err == nil {
		t.Fatal("artifact create succeeded without credentials")
	}
	if !strings.Contains(stderr, "not logged in") {
		t.Fatalf("stderr = %q, want not logged in", stderr)
	}
	if got := server.requestCount.Load(); got != 0 {
		t.Fatalf("server received %d requests, want 0", got)
	}
	assertNoTokenLeak(t, stdout, stderr)
}

func TestArtifactCreateWithoutConfiguredServerReturnsGuidance(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
	if err := config.Save(config.Credentials{Token: testToken, Server: ""}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	stdout, stderr, err := executeCLI(t, []string{"artifact", "create", "--file", filePath}, nil)
	if err == nil {
		t.Fatal("artifact create succeeded without configured server")
	}
	for _, want := range []string{"capstan login", "--server", "CAPSTAN_SERVER"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	}
	assertNoTokenLeak(t, stdout, stderr)
}

func TestArtifactCreateDifferentServerDoesNotSendToken(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
	serverA := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer serverA.Close()
	serverB := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer serverB.Close()
	saveTestCreds(t, serverA.URL)

	stdout, stderr, err := executeCLI(t, []string{"--server", serverB.URL, "artifact", "create", "--file", filePath}, nil)
	if err == nil {
		t.Fatal("artifact create succeeded for a different server")
	}
	if !strings.Contains(stderr, "not logged in to") {
		t.Fatalf("stderr = %q, want not logged in to", stderr)
	}
	if got := serverB.requestCount.Load(); got != 0 {
		t.Fatalf("server B received %d requests, want 0", got)
	}
	assertNoTokenLeak(t, stdout, stderr)
}

func TestArtifactCreateXHTMLInference(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "artifact.xhtml", "<html></html>")
	server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer server.Close()
	saveTestCreds(t, server.URL)

	_, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath}, nil)
	if err != nil {
		t.Fatalf("artifact create returned error: %v", err)
	}
	assertNoTokenLeak(t, stderr)
	if got := server.lastBody(t).ContentType; got != "application/xhtml+xml" {
		t.Fatalf("content_type = %q, want application/xhtml+xml", got)
	}
}

func TestArtifactCreateRejectsInvalidContentTypeBeforeRequest(t *testing.T) {
	setTestConfigHome(t)
	filePath := writeArtifactFile(t, "artifact.html", "<html></html>")
	server := newArtifactTestServer(t, artifactServerOptions{StatusCode: http.StatusCreated})
	defer server.Close()
	saveTestCreds(t, server.URL)

	stdout, stderr, err := executeCLI(t, []string{"--server", server.URL, "artifact", "create", "--file", filePath, "--content-type", "text/plain"}, nil)
	if err == nil {
		t.Fatal("artifact create succeeded with invalid content type")
	}
	if got := server.requestCount.Load(); got != 0 {
		t.Fatalf("server received %d requests, want 0", got)
	}
	assertNoTokenLeak(t, stdout, stderr)
}

type artifactServerOptions struct {
	StatusCode int
	Body       string
}

type artifactTestServer struct {
	*httptest.Server
	requestCount  atomic.Int64
	mu            sync.Mutex
	body          artifactRequestBody
	rawBody       []byte
	authorization string
}

type artifactRequestBody struct {
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
	Visibility  string `json:"visibility"`
	ExpiresAt   string `json:"expires_at"`
}

func newArtifactTestServer(t *testing.T, opts artifactServerOptions) *artifactTestServer {
	t.Helper()

	if opts.StatusCode == 0 {
		opts.StatusCode = http.StatusCreated
	}

	fake := &artifactTestServer{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/artifacts" {
			http.NotFound(w, r)

			return
		}

		fake.requestCount.Add(1)
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}

		var requestBody artifactRequestBody
		if err := json.Unmarshal(body, &requestBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		fake.mu.Lock()
		fake.body = requestBody
		fake.rawBody = append(fake.rawBody[:0], body...)
		fake.authorization = r.Header.Get("Authorization")
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(opts.StatusCode)
		if opts.Body != "" {
			_, _ = io.WriteString(w, opts.Body)

			return
		}

		_, _ = fmt.Fprintf(w, `{"artifact":{"id":123,"author_id":42,"visibility":"org_auth","expires_at":"","content_type":"text/html","size_bytes":10,"content_hash":"abc","share_url":%q,"created_at":"2026-08-04T00:00:00Z"},"share_url":%q}`, testShareURL, testShareURL)
	}))

	return fake
}

func (s *artifactTestServer) lastBody(t *testing.T) artifactRequestBody {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.body
}

func (s *artifactTestServer) lastAuthorization(t *testing.T) string {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.authorization
}

func (s *artifactTestServer) lastRawBody(t *testing.T) []byte {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]byte(nil), s.rawBody...)
}

func writeArtifactFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write artifact file: %v", err)
	}

	return path
}

func saveTestCreds(t *testing.T, server string) {
	t.Helper()

	if err := config.Save(config.Credentials{Token: testToken, Server: server}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
}
