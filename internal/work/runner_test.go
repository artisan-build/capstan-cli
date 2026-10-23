package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testItemID       = "018f0000-0000-7000-8000-000000000001"
	testItemID2      = "018f0000-0000-7000-8000-000000000002"
	testTaskID       = "018f0000-0000-7000-8000-000000000011"
	testNonce        = "018f0000-0000-7000-8000-000000000021"
	testRespawnNonce = "018f0000-0000-7000-8000-000000000022"
	testDirectiveID  = "018f0000-0000-7000-8000-000000000031"
	testDirectiveID2 = "018f0000-0000-7000-8000-000000000032"
)

type staticHealth struct {
	snapshot HealthSnapshot
	err      error
}

func (h staticHealth) Measure(context.Context) (HealthSnapshot, error) { return h.snapshot, h.err }

type fakeLauncher struct {
	mu           sync.Mutex
	launches     []LaunchSpec
	alive        map[string]bool
	aliveErr     error
	launchErr    error
	gateErr      error
	gateErrAt    int
	terminated   []string
	gates        []string
	enters       []string
	pointers     []string
	sessionIndex int
}

func (f *fakeLauncher) Launch(_ context.Context, spec LaunchSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, spec)
	if f.launchErr != nil {
		return "", f.launchErr
	}
	f.sessionIndex++
	ref := fmt.Sprintf("fake:session:%d", f.sessionIndex)
	if f.alive == nil {
		f.alive = make(map[string]bool)
	}
	f.alive[ref] = true

	return ref, nil
}

func (f *fakeLauncher) Alive(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aliveErr != nil {
		return false, f.aliveErr
	}
	value, ok := f.alive[ref]
	if !ok {
		return true, nil
	}

	return value, nil
}

func (f *fakeLauncher) Terminate(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, ref)
	if f.alive != nil {
		f.alive[ref] = false
	}

	return nil
}

func (f *fakeLauncher) OpenGate(_ context.Context, _ string, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates = append(f.gates, path)
	if f.gateErr != nil && len(f.gates) == f.gateErrAt {
		return f.gateErr
	}

	return nil
}

func (f *fakeLauncher) SendEnter(_ context.Context, ref, _ string, directiveID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enters = append(f.enters, ref+":"+directiveID)

	return nil
}

func (f *fakeLauncher) ResendPointer(_ context.Context, ref, path, directiveID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pointers = append(f.pointers, ref+":"+path+":"+directiveID)

	return nil
}

type fakeAPI struct {
	claims       []ClaimRequest
	renews       [][]RenewRequestItem
	transitions  []TransitionRequest
	claimFn      func(ClaimRequest) (ClaimResponse, error)
	renewFn      func([]RenewRequestItem) ([]RenewResult, error)
	transitionFn func(TransitionRequest) error
}

func (f *fakeAPI) Claim(_ context.Context, request ClaimRequest) (ClaimResponse, error) {
	f.claims = append(f.claims, request)
	if f.claimFn != nil {
		return f.claimFn(request)
	}

	return ClaimResponse{}, nil
}

func (f *fakeAPI) Renew(_ context.Context, request []RenewRequestItem) ([]RenewResult, error) {
	f.renews = append(f.renews, append([]RenewRequestItem(nil), request...))
	if f.renewFn != nil {
		return f.renewFn(request)
	}

	return nil, nil
}

func (f *fakeAPI) Transition(_ context.Context, request TransitionRequest) error {
	f.transitions = append(f.transitions, request)
	if f.transitionFn != nil {
		return f.transitionFn(request)
	}

	return nil
}

