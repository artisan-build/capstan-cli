package work

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func observedOpenCodeFrame(suggestion string) string {
	return "    ┃\n    ┃  Ask anything… \"" + suggestion + "\"\n    ┃\n    ┃  Build · Fixture Model Provider\n    ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n    <worktree> shift+tab agents  ctrl+p commands\n\n  ⊙ 1 MCP failed /mcps    2.0.26\n"
}

func TestSoloOpenCodeOldAndObservedCompleteFramesRejectPartialFrames(t *testing.T) {
	if !readyForHarness("opencode", "┃ Ask anything, / for commands, @ for context…") {
		t.Fatal("old marker lost")
	}
	for _, suggestion := range []string{"Fix a TODO in the codebase", "Fix broken tests", "A rotating suggestion"} {
		frame := observedOpenCodeFrame(suggestion)
		if !readyForHarness("opencode", frame) {
			t.Fatal("observed complete frame not accepted")
		}
		for _, partial := range []string{
			"┃ Ask anything…", "┃ Ask anything… \"" + suggestion + "\"",
			strings.Replace(frame, "╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀", "", 1),
			strings.Replace(frame, "Build · Fixture Model Provider", "", 1),
			strings.Replace(frame, "Ask anything…", "Ask", 1),
		} {
			if readyForHarness("opencode", partial) {
				t.Fatalf("partial frame accepted: %q", partial)
			}
		}
	}
}

func TestSoloOpenCodeNewFrameStillRequiresIdleAndExactIdentity(t *testing.T) {
	for _, mode := range []string{"busy", "reused"} {
		t.Run(mode, func(t *testing.T) {
			ref := SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "opencode", AgentToolID: 9}
			status := soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}
			if mode == "reused" {
				status.Name = "unrelated-agent"
				status.AgentState.Idle = true
			}
			f := &fakeSoloCall{status: status, outputs: []string{observedOpenCodeFrame("Fix broken tests")}}
			l := NewSoloLauncher(f, 5*time.Millisecond)
			l.pollInterval = time.Millisecond
			if err := l.waitReady(context.Background(), ref, false); err == nil {
				t.Fatal("frame bypassed idle/identity check")
			}
		})
	}
}

func TestSoloDeadlineNudgeExactIdentityTextAndSubmit(t *testing.T) {
	ref := SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 9}
	value, err := ref.String()
	if err != nil {
		t.Fatal(err)
	}
	for _, reused := range []bool{false, true} {
		f := &fakeSoloCall{status: soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}}
		if reused {
			f.status.Name = "capstan-builder-" + testRespawnNonce
		}
		l := NewSoloLauncher(f, time.Second)
		err := l.Continue(context.Background(), value, "Post work progress or declare the work finished.")
		var sends []map[string]any
		for _, call := range f.calls {
			if call.name == "send_input" {
				sends = append(sends, call.arguments)
			}
		}
		if reused {
			if err == nil || len(sends) != 0 {
				t.Fatal("nudge reached reused process")
			}
			continue
		}
		if err != nil || len(sends) != 2 || sends[0]["input"] != "Post work progress or declare the work finished." || sends[0]["submit"] != false || !reflect.DeepEqual(sends[1]["bytes"], []int{13}) {
			t.Fatalf("nudge sends: %#v %v", sends, err)
		}
		for _, send := range sends {
			if send["process_id"] != int64(77) || send["project_id"] != int64(8) {
				t.Fatal("nudge changed exact process scope")
			}
		}
	}
}

func TestSoloLongSteerWritesBoundedUTF8ChunksWithoutClipping(t *testing.T) {
	ref, err := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 9}).String()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSoloCall{status: soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}}
	text := strings.Repeat("correction 界 ", 80)
	if err := NewSoloLauncher(f, time.Second).Continue(context.Background(), ref, text); err != nil {
		t.Fatal(err)
	}
	var delivered strings.Builder
	enters := 0
	for _, call := range f.calls {
		if call.name != "send_input" {
			continue
		}
		if chunk, ok := call.arguments["input"].(string); ok {
			if len(chunk) > maxPointerBytes {
				t.Fatal("PTY write exceeded proven boundary")
			}
			delivered.WriteString(chunk)
		}
		if reflect.DeepEqual(call.arguments["bytes"], []int{13}) {
			enters++
		}
	}
	if delivered.String() != text || enters != 1 {
		t.Fatal("long steer clipped or submitted early")
	}
}

