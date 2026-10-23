package work

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSoloSelfTesterObservesEveryRungAndFailsClosed(t *testing.T) {
	config := testSoloConfig(t)
	config.SoloMCPPath = "/fake/solo-mcp"
	config.SoloCallTimeout = time.Second

	t.Run("all rungs", func(t *testing.T) {
		calls := 0
		tester := SoloSelfTester{Timeout: time.Second, Tick: func(ctx context.Context, path string) error {
			calls++
			if calls > 1 {
				return nil
			}
			loaded, err := LoadConfig(path)
			if err != nil {
				return err
			}
			claim := selfTestHTTP(t, ctx, loaded, http.MethodPost, "/api/v1/work/self-test/claim", `{}`)
			var response ClaimResponse
			if err := json.Unmarshal(claim, &response); err != nil {
				return err
			}
			transition := TransitionRequest{ItemID: response.Items[0].ID, Fence: 1, State: "delivered", LaunchNonce: response.Items[0].LaunchNonce, SessionRef: "solo:self-test"}
			payload, _ := json.Marshal(transition)
			selfTestHTTP(t, ctx, loaded, http.MethodPost, "/api/v1/work/self-test/transition", string(payload))
			briefURL := response.Items[0].Links["brief"]
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, briefURL, nil)
			result, err := http.DefaultClient.Do(request)
			if err != nil {
				return err
			}
			_ = result.Body.Close()
			milestoneURL := response.Items[0].Links["milestone"]
			for _, body := range []string{`{"kind":"progress"}`, `{"kind":"done"}`} {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, milestoneURL, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				res, requestErr := http.DefaultClient.Do(req)
				if requestErr != nil {
					return requestErr
				}
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
			return nil
		}}
		var output bytes.Buffer
		evidence, err := tester.Run(context.Background(), config, &output)
		if err != nil {
			t.Fatal(err)
		}
		if err := evidence.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, rung := range []string{"claim", "spawn", "brief fetch", "milestone", "done"} {
			if !strings.Contains(output.String(), rung) {
				t.Fatalf("output missing %q: %s", rung, output.String())
			}
		}
	})

	t.Run("missing fetch milestone and done", func(t *testing.T) {
		tester := SoloSelfTester{Timeout: 200 * time.Millisecond, Tick: func(ctx context.Context, path string) error {
			loaded, err := LoadConfig(path)
			if err != nil {
				return err
			}
			claim := selfTestHTTP(t, ctx, loaded, http.MethodPost, "/api/v1/work/self-test/claim", `{}`)
			var response ClaimResponse
			_ = json.Unmarshal(claim, &response)
			if len(response.Items) > 0 {
				transition := TransitionRequest{ItemID: response.Items[0].ID, Fence: 1, State: "delivered", LaunchNonce: response.Items[0].LaunchNonce, SessionRef: "solo:self-test"}
				payload, _ := json.Marshal(transition)
				selfTestHTTP(t, ctx, loaded, http.MethodPost, "/api/v1/work/self-test/transition", string(payload))
			}
			return nil
		}}
		evidence, err := tester.Run(context.Background(), config, io.Discard)
		if err == nil || evidence.Fetch || evidence.Milestone || evidence.Done {
			t.Fatalf("evidence, error = %#v, %v", evidence, err)
		}
	})
}

func selfTestHTTP(t *testing.T, ctx context.Context, config Config, method, path, body string) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, config.CapstanURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer setup-self-test")
	request.Header.Set("X-Capstan-Actor-ID", "1")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, data)
	}
	return data
}