func TestRunnerSendsExactTickAndLaunchTransitions(t *testing.T) {
	var claimBody ClaimRequest
	var transitions []TransitionRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer bound-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("X-Capstan-Actor-ID"); got != "42" {
			t.Errorf("X-Capstan-Actor-ID = %q", got)
		}
		if got := request.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		switch filepath.Base(request.URL.Path) {
		case "claim":
			if err := json.NewDecoder(request.Body).Decode(&claimBody); err != nil {
				t.Fatalf("decode claim: %v", err)
			}
			writeJSON(t, writer, ClaimResponse{Items: []ClaimItem{{
				ID: testItemID, TaskID: testTaskID, Role: "builder", Fence: 3, LeaseUntil: "2026-10-23T10:00:00Z",
				Links: testLinks(serverURL(request)), LaunchNonce: testNonce,
			}}, Directives: []Directive{}})
		case "transition":
			var transition TransitionRequest
			if err := json.NewDecoder(request.Body).Decode(&transition); err != nil {
				t.Fatalf("decode transition: %v", err)
			}
			transitions = append(transitions, transition)
			writeJSON(t, writer, map[string]any{"item": map[string]any{
				"id": testItemID, "task_id": testTaskID, "role": "builder", "state": transition.State, "fence": 3,
			}})
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()

	config := testConfig(t)
	config.CapstanURL = server.URL
	launcher := &fakeLauncher{}
	runner := NewRunner(config, NewClient(config, "bound-secret"), launcher, staticHealth{snapshot: HealthSnapshot{Load1m: 1, MemoryPercent: 20}}, nil)
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantClaim := ClaimRequest{
		Capacity: 2, OrchestratorCapacity: 1, Roles: []string{"builder", "orchestrator"}, Capabilities: []string{"git"},
		RoleProviders: map[string]string{"builder": "opencode", "orchestrator": "claude"}, CurrentClaimedItemCount: 0,
		HeldItems: []HeldItem{},
	}
	if !reflect.DeepEqual(claimBody, wantClaim) {
		t.Fatalf("claim body = %#v, want %#v", claimBody, wantClaim)
	}
	if len(transitions) != 2 {
		t.Fatalf("transitions = %#v", transitions)
	}
	if got := transitions[0]; got.State != "delivered" || got.LaunchNonce != testNonce || got.SessionRef != "fake:session:1" {
		t.Fatalf("delivered transition = %#v", got)
	}
	if got := transitions[1]; got.State != "routed" || got.LaunchNonce != "" || got.SessionRef != "" {
		t.Fatalf("routed transition = %#v", got)
	}
	stateData, err := os.ReadFile(config.StateFile)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	for _, forbidden := range []string{"bound-secret", "signature=", "brief?"} {
		if strings.Contains(string(stateData), forbidden) {
			t.Fatalf("global state contains %q: %s", forbidden, stateData)
		}
	}
	assertMode(t, config.StateFile, 0o600)
	linksPath := filepath.Join(config.WorkDir, testItemID+"-f3", linksFilename)
	assertMode(t, linksPath, 0o600)
	assertMode(t, filepath.Join(config.WorkDir, testItemID+"-f3", promptFilename), 0o600)
	assertMode(t, filepath.Join(config.WorkDir, testItemID+"-f3"), 0o700)
}

func TestRunnerRecoveryRenewsReplacesLinksAndFinishesPartialTransitions(t *testing.T) {
	config := testConfig(t)
	item := seedItem(t, config, testItemID, "builder", "launched", "fake:old")
	oldLinks := testLinks("https://old.example")
	if err := writeLinksFile(item, oldLinks); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		renewFn: func(items []RenewRequestItem) ([]RenewResult, error) {
			return []RenewResult{{ItemID: testItemID, Status: "renewed", Item: &RenewedItem{
				ID: testItemID, TaskID: testTaskID, Role: "builder", State: "claimed", Fence: 1, Links: testLinks("https://new.example"),
			}}}, nil
		},
		claimFn: func(ClaimRequest) (ClaimResponse, error) { return ClaimResponse{}, nil },
	}
	launcher := &fakeLauncher{alive: map[string]bool{"fake:old": true}}
	if err := NewRunner(config, api, launcher, staticHealth{snapshot: HealthSnapshot{}}, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(api.renews) != 1 || len(api.renews[0]) != 1 {
		t.Fatalf("renew batches = %#v", api.renews)
	}
	if got := []string{api.transitions[0].State, api.transitions[1].State}; !reflect.DeepEqual(got, []string{"delivered", "routed"}) {
		t.Fatalf("transition states = %#v", got)
	}
	data, err := os.ReadFile(filepath.Join(item.WorkDir, linksFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new.example") || strings.Contains(string(data), "old.example") {
		t.Fatalf("renewed links were not replaced: %s", data)
	}
}

func TestRunnerLostDeadAndLaunchFailureRecovery(t *testing.T) {
	t.Run("lost live session is terminated without release", func(t *testing.T) {
		config := testConfig(t)
		seedItem(t, config, testItemID, "builder", "routed", "fake:live")
		api := &fakeAPI{
			renewFn: func([]RenewRequestItem) ([]RenewResult, error) {
				return []RenewResult{{ItemID: testItemID, Status: "lost"}}, nil
			},
			claimFn: func(ClaimRequest) (ClaimResponse, error) { return ClaimResponse{}, nil },
		}
		launcher := &fakeLauncher{alive: map[string]bool{"fake:live": true}}
		if err := NewRunner(config, api, launcher, staticHealth{}, nil).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(launcher.terminated, []string{"fake:live"}) || len(api.transitions) != 0 {
			t.Fatalf("terminated=%#v transitions=%#v", launcher.terminated, api.transitions)
		}
	})

	t.Run("renewed dead session is released", func(t *testing.T) {
		config := testConfig(t)
		seedItem(t, config, testItemID, "builder", "routed", "fake:dead")
		api := renewedAPI("builder", "routed")
		launcher := &fakeLauncher{alive: map[string]bool{"fake:dead": false}}
		if err := NewRunner(config, api, launcher, staticHealth{}, nil).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.transitions) != 1 || api.transitions[0].State != "released" {
			t.Fatalf("transitions = %#v", api.transitions)
		}
	})

	t.Run("release conflict forgets the stale local record", func(t *testing.T) {
		config := testConfig(t)
		seedItem(t, config, testItemID, "builder", "routed", "fake:dead")
		api := renewedAPI("builder", "routed")
		api.transitionFn = func(request TransitionRequest) error {
			if request.State == "released" {
				return APIError{Status: http.StatusConflict, Code: "claim_lost"}
			}
			return nil
		}
		launcher := &fakeLauncher{alive: map[string]bool{"fake:dead": false}}
		if err := NewRunner(config, api, launcher, staticHealth{}, nil).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		state, err := NewStateStore(config.StateFile, config.WorkDir).Load()
		if err != nil || len(state.Items) != 0 {
			t.Fatalf("state after release conflict = %#v, %v", state, err)
		}
	})

	t.Run("failed launch releases claim", func(t *testing.T) {
		config := testConfig(t)
		api := &fakeAPI{claimFn: func(ClaimRequest) (ClaimResponse, error) {
			return ClaimResponse{Items: []ClaimItem{{
				ID: testItemID, TaskID: testTaskID, Role: "builder", Fence: 1, LaunchNonce: testNonce, Links: testLinks("https://server.example"),
			}}}, nil
		}}
		launcher := &fakeLauncher{launchErr: errors.New("test launch failure")}
		if err := NewRunner(config, api, launcher, staticHealth{}, nil).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.transitions) != 1 || api.transitions[0].State != "released" {
			t.Fatalf("transitions = %#v", api.transitions)
		}
		state, err := NewStateStore(config.StateFile, config.WorkDir).Load()
		if err != nil || len(state.Items) != 0 {
			t.Fatalf("state after failed launch = %#v, %v", state, err)
		}
	})
}