func TestSoloSelfTestSuccessCapturesSessionBeforeLostForgetTick(t *testing.T) {
	c := testSoloConfig(t)
	c.SoloCallTimeout = time.Second
	calls := 0
	var want, configPath string
	closed := 0
	tester := SoloSelfTester{Timeout: time.Second,
		Tick: func(ctx context.Context, path string) error {
			calls++
			configPath = path
			config, err := LoadConfig(path)
			if err != nil {
				return err
			}
			if calls > 1 {
				// The real runner forgets this orchestrator holding on its final lost renewal.
				return NewStateStore(config.StateFile, config.WorkDir).Save(State{Version: stateVersion, Items: map[string]*ItemState{}})
			}
			var response ClaimResponse
			if err := json.Unmarshal(selfTestHTTP(t, ctx, config, http.MethodPost, "/api/v1/work/self-test/claim", `{}`), &response); err != nil {
				return err
			}
			claim := response.Items[0]
			nonce := claim.LaunchNonce
			want, err = (SoloSessionRef{SpokeID: 1, SoloProjectID: 12, SoloProcessID: 77, LaunchNonce: nonce, Harness: "claude", AgentToolID: 13}).String()
			if err != nil {
				return err
			}
			item := &ItemState{TaskID: testSelfTaskID, Role: "orchestrator", Fence: 1, Phase: "routed", WorkDir: filepath.Join(config.WorkDir, testSelfItemID+"-f1"), LaunchNonce: nonce, SessionRef: want, SpokeID: 1}
			if err := os.MkdirAll(item.WorkDir, 0o700); err != nil {
				return err
			}
			if err := NewStateStore(config.StateFile, config.WorkDir).Save(State{Version: stateVersion, Items: map[string]*ItemState{testSelfItemID: item}}); err != nil {
				return err
			}
			payload, _ := json.Marshal(TransitionRequest{ItemID: testSelfItemID, Fence: 1, State: "delivered", LaunchNonce: nonce, SessionRef: want})
			selfTestHTTP(t, ctx, config, http.MethodPost, "/api/v1/work/self-test/transition", string(payload))
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, claim.Links["brief"], nil)
			result, err := http.DefaultClient.Do(request)
			if err != nil {
				return err
			}
			_ = result.Body.Close()
			for _, kind := range []string{"progress", "done"} {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, claim.Links["milestone"], strings.NewReader(`{"kind":"`+kind+`"}`))
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				_ = res.Body.Close()
			}
			return nil
		},
		Terminate: func(_ context.Context, config Config, ref string) error {
			closed++
			if ref != want || len(loadWorkState(t, config).Items) != 0 {
				t.Fatal("cleanup did not retain exact pre-forget session")
			}
			return nil
		},
	}
	if evidence, err := tester.Run(context.Background(), c, io.Discard); err != nil || !evidence.Done || closed != 1 || calls != 2 {
		t.Fatalf("self-test: %#v %v closed=%d calls=%d", evidence, err, closed, calls)
	}
	if _, err := os.Stat(filepath.Dir(configPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("self-test temp directory retained")
	}
}

func TestSoloSelfTestCleanupDoesNotCloseUnrelatedOrReusedProcess(t *testing.T) {
	for _, mode := range []string{"unrelated_nonce", "reused_process", "unrelated_item"} {
		t.Run(mode, func(t *testing.T) {
			c := testSoloConfig(t)
			c.SoloCallTimeout = time.Second
			f := &fakeSoloCall{}
			l := NewSoloLauncher(f, time.Second)
			tester := SoloSelfTester{Timeout: time.Second, Tick: func(ctx context.Context, path string) error {
				config, err := LoadConfig(path)
				if err != nil {
					return err
				}
				var response ClaimResponse
				if err := json.Unmarshal(selfTestHTTP(t, ctx, config, http.MethodPost, "/api/v1/work/self-test/claim", `{}`), &response); err != nil {
					return err
				}
				nonce := response.Items[0].LaunchNonce
				refNonce := nonce
				if mode == "unrelated_nonce" {
					refNonce = testNonce
				}
				ref, err := (SoloSessionRef{SpokeID: 1, SoloProjectID: 12, SoloProcessID: 77, LaunchNonce: refNonce, Harness: "claude", AgentToolID: 13}).String()
				if err != nil {
					return err
				}
				id := testSelfItemID
				if mode == "unrelated_item" {
					id = testItemID
				}
				item := &ItemState{TaskID: testSelfTaskID, Role: "orchestrator", Fence: 1, Phase: "routed", WorkDir: filepath.Join(config.WorkDir, id+"-f1"), LaunchNonce: nonce, SessionRef: ref, SpokeID: 1}
				if err := NewStateStore(config.StateFile, config.WorkDir).Save(State{Version: stateVersion, Items: map[string]*ItemState{id: item}}); err != nil {
					return err
				}
				f.status = soloProcessStatus{ID: 77, Kind: "agent", Status: "Running", Name: "unrelated-reused-process"}
				return errors.New("after spawn failure")
			}, Terminate: func(ctx context.Context, _ Config, ref string) error { return l.Terminate(ctx, ref) }}
			if _, err := tester.Run(context.Background(), c, io.Discard); err == nil {
				t.Fatal("fixture unexpectedly succeeded")
			}
			for _, call := range f.calls {
				if call.name == "close_process" {
					t.Fatal("cleanup closed unrelated/reused process")
				}
			}
		})
	}
}
