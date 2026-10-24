package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

func TestSelfTestConfigPreservesExplicitEmptySoloExtraArgs(t *testing.T) {
	config := testSoloConfig(t)
	config.SoloCallTimeout = time.Second
	config.SoloBindings = cloneSoloBindings(config.SoloBindings)
	path, cleanup, err := writeSelfTestConfig(config, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(self-test config) error = %v", err)
	}
	if loaded.SoloBindings["orchestrator"].ExtraArgs == nil {
		t.Fatal("self-test config changed explicit empty extra_args into null")
	}
}

func TestSoloSelfTesterClosesExactSpawnedSessionBeforeTemporaryStateRemoval(t *testing.T) {
	for _, mode := range []string{"error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			config := testSoloConfig(t)
			config.SoloMCPPath = "/fake/solo-mcp"
			config.SoloCallTimeout = time.Second
			var configPath, wantRef string
			terminated := false
			timeout := time.Second
			if mode == "timeout" {
				timeout = 200 * time.Millisecond
			}
			tester := SoloSelfTester{
				Timeout: timeout,
				Tick: func(ctx context.Context, path string) error {
					configPath = path
					loaded, err := LoadConfig(path)
					if err != nil {
						return err
					}
					claim := selfTestHTTP(t, ctx, loaded, http.MethodPost, "/api/v1/work/self-test/claim", `{}`)
					var response ClaimResponse
					if err := json.Unmarshal(claim, &response); err != nil {
						return err
					}
					nonce := response.Items[0].LaunchNonce
					wantRef, err = (SoloSessionRef{SpokeID: 1, SoloProjectID: 12, SoloProcessID: 77, LaunchNonce: nonce, Harness: "claude", AgentToolID: 13}).String()
					if err != nil {
						return err
					}
					workDir := filepath.Join(loaded.WorkDir, testSelfItemID+"-f1")
					if err := os.MkdirAll(workDir, 0o700); err != nil {
						return err
					}
					state := State{Version: stateVersion, Items: map[string]*ItemState{testSelfItemID: {TaskID: testSelfTaskID, Role: "orchestrator", Fence: 1, Phase: "launched", WorkDir: workDir, LaunchNonce: nonce, SessionRef: wantRef, SpokeID: 1}}}
					if err := NewStateStore(loaded.StateFile, loaded.WorkDir).Save(state); err != nil {
						return err
					}
					if mode == "timeout" {
						<-ctx.Done()
						return ctx.Err()
					}
					return errors.New("failure after spawn")
				},
				Terminate: func(_ context.Context, loaded Config, ref string) error {
					if _, err := os.Stat(loaded.StateFile); err != nil {
						t.Fatalf("temporary state disappeared before termination: %v", err)
					}
					if ref != wantRef {
						t.Fatalf("terminated ref = %q, want %q", ref, wantRef)
					}
					terminated = true
					return nil
				},
			}
			if _, err := tester.Run(context.Background(), config, io.Discard); err == nil {
				t.Fatal("post-spawn failure passed")
			}
			if !terminated {
				t.Fatal("spawned self-test session was not terminated")
			}
			if _, err := os.Stat(filepath.Dir(configPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary state remains after termination: %v", err)
			}
		})
	}
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