func TestRunnerDirectiveAuthorityReplayAndFollowingTickOutcome(t *testing.T) {
	t.Run("no directive makes no repair call", func(t *testing.T) {
		config := testConfig(t)
		seedItem(t, config, testItemID, "builder", "routed", "fake:held")
		api := renewedAPI("builder", "routed")
		launcher := &fakeLauncher{alive: map[string]bool{"fake:held": true}}
		if err := NewRunner(config, api, launcher, staticHealth{}, nil).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(launcher.enters)+len(launcher.pointers)+len(launcher.launches) != 0 {
			t.Fatalf("repairs executed without directive: %#v %#v %#v", launcher.enters, launcher.pointers, launcher.launches)
		}
	})

	for _, serverPhase := range []string{"delivered", "routed"} {
		t.Run("interrupted respawn waits for server replay from "+serverPhase, func(t *testing.T) {
			config := testConfig(t)
			item := seedItem(t, config, testItemID, "builder", "routed", "fake:old")
			item.Phase = "claimed"
			item.SessionRef = ""
			item.PendingLaunchKey = "directive:" + testDirectiveID
			item.Directive = &DirectiveState{ID: testDirectiveID, Kind: "respawn", Status: "pending"}
			if err := NewStateStore(config.StateFile, config.WorkDir).Save(State{Version: stateVersion, Items: map[string]*ItemState{testItemID: item}}); err != nil {
				t.Fatal(err)
			}
			api := renewedAPI("builder", serverPhase)
			api.claimFn = func(request ClaimRequest) (ClaimResponse, error) {
				if len(request.HeldItems) != 1 || request.HeldItems[0].ItemID != testItemID {
					t.Fatalf("pending respawn held items = %#v", request.HeldItems)
				}
				return ClaimResponse{}, nil
			}
			launcher := &fakeLauncher{}
			runner := NewRunner(config, api, launcher, staticHealth{}, nil)
			for tick := 1; tick <= 2; tick++ {
				if err := runner.Run(context.Background()); err != nil {
					t.Fatalf("tick %d error = %v", tick, err)
				}
			}
			if len(api.claims) != 2 {
				t.Fatalf("claim calls = %d", len(api.claims))
			}
			if len(launcher.launches)+len(launcher.enters)+len(launcher.pointers) != 0 {
				t.Fatalf("interrupted respawn retried without directive: %#v", launcher.launches)
			}
			state, err := NewStateStore(config.StateFile, config.WorkDir).Load()
			if err != nil {
				t.Fatal(err)
			}
			got := state.Items[testItemID]
			if got == nil || got.Phase != "claimed" || got.SessionRef != "" || got.PendingLaunchKey != "directive:"+testDirectiveID {
				t.Fatalf("pending respawn state = %#v", got)
			}
		})
	}

	for _, serverPhase := range []string{"delivered", "routed"} {
		for _, failure := range []string{"launch", "gate"} {
			t.Run("failed respawn stays held after "+failure+" failure from "+serverPhase, func(t *testing.T) {
				config := testConfig(t)
				seedItem(t, config, testItemID, "builder", "routed", "fake:old")
				api := renewedAPI("builder", serverPhase)
				claimCount := 0
				api.claimFn = func(request ClaimRequest) (ClaimResponse, error) {
					claimCount++
					switch claimCount {
					case 1:
						return ClaimResponse{Directives: []Directive{respawnDirective(testDirectiveID, testRespawnNonce)}}, nil
					case 2:
						if len(request.HeldItems) != 1 || request.HeldItems[0].DirectiveID != testDirectiveID || request.HeldItems[0].DirectiveOutcome != "failed" {
							t.Fatalf("failed respawn outcome = %#v", request.HeldItems)
						}
						return ClaimResponse{}, nil
					case 3:
						if len(request.HeldItems) != 1 || request.HeldItems[0].ItemID != testItemID || request.HeldItems[0].DirectiveID != "" {
							t.Fatalf("silent tick held items = %#v", request.HeldItems)
						}
						return ClaimResponse{}, nil
					case 4:
						if len(request.HeldItems) != 1 || request.HeldItems[0].ItemID != testItemID || request.HeldItems[0].DirectiveID != "" {
							t.Fatalf("second silent tick held items = %#v", request.HeldItems)
						}
						return ClaimResponse{}, nil
					case 5:
						if len(request.HeldItems) != 1 || request.HeldItems[0].ItemID != testItemID {
							t.Fatalf("replay tick held items = %#v", request.HeldItems)
						}
						return ClaimResponse{Directives: []Directive{respawnDirective(testDirectiveID2, "018f0000-0000-7000-8000-000000000023")}}, nil
					default:
						t.Fatalf("unexpected claim call %d", claimCount)
						return ClaimResponse{}, nil
					}
				}
				launcher := &fakeLauncher{alive: map[string]bool{"fake:old": true}}
				if failure == "launch" {
					launcher.launchErr = errors.New("test respawn launch failure")
				} else {
					launcher.gateErr = errors.New("test respawn gate failure")
					launcher.gateErrAt = 2
				}
				runner := NewRunner(config, api, launcher, staticHealth{}, nil)
				if err := runner.Run(context.Background()); err != nil {
					t.Fatalf("failure tick error = %v", err)
				}
				for tick := 1; tick <= 3; tick++ {
					if err := runner.Run(context.Background()); err != nil {
						t.Fatalf("following tick %d error = %v", tick, err)
					}
				}
				if len(api.claims) != 4 || len(api.transitions) != 0 {
					t.Fatalf("claims=%d transitions=%#v", len(api.claims), api.transitions)
				}
				if len(launcher.launches) != 1 {
					t.Fatalf("launches before replay = %#v", launcher.launches)
				}
				state, err := NewStateStore(config.StateFile, config.WorkDir).Load()
				if err != nil || state.Items[testItemID] == nil {
					t.Fatalf("held state after silent ticks = %#v, %v", state, err)
				}

				launcher.launchErr = nil
				if err := runner.Run(context.Background()); err != nil {
					t.Fatalf("server replay tick error = %v", err)
				}
				if len(launcher.launches) != 2 || launcher.launches[1].LaunchKey != "directive:"+testDirectiveID2 {
					t.Fatalf("launches after replay = %#v", launcher.launches)
				}
			})
		}
	}

	t.Run("send_enter executes once and reports on next tick", func(t *testing.T) {
		config := testConfig(t)
		seedItem(t, config, testItemID, "builder", "routed", "fake:held")
		calls := 0
		directive := Directive{ID: testDirectiveID, ItemID: testItemID, Fence: 1, Kind: "send_enter", Payload: DirectivePayload{}}
		api := renewedAPI("builder", "routed")
		api.claimFn = func(request ClaimRequest) (ClaimResponse, error) {
			calls++
			if calls == 2 {
				if len(request.HeldItems) != 1 || request.HeldItems[0].DirectiveID != testDirectiveID || request.HeldItems[0].DirectiveOutcome != "succeeded" {
					t.Fatalf("following tick held item = %#v", request.HeldItems)
				}
			}
			return ClaimResponse{Directives: []Directive{directive}}, nil
		}
		launcher := &fakeLauncher{alive: map[string]bool{"fake:held": true}}
		runner := NewRunner(config, api, launcher, staticHealth{}, nil)
		if err := runner.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := runner.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(launcher.enters) != 1 {
			t.Fatalf("send_enter calls = %#v", launcher.enters)
		}
	})

	t.Run("resend pointer and respawn use only server links and nonce", func(t *testing.T) {
		config := testConfig(t)
		seedItem(t, config, testItemID, "builder", "routed", "fake:old")
		api := renewedAPI("builder", "routed")
		claimCount := 0
		api.claimFn = func(ClaimRequest) (ClaimResponse, error) {
			request := api.claims[len(api.claims)-1]
			claimCount++
			switch claimCount {
			case 1:
				return ClaimResponse{Directives: []Directive{{
					ID: testDirectiveID, ItemID: testItemID, Fence: 1, Kind: "resend_pointer", Links: testLinks("https://pointer.example"),
				}}}, nil
			case 2:
				if len(request.HeldItems) != 1 || request.HeldItems[0].DirectiveID != testDirectiveID || request.HeldItems[0].DirectiveOutcome != "succeeded" {
					t.Fatalf("resend outcome tick = %#v", request.HeldItems)
				}
				return ClaimResponse{Directives: []Directive{{
					ID: "018f0000-0000-7000-8000-000000000032", ItemID: testItemID, Fence: 1, Kind: "respawn",
					Payload: DirectivePayload{LaunchNonce: testRespawnNonce}, Links: testLinks("https://respawn.example"),
				}}}, nil
			default:
				if len(request.HeldItems) != 1 || request.HeldItems[0].DirectiveID != "018f0000-0000-7000-8000-000000000032" || request.HeldItems[0].DirectiveOutcome != "succeeded" || request.HeldItems[0].LaunchNonce != testRespawnNonce || request.HeldItems[0].SessionRef != "fake:session:1" {
					t.Fatalf("respawn outcome tick = %#v", request.HeldItems)
				}
				return ClaimResponse{}, nil
			}
		}
		launcher := &fakeLauncher{alive: map[string]bool{"fake:old": true}}
		runner := NewRunner(config, api, launcher, staticHealth{}, nil)
		if err := runner.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(launcher.pointers) != 1 {
			t.Fatalf("pointer calls = %#v", launcher.pointers)
		}
		if err := runner.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(launcher.launches) != 1 || launcher.launches[0].LaunchKey != "directive:018f0000-0000-7000-8000-000000000032" {
			t.Fatalf("respawn launches = %#v", launcher.launches)
		}
		state, err := NewStateStore(config.StateFile, config.WorkDir).Load()
		if err != nil {
			t.Fatal(err)
		}
		if state.Items[testItemID].LaunchNonce != testRespawnNonce || state.Items[testItemID].SessionRef != "fake:session:1" {
			t.Fatalf("respawn state = %#v", state.Items[testItemID])
		}
		data, _ := os.ReadFile(filepath.Join(state.Items[testItemID].WorkDir, linksFilename))
		if !strings.Contains(string(data), "respawn.example") {
			t.Fatalf("respawn links = %s", data)
		}
		if err := runner.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRunnerCapacityAndHealthGates(t *testing.T) {
	config := testConfig(t)
	seedItem(t, config, testItemID, "builder", "routed", "fake:leaf")
	seedItemIntoExistingState(t, config, testItemID2, "builder", "routed", "fake:leaf2")
	api := &fakeAPI{
		renewFn: func(request []RenewRequestItem) ([]RenewResult, error) {
			results := make([]RenewResult, 0, len(request))
			for _, held := range request {
				results = append(results, RenewResult{ItemID: held.ItemID, Status: "renewed", Item: &RenewedItem{
					ID: held.ItemID, TaskID: testTaskID, Role: "builder", State: "routed", Fence: 1, Links: testLinks("https://renew.example"),
				}})
			}
			return results, nil
		},
		claimFn: func(ClaimRequest) (ClaimResponse, error) { return ClaimResponse{}, nil },
	}
	launcher := &fakeLauncher{alive: map[string]bool{"fake:leaf": true, "fake:leaf2": true}}
	if err := NewRunner(config, api, launcher, staticHealth{snapshot: HealthSnapshot{Load1m: 1, MemoryPercent: 20}}, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := api.claims[0]
	if request.Capacity != 0 || request.OrchestratorCapacity != 1 || request.CurrentClaimedItemCount != 2 {
		t.Fatalf("split-capacity claim = %#v", request)
	}

	api.claims = nil
	if err := NewRunner(config, api, launcher, staticHealth{snapshot: HealthSnapshot{Load1m: 99, MemoryPercent: 99}}, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	request = api.claims[0]
	if request.Capacity != 0 || request.OrchestratorCapacity != 0 || request.CurrentClaimedItemCount != 2 {
		t.Fatalf("gated claim = %#v", request)
	}

	api.claims = nil
	if err := NewRunner(config, api, launcher, staticHealth{err: errors.New("test health error")}, nil).Run(context.Background()); err == nil {
		t.Fatal("health measurement error did not fail closed")
	}
	if len(api.claims) != 0 {
		t.Fatalf("claim sent after failed health measurement: %#v", api.claims)
	}
}

func TestRunnerRenewBatchNeverExceedsCap(t *testing.T) {
	config := testConfig(t)
	config.MaxConcurrent = MaxBatch
	config.OrchestratorCapacity = 0
	state := State{Version: stateVersion, Items: make(map[string]*ItemState)}
	launcher := &fakeLauncher{alive: make(map[string]bool)}
	for index := 1; index <= MaxBatch; index++ {
		id := fmt.Sprintf("018f0000-0000-7000-8000-%012d", index)
		workDir := filepath.Join(config.WorkDir, id+"-f1")
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			t.Fatal(err)
		}
		state.Items[id] = &ItemState{TaskID: testTaskID, Role: "builder", Fence: 1, Phase: "routed", WorkDir: workDir, LaunchNonce: testNonce, SessionRef: "fake:" + id}
		launcher.alive["fake:"+id] = true
	}
	if err := NewStateStore(config.StateFile, config.WorkDir).Save(state); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		renewFn: func(request []RenewRequestItem) ([]RenewResult, error) {
			results := make([]RenewResult, 0, len(request))
			for _, item := range request {
				results = append(results, RenewResult{ItemID: item.ItemID, Status: "renewed", Item: &RenewedItem{ID: item.ItemID, TaskID: testTaskID, Role: "builder", State: "routed", Fence: 1, Links: testLinks("https://renew.example")}})
			}
			return results, nil
		},
		claimFn: func(ClaimRequest) (ClaimResponse, error) { return ClaimResponse{}, nil },
	}
	if err := NewRunner(config, api, launcher, staticHealth{}, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.renews) != 1 || len(api.renews[0]) != MaxBatch {
		t.Fatalf("renew batch sizes = %#v", api.renews)
	}
}

func TestRunnerSoloRoutingIdentityReuseAndColdReplacement(t *testing.T) {
	t.Run("leaf uses authoritative spoke and configured tool with fresh nonce", func(t *testing.T) {
		config := testSoloConfig(t)
		spokeID := int64(71)
		api := &fakeAPI{claimFn: func(ClaimRequest) (ClaimResponse, error) {
			return ClaimResponse{SpokeID: &spokeID, Items: []ClaimItem{{ID: testItemID, TaskID: testTaskID, Role: "builder", Fence: 1, LaunchNonce: testNonce, Links: testLinks("https://server.example")}}, Directives: []Directive{}}, nil
		}}
		solo := &fakeSoloCall{outputs: []string{"\n❯\n"}}
		launcher := NewSoloLauncher(solo, time.Second)
		launcher.pollInterval = time.Millisecond
		runner := NewRunner(config, api, launcher, staticHealth{}, nil)
		if err := runner.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.transitions) != 2 || api.transitions[0].LaunchNonce != testNonce {
			t.Fatalf("transitions = %#v", api.transitions)
		}
		ref, err := ParseSoloSessionRef(api.transitions[0].SessionRef)
		if err != nil {
			t.Fatal(err)
		}
		if ref.SpokeID != spokeID || ref.SoloProjectID != 8 || ref.SoloProcessID != 77 || ref.LaunchNonce != testNonce || ref.Harness != "opencode" || ref.AgentToolID != 9 {
			t.Fatalf("session identity = %#v", ref)
		}
		spawn := firstSoloCall(t, solo.calls, "spawn_agent")
		if spawn.arguments["project_id"] != int64(8) || spawn.arguments["agent_tool_id"] != int64(9) {
			t.Fatalf("leaf spawn = %#v", spawn.arguments)
		}
	})

	t.Run("orchestrator reusable seat waits for idle and does not spawn", func(t *testing.T) {
		config := testSoloConfig(t)
		spokeID := int64(71)
		ref, _ := (SoloSessionRef{SpokeID: spokeID, SoloProjectID: 12, SoloProcessID: 88, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 13}).String()
		api := &fakeAPI{claimFn: func(ClaimRequest) (ClaimResponse, error) {
			return ClaimResponse{SpokeID: &spokeID, Items: []ClaimItem{{ID: testItemID, TaskID: testTaskID, Role: "orchestrator", Fence: 1, SeatFence: 3, LaunchNonce: testNonce, SessionRef: ref, Links: testLinks("https://server.example")}}, Directives: []Directive{}}, nil
		}}
		solo := &fakeSoloCall{status: soloProcessStatus{ID: 88, Name: "capstan-orchestrator-" + testNonce, Kind: "agent", Status: "Running"}}
		solo.status.AgentState.Idle = true
		launcher := NewSoloLauncher(solo, time.Second)
		launcher.pollInterval = time.Millisecond
		if err := NewRunner(config, api, launcher, staticHealth{}, nil).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, call := range solo.calls {
			if call.name == "spawn_agent" {
				t.Fatal("reusable orchestrator spawned")
			}
		}
		if api.transitions[0].SessionRef != ref {
			t.Fatalf("reused session = %q", api.transitions[0].SessionRef)
		}
	})

	t.Run("dead orchestrator is released and cold-spawned from the returned claim", func(t *testing.T) {
		config := testSoloConfig(t)
		spokeID := int64(71)
		oldRef, _ := (SoloSessionRef{SpokeID: spokeID, SoloProjectID: 12, SoloProcessID: 66, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 13}).String()
		item := seedItem(t, config, testItemID, "orchestrator", "routed", oldRef)
		item.SpokeID = spokeID
		if err := NewStateStore(config.StateFile, config.WorkDir).Save(State{Version: stateVersion, Items: map[string]*ItemState{testItemID: item}}); err != nil {
			t.Fatal(err)
		}
		api := &fakeAPI{
			renewFn: func([]RenewRequestItem) ([]RenewResult, error) {
				return []RenewResult{{ItemID: testItemID, Status: "renewed", Item: &RenewedItem{ID: testItemID, TaskID: testTaskID, Role: "orchestrator", State: "routed", Fence: 1, Links: testLinks("https://renew.example")}}}, nil
			},
			claimFn: func(ClaimRequest) (ClaimResponse, error) {
				return ClaimResponse{SpokeID: &spokeID, Items: []ClaimItem{{ID: testItemID, TaskID: testTaskID, Role: "orchestrator", Fence: 2, LaunchNonce: testNonce, Links: testLinks("https://server.example")}}, Directives: []Directive{}}, nil
			},
		}
		solo := &fakeSoloCall{status: soloProcessStatus{ID: 66, Name: "capstan-orchestrator-" + testNonce, Kind: "agent", Status: "Stopped"}, outputs: []string{"\n❯\n"}}
		launcher := NewSoloLauncher(solo, time.Second)
		launcher.pollInterval = time.Millisecond
		runner := NewRunner(config, api, launcher, staticHealth{}, nil)
		if err := runner.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.transitions) != 3 || api.transitions[0].State != "released" || api.transitions[1].State != "delivered" {
			t.Fatalf("cold replacement transitions = %#v", api.transitions)
		}
		spawn := firstSoloCall(t, solo.calls, "spawn_agent")
		if spawn.arguments["project_id"] != int64(12) || spawn.arguments["agent_tool_id"] != int64(13) {
			t.Fatalf("orchestrator cold spawn = %#v", spawn.arguments)
		}
	})

	t.Run("claim without authoritative spoke id fails before spawn", func(t *testing.T) {
		config := testSoloConfig(t)
		api := &fakeAPI{claimFn: func(ClaimRequest) (ClaimResponse, error) {
			return ClaimResponse{Items: []ClaimItem{{ID: testItemID, TaskID: testTaskID, Role: "builder", Fence: 1, LaunchNonce: testNonce, Links: testLinks("https://server.example")}}, Directives: []Directive{}}, nil
		}}
		solo := &fakeSoloCall{}
		err := NewRunner(config, api, NewSoloLauncher(solo, time.Second), staticHealth{}, nil).Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "authoritative spoke") {
			t.Fatalf("error = %v", err)
		}
		if len(solo.calls) != 0 {
			t.Fatalf("Solo called before spoke validation: %#v", solo.calls)
		}
	})
}

func TestRunnerSoloEndToEndFakeServerCarriesAuthoritativeIdentity(t *testing.T) {
	spokeID := int64(71)
	var transitions []TransitionRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch filepath.Base(request.URL.Path) {
		case "claim":
			writeJSON(t, writer, ClaimResponse{SpokeID: &spokeID, Items: []ClaimItem{{ID: testItemID, TaskID: testTaskID, Role: "builder", Fence: 1, LaunchNonce: testNonce, Links: testLinks(serverURL(request))}}, Directives: []Directive{}})
		case "transition":
			var transition TransitionRequest
			if err := json.NewDecoder(request.Body).Decode(&transition); err != nil {
				t.Fatal(err)
			}
			transitions = append(transitions, transition)
			writeJSON(t, writer, map[string]any{"item": map[string]any{"id": testItemID, "task_id": testTaskID, "role": "builder", "state": transition.State, "fence": 1}})
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	config := testSoloConfig(t)
	config.CapstanURL = server.URL
	solo := &fakeSoloCall{outputs: []string{"\n❯\n"}}
	launcher := NewSoloLauncher(solo, time.Second)
	launcher.pollInterval = time.Millisecond
	if err := NewRunner(config, NewClient(config, "bound-secret"), launcher, staticHealth{}, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 2 || transitions[0].State != "delivered" || transitions[1].State != "routed" {
		t.Fatalf("transitions = %#v", transitions)
	}
	ref, err := ParseSoloSessionRef(transitions[0].SessionRef)
	if err != nil {
		t.Fatal(err)
	}
	if ref.SpokeID != spokeID || ref.SoloProjectID != 8 || ref.SoloProcessID != 77 || ref.LaunchNonce != testNonce || ref.Harness != "opencode" || ref.AgentToolID != 9 {
		t.Fatalf("session ref = %#v", ref)
	}
}

func testSoloConfig(t *testing.T) Config {
	config := testConfig(t)
	config.Launcher = "solo"
	config.RoleCommands = nil
	config.SoloBindings = map[string]SoloBinding{
		"builder":      {ProjectID: 8, AgentToolID: 9, Harness: "opencode", ExtraArgs: []string{}},
		"orchestrator": {ProjectID: 12, AgentToolID: 13, Harness: "claude", ExtraArgs: []string{}},
	}
	config.SoloReadyTimeout = time.Second
	return config
}

func firstSoloCall(t *testing.T, calls []soloCallRecord, name string) soloCallRecord {
	t.Helper()
	for _, call := range calls {
		if call.name == name {
			return call
		}
	}
	t.Fatalf("missing Solo call %s: %#v", name, calls)
	return soloCallRecord{}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(root, "prompt.md")
	orchestrator := filepath.Join(root, "orchestrator.md")
	for _, path := range []string{prompt, orchestrator} {
		if err := os.WriteFile(path, []byte("Read the current links from {links_file}.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return Config{
		Path: filepath.Join(root, "work.json"), CapstanURL: "https://capstan.example", Team: "team", ActorID: "42",
		CredentialFile: filepath.Join(root, "credential"), MaxConcurrent: 2, OrchestratorCapacity: 1,
		MaxLoad1m: 10, MaxMemoryPercent: 90, Capabilities: []string{"git"}, Roles: []string{"builder", "orchestrator"},
		RoleProviders: map[string]string{"builder": "opencode", "orchestrator": "claude"},
		StateFile:     filepath.Join(root, "state", "work.json"), WorkDir: workDir, LockFile: filepath.Join(root, "state", "work.lock"),
		PromptTemplate: prompt, OrchestratorPromptTemplate: orchestrator,
		RoleCommands:   map[string][]string{"builder": {"worker", "--links", "{links_file}"}, "orchestrator": {"orchestrator", "{prompt_file}"}},
		RequestTimeout: 5_000_000_000,
	}
}

func seedItem(t *testing.T, config Config, id, role, phase, session string) *ItemState {
	t.Helper()
	item := &ItemState{TaskID: testTaskID, Role: role, Fence: 1, Phase: phase, WorkDir: filepath.Join(config.WorkDir, id+"-f1"), LaunchNonce: testNonce, SessionRef: session}
	if err := os.MkdirAll(item.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := State{Version: stateVersion, Items: map[string]*ItemState{id: item}}
	if err := NewStateStore(config.StateFile, config.WorkDir).Save(state); err != nil {
		t.Fatal(err)
	}

	return item
}

func seedItemIntoExistingState(t *testing.T, config Config, id, role, phase, session string) {
	t.Helper()
	store := NewStateStore(config.StateFile, config.WorkDir)
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := &ItemState{TaskID: testTaskID, Role: role, Fence: 1, Phase: phase, WorkDir: filepath.Join(config.WorkDir, id+"-f1"), LaunchNonce: testNonce, SessionRef: session}
	if err := os.MkdirAll(item.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state.Items[id] = item
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
}

func renewedAPI(role, phase string) *fakeAPI {
	return &fakeAPI{
		renewFn: func(request []RenewRequestItem) ([]RenewResult, error) {
			return []RenewResult{{ItemID: request[0].ItemID, Status: "renewed", Item: &RenewedItem{
				ID: request[0].ItemID, TaskID: testTaskID, Role: role, State: phase, Fence: 1, Links: testLinks("https://renew.example"),
			}}}, nil
		},
		claimFn: func(ClaimRequest) (ClaimResponse, error) { return ClaimResponse{}, nil },
	}
}

func testLinks(base string) map[string]string {
	return map[string]string{
		"brief": base + "/brief?signature=brief", "milestone": base + "/milestone?signature=milestone",
		"steers": base + "/steers?signature=steers", "evidence": base + "/evidence?signature=evidence",
		"orchestration": base + "/orchestration?signature=context", "orchestrate": base + "/orchestrate?signature=action",
	}
}

func respawnDirective(id, nonce string) Directive {
	return Directive{
		ID: id, ItemID: testItemID, Fence: 1, Kind: "respawn",
		Payload: DirectivePayload{LaunchNonce: nonce}, Links: testLinks("https://respawn.example"),
	}
}

func serverURL(request *http.Request) string { return "http://" + request.Host }

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func writeLinksFile(item *ItemState, links map[string]string) error {
	data, _ := json.Marshal(links)
	return atomicPrivateWrite(filepath.Join(item.WorkDir, linksFilename), data)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %04o, want %04o", path, got, want)
	}
}
