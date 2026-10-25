package work

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func testHybridConfig(t *testing.T) Config {
	t.Helper()
	c := testSoloConfig(t)
	c.Launcher = "hybrid"
	c.AmpPath = "/opt/amp"
	c.AmpRoles = []string{"builder"}
	delete(c.SoloBindings, "builder")
	c.RoleProviders["builder"] = "claude"
	return c
}
func ampClaim() ClaimItem {
	repository := testRepository
	return ClaimItem{ID: testItemID, TaskID: testTaskID, Role: "builder", Fence: 1, LaunchNonce: testNonce, Links: testLinks("https://capstan.example"), Repository: &repository}
}
func claimResponse(items ...ClaimItem) ClaimResponse {
	spoke := int64(42)
	return ClaimResponse{SpokeID: &spoke, Items: items, Directives: []Directive{}}
}
func seedAmpItem(t *testing.T, c Config, phase string) *ItemState {
	t.Helper()
	item := &ItemState{TaskID: testTaskID, Role: "builder", Fence: 1, Phase: phase, WorkDir: filepath.Join(c.WorkDir, testItemID+"-f1"), LaunchNonce: testNonce, SessionRef: testAmpRef(t), Repository: testRepository, SpokeID: 42}
	if err := os.MkdirAll(item.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := NewStateStore(c.StateFile, c.WorkDir).Save(State{Version: stateVersion, Items: map[string]*ItemState{testItemID: item}}); err != nil {
		t.Fatal(err)
	}
	return item
}
func ampRenew(phase string) RenewResult {
	repo := testRepository
	return RenewResult{ItemID: testItemID, Status: "renewed", Item: &RenewedItem{ID: testItemID, TaskID: testTaskID, Role: "builder", Fence: 1, State: phase, Repository: &repo, Links: testLinks("https://capstan.example")}}
}
func loadWorkState(t *testing.T, c Config) State {
	t.Helper()
	s, err := NewStateStore(c.StateFile, c.WorkDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type correctionAPI struct {
	*fakeAPI
	steers   []Steer
	fetches  []string
	fetchErr error
}

func (a *correctionAPI) Steers(_ context.Context, value string) ([]Steer, error) {
	a.fetches = append(a.fetches, value)
	return a.steers, a.fetchErr
}

type messageRecord struct{ ref, text string }
type recordingMessages struct {
	Launcher
	messages []messageRecord
	err      error
	before   func()
}

func (l *recordingMessages) Continue(_ context.Context, ref, text string) error {
	if l.before != nil {
		l.before()
	}
	l.messages = append(l.messages, messageRecord{ref, text})
	return l.err
}

func TestAmpRunnerDispatchOrderingMetadataRepositoryAndNoLeaks(t *testing.T) {
	c := testHybridConfig(t)
	commands := &fakeAmpCommands{}
	launcher := NewHybridLauncher(&fakeLauncher{}, NewAmpLauncher(c.AmpPath, commands), c.AmpRoles)
	api := &fakeAPI{claimFn: func(ClaimRequest) (ClaimResponse, error) { return claimResponse(ampClaim()), nil }}
	api.transitionFn = func(request TransitionRequest) error {
		if len(commands.calls) != 3 {
			t.Fatal("transition before initialized dispatch")
		}
		s := loadWorkState(t, c)
		if s.Items[testItemID].SessionRef != testAmpRef(t) {
			t.Fatal("transition before persisted identity")
		}
		return nil
	}
	var logs []string
	if err := NewRunner(c, api, launcher, staticHealth{}, func(s string) { logs = append(logs, s) }).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.transitions) != 2 || api.transitions[0].State != "delivered" || api.transitions[0].SessionRef != testAmpRef(t) || api.transitions[0].LaunchNonce != testNonce || api.transitions[1].State != "routed" {
		t.Fatalf("transitions: %#v", api.transitions)
	}
	s := loadWorkState(t, c)
	if s.Items[testItemID].Repository != testRepository || s.Items[testItemID].Phase != "routed" || api.claims[0].ItemShape != "repository-v1" {
		t.Fatal("identity/shape not persisted")
	}
	data, _ := os.ReadFile(c.StateFile)
	for _, value := range append(logs, string(data)) {
		for _, secret := range []string{"signature=", "Bearer ", "private prompt"} {
			if strings.Contains(value, secret) {
				t.Fatal("secret leaked to log/state")
			}
		}
	}
}

func TestAmpRunnerNullRepositoryReleasesWithoutGuessing(t *testing.T) {
	for _, repository := range []*string{nil, ptrString("Owner/repo"), ptrString("https://github.com/example/work"), ptrString("")} {
		c := testHybridConfig(t)
		commands := &fakeAmpCommands{}
		claim := ampClaim()
		claim.Repository = repository
		api := &fakeAPI{claimFn: func(ClaimRequest) (ClaimResponse, error) { return claimResponse(claim), nil }}
		var logs []string
		if err := NewRunner(c, api, NewHybridLauncher(&fakeLauncher{}, NewAmpLauncher(c.AmpPath, commands), c.AmpRoles), staticHealth{}, func(s string) { logs = append(logs, s) }).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(commands.calls) != 0 || len(api.transitions) != 1 || api.transitions[0].State != "released" || len(loadWorkState(t, c).Items) != 0 {
			t.Fatal("null/invalid repository dispatched or held")
		}
		if len(logs) == 0 || !strings.Contains(logs[0], "explicit repository") {
			t.Fatal("missing clear redacted reason")
		}
	}
}
func ptrString(value string) *string { return &value }

func TestAmpRunnerRepositoryMismatchFailsClosedBeforeDelivery(t *testing.T) {
	for _, repository := range []*string{nil, ptrString("other/repo")} {
		c := testHybridConfig(t)
		seedAmpItem(t, c, "routed")
		before, _ := os.ReadFile(c.StateFile)
		renew := ampRenew("routed")
		renew.Item.Repository = repository
		api := &fakeAPI{renewFn: func([]RenewRequestItem) ([]RenewResult, error) { return []RenewResult{renew}, nil }}
		commands := &fakeAmpCommands{}
		err := NewRunner(c, api, NewHybridLauncher(&fakeLauncher{}, NewAmpLauncher(c.AmpPath, commands), c.AmpRoles), staticHealth{}, nil).Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "repository") || len(commands.calls) != 0 || len(api.claims) != 0 || len(api.transitions) != 0 {
			t.Fatal("repository mismatch did not fail closed")
		}
		after, _ := os.ReadFile(c.StateFile)
		if string(before) != string(after) {
			t.Fatal("mismatch changed holding")
		}
	}
}

func TestAmpRunnerAwaitingAndAcceptLostNeverProbeOrKillRemote(t *testing.T) {
	c := testHybridConfig(t)
	seedAmpItem(t, c, "routed")
	phase := "awaiting_verification"
	lost := false
	api := &fakeAPI{renewFn: func([]RenewRequestItem) ([]RenewResult, error) {
		if lost {
			return []RenewResult{{ItemID: testItemID, Status: "lost"}}, nil
		}
		return []RenewResult{ampRenew(phase)}, nil
	}}
	// Any launcher operation at all is a regression while awaiting or forgetting an accepted orb.
	l := &fakeLauncher{aliveErr: errors.New("local stream has exited")}
	r := NewRunner(c, api, l, staticHealth{}, nil)
	for i := 0; i < 2; i++ {
		if err := r.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if loadWorkState(t, c).Items[testItemID].Phase != "awaiting_verification" || len(api.transitions) != 0 || len(l.launches) != 0 || len(l.gates) != 0 {
		t.Fatal("waiting item routed/released/relaunched")
	}
	lost = true
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(loadWorkState(t, c).Items) != 0 || len(l.terminated) != 0 || len(api.transitions) != 0 {
		t.Fatal("accepted orb was killed or retained")
	}
}

func TestAmpRunnerCorrectionSameThreadNoReplayAndCrashRetry(t *testing.T) {
	c := testHybridConfig(t)
	seedAmpItem(t, c, "awaiting_verification")
	api := &correctionAPI{fakeAPI: &fakeAPI{renewFn: func([]RenewRequestItem) ([]RenewResult, error) { return []RenewResult{ampRenew("routed")}, nil }}, steers: []Steer{
		{ID: testDirectiveID, TaskID: testTaskID, Text: "task-wide context"},
		{ID: testDirectiveID2, TaskID: testTaskID, ItemID: testItemID, Text: "fix the failing assertion"},
	}}
	l := &recordingMessages{Launcher: &fakeLauncher{}, err: errors.New("simulated interruption")}
	l.before = func() {
		s := loadWorkState(t, c)
		if !s.Items[testItemID].CorrectionPending || s.Items[testItemID].Phase != "routed" || len(s.Items[testItemID].DeliveredSteers) != 0 {
			t.Fatal("correction was not durable before send")
		}
	}
	r := NewRunner(c, api, l, staticHealth{}, nil)
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("interrupted correction passed")
	}
	s := loadWorkState(t, c)
	if !s.Items[testItemID].CorrectionPending || len(s.Items[testItemID].DeliveredSteers) != 0 {
		t.Fatal("crash silently lost correction")
	}
	l.err = nil
	l.before = nil
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.messages) != 2 || l.messages[1].ref != testAmpRef(t) || l.messages[1].text != "fix the failing assertion" || len(api.transitions) != 0 {
		t.Fatalf("continuation: %#v", l.messages)
	}
	if len(api.fetches) != 2 || api.fetches[1] != ampRenew("routed").Item.Links["steers"] {
		t.Fatal("did not use refreshed signed endpoint")
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.messages) != 2 || len(api.fetches) != 3 {
		t.Fatal("ordinary renewal did not poll or replayed a correction")
	}
	// A later verification episode returns the same steer plus a new one.
	s = loadWorkState(t, c)
	s.Items[testItemID].Phase = "awaiting_verification"
	if err := r.store.Save(s); err != nil {
		t.Fatal(err)
	}
	api.steers = append(api.steers, Steer{ID: testItemID2, TaskID: testTaskID, ItemID: testItemID, Text: "one more correction"})
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.messages) != 3 || l.messages[2].text != "one more correction" {
		t.Fatal("old steer replayed/new steer lost")
	}
	data, _ := os.ReadFile(c.StateFile)
	for _, secret := range []string{"fix the failing assertion", "one more correction", "signature="} {
		if strings.Contains(string(data), secret) {
			t.Fatal("correction content persisted in recovery metadata")
		}
	}
}

func TestAmpRunnerCorrectionResponseIsValidatedBeforeAnySend(t *testing.T) {
	for _, bad := range []Steer{
		{ID: testDirectiveID2, TaskID: testTaskID, ItemID: testItemID2, Text: "other item"},
		{ID: testDirectiveID2, TaskID: testTaskID, ItemID: testItemID, Text: strings.Repeat("x", 8193)},
		{ID: testDirectiveID2, TaskID: testItemID2, ItemID: testItemID, Text: "other task"},
	} {
		c := testHybridConfig(t)
		seedAmpItem(t, c, "awaiting_verification")
		l := &recordingMessages{Launcher: &fakeLauncher{}}
		api := &correctionAPI{fakeAPI: &fakeAPI{renewFn: func([]RenewRequestItem) ([]RenewResult, error) { return []RenewResult{ampRenew("routed")}, nil }}, steers: []Steer{{ID: testDirectiveID, TaskID: testTaskID, ItemID: testItemID, Text: "first"}, bad}}
		if err := NewRunner(c, api, l, staticHealth{}, nil).Run(context.Background()); err == nil || len(l.messages) != 0 {
			t.Fatal("partially sent malformed steers")
		}
	}
}

func TestAmpRunnerDeadlineNudgeDurableReplayOutcomeAndOnlyServerAuthority(t *testing.T) {
	c := testHybridConfig(t)
	seedAmpItem(t, c, "routed")
	commands := &fakeAmpCommands{}
	api := &fakeAPI{renewFn: func([]RenewRequestItem) ([]RenewResult, error) { return []RenewResult{ampRenew("routed")}, nil }}
	directive := Directive{ID: testDirectiveID, ItemID: testItemID, Fence: 1, Kind: "nudge", Payload: DirectivePayload{Message: "Post work progress or declare the work finished."}}
	api.claimFn = func(ClaimRequest) (ClaimResponse, error) {
		response := claimResponse()
		response.Directives = []Directive{directive}
		return response, nil
	}
	l := NewHybridLauncher(&fakeLauncher{}, NewAmpLauncher(c.AmpPath, commands), c.AmpRoles)
	r := NewRunner(c, api, l, staticHealth{}, nil)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(commands.calls) != 2 || !reflect.DeepEqual(commands.calls[1].args, []string{"threads", "continue", testAmpThread, "-ox", directive.Payload.Message}) {
		t.Fatal("nudge did not continue the exact thread")
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(commands.calls) != 2 || len(api.claims[1].HeldItems) != 1 || api.claims[1].HeldItems[0].DirectiveID != testDirectiveID || api.claims[1].HeldItems[0].DirectiveOutcome != "succeeded" {
		t.Fatal("nudge replay/outcome regression")
	}
	api.claimFn = func(ClaimRequest) (ClaimResponse, error) { return claimResponse(), nil }
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(commands.calls) != 2 {
		t.Fatal("renewal generated a local continuation")
	}
}

func TestAmpRunnerRepairAndServerAuthorizedRespawn(t *testing.T) {
	c := testHybridConfig(t)
	seedAmpItem(t, c, "routed")
	commands := &fakeAmpCommands{}
	l := NewHybridLauncher(&fakeLauncher{}, NewAmpLauncher(c.AmpPath, commands), c.AmpRoles)
	api := &fakeAPI{renewFn: func([]RenewRequestItem) ([]RenewResult, error) { return []RenewResult{ampRenew("routed")}, nil }}
	r := NewRunner(c, api, l, staticHealth{}, nil)
	s := loadWorkState(t, c)
	if err := r.handleDirectives(context.Background(), &s, []Directive{{ID: testItemID2, ItemID: testItemID, Fence: 1, Kind: "respawn", Payload: DirectivePayload{LaunchNonce: testNonce}, Links: testLinks("https://capstan.example")}}); err == nil || len(commands.calls) != 0 {
		t.Fatal("respawn did not require a rotated server nonce")
	}
	for _, kind := range []string{"send_enter", "resend_pointer"} {
		if err := r.handleDirectives(context.Background(), &s, []Directive{{ID: testDirectiveID, ItemID: testItemID, Fence: 1, Kind: kind, Links: testLinks("https://capstan.example")}}); err != nil {
			t.Fatal(err)
		}
		if commands.calls[len(commands.calls)-1].args[2] != testAmpThread {
			t.Fatal("repair replaced thread")
		}
		// Distinct repair episodes have distinct directive identities.
		s.Items[testItemID].Directive = nil
	}
	commands.start = func([]string) ([]byte, error) {
		return []byte(strings.ReplaceAll(string(ampInitJSON()), testAmpThread, "T-01a1734d-7a34-73f8-b2c8-1eeed10a1d15")), nil
	}
	d := Directive{ID: testDirectiveID2, ItemID: testItemID, Fence: 1, Kind: "respawn", Payload: DirectivePayload{LaunchNonce: testRespawnNonce}, Links: testLinks("https://capstan.example")}
	if err := r.handleDirectives(context.Background(), &s, []Directive{d}); err != nil {
		t.Fatal(err)
	}
	if s.Items[testItemID].LaunchNonce != testRespawnNonce || s.Items[testItemID].SessionRef == testAmpRef(t) || commands.calls[len(commands.calls)-1].nonce != testRespawnNonce {
		t.Fatal("authorized respawn did not rotate identity")
	}
	held := reportHeld(s)
	if held[0].LaunchNonce != testRespawnNonce || held[0].SessionRef != s.Items[testItemID].SessionRef || held[0].DirectiveOutcome != "succeeded" {
		t.Fatal("replacement identity not reported")
	}
	starts := len(commands.calls)
	if err := r.handleDirectives(context.Background(), &s, []Directive{d}); err != nil || len(commands.calls) != starts {
		t.Fatal("respawn directive replay dispatched again")
	}
}

func TestAmpRunnerUncertainDispatchRetainsClaimForInitRecovery(t *testing.T) {
	c := testHybridConfig(t)
	commands := &fakeAmpCommands{start: func([]string) ([]byte, error) { return nil, errAmpDispatchUncertain }}
	api := &fakeAPI{claimFn: func(ClaimRequest) (ClaimResponse, error) { return claimResponse(ampClaim()), nil }, renewFn: func([]RenewRequestItem) ([]RenewResult, error) { return []RenewResult{ampRenew("claimed")}, nil }}
	r := NewRunner(c, api, NewHybridLauncher(&fakeLauncher{}, NewAmpLauncher(c.AmpPath, commands), c.AmpRoles), staticHealth{}, nil)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.transitions) != 0 || loadWorkState(t, c).Items[testItemID].PendingLaunchKey == "" {
		t.Fatal("uncertain dispatch was released/forgotten")
	}
	commands.start = nil
	api.claimFn = func(ClaimRequest) (ClaimResponse, error) { return claimResponse(), nil }
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if loadWorkState(t, c).Items[testItemID].SessionRef != testAmpRef(t) || len(api.transitions) != 2 {
		t.Fatal("initialization did not recover")
	}
}

func TestCorrectionCrashAfterRemoteSendBeforeSaveRetries(t *testing.T) {
	c := testHybridConfig(t)
	seedAmpItem(t, c, "routed")
	api := &correctionAPI{fakeAPI: &fakeAPI{renewFn: func([]RenewRequestItem) ([]RenewResult, error) { return []RenewResult{ampRenew("routed")}, nil }}, steers: []Steer{{ID: testDirectiveID, TaskID: testTaskID, ItemID: testItemID, Text: "retry after crash"}}}
	l := &recordingMessages{Launcher: &fakeLauncher{}}
	backup := c.StateFile + ".backup"
	l.before = func() {
		if err := os.Rename(c.StateFile, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(c.StateFile, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRunner(c, api, l, staticHealth{}, nil)
	if err := r.Run(context.Background()); err == nil || len(l.messages) != 1 {
		t.Fatal("simulated post-send state failure passed")
	}
	if err := os.Remove(c.StateFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, c.StateFile); err != nil {
		t.Fatal(err)
	}
	s := loadWorkState(t, c)
	if !s.Items[testItemID].CorrectionPending || len(s.Items[testItemID].DeliveredSteers) != 0 {
		t.Fatal("post-send crash lost pending correction")
	}
	l.before = nil
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.messages) != 2 || l.messages[0] != l.messages[1] {
		t.Fatal("post-send crash did not retry exact correction/thread")
	}
}

func TestAmpRoutedPollDeliversInterTickCorrectionsAndOrdinaryItemSteers(t *testing.T) {
	for _, source := range []string{"inter-tick-correction", "ordinary-item-steer"} {
		t.Run(source, func(t *testing.T) {
			c := testHybridConfig(t)
			seedAmpItem(t, c, "routed")
			api := &correctionAPI{fakeAPI: &fakeAPI{}, steers: []Steer{}}
			ticks := 0
			api.renewFn = func([]RenewRequestItem) ([]RenewResult, error) {
				ticks++
				result := ampRenew("routed")
				result.Item.Links["steers"] += "&tick=" + strconv.Itoa(ticks)
				return []RenewResult{result}, nil
			}
			commands := &fakeAmpCommands{}
			r := NewRunner(c, api, NewHybridLauncher(&fakeLauncher{}, NewAmpLauncher(c.AmpPath, commands), c.AmpRoles), staticHealth{}, nil)
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(api.fetches) != 1 || len(commands.calls) != 0 {
				t.Fatal("empty routed holding was not polled without continuation")
			}
			if loadWorkState(t, c).Items[testItemID].Phase != "routed" {
				t.Fatal("fixture did not remain routed")
			}
			// A finish/corrections episode may occur entirely between these ticks.
			// An ordinary orchestrator item steer has the same worker representation.
			api.steers = []Steer{
				{ID: testDirectiveID, TaskID: testTaskID, Text: "task-wide context is not an item steer"},
				{ID: testDirectiveID2, TaskID: testTaskID, ItemID: testItemID, Text: source},
			}
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(commands.calls) != 2 || !reflect.DeepEqual(commands.calls[1].args, []string{"threads", "continue", testAmpThread, "-ox", source}) {
				t.Fatal("unobserved/ordinary item steer did not continue the exact thread")
			}
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(commands.calls) != 2 || len(api.fetches) != 3 {
				t.Fatal("normal renewal stopped polling or replayed delivered steer")
			}
			for i, value := range api.fetches {
				if !strings.HasSuffix(value, "&tick="+strconv.Itoa(i+1)) {
					t.Fatal("poll used stale signed endpoint")
				}
			}
			state := loadWorkState(t, c)
			if state.Items[testItemID].SessionRef != testAmpRef(t) || !reflect.DeepEqual(state.Items[testItemID].DeliveredSteers, []string{testDirectiveID2}) || state.Items[testItemID].CorrectionPending || len(api.transitions) != 0 {
				t.Fatal("steer delivery changed thread/claim or lost durable no-replay state")
			}
		})
	}
}

func TestCorrectionMissingSteerAfterFetchCrashRetainsPending(t *testing.T) {
	c := testHybridConfig(t)
	seedAmpItem(t, c, "routed")
	state := loadWorkState(t, c)
	state.Items[testItemID].CorrectionPending = true
	state.Items[testItemID].PendingSteers = []string{testDirectiveID}
	if err := NewStateStore(c.StateFile, c.WorkDir).Save(state); err != nil {
		t.Fatal(err)
	}
	api := &correctionAPI{fakeAPI: &fakeAPI{renewFn: func([]RenewRequestItem) ([]RenewResult, error) { return []RenewResult{ampRenew("routed")}, nil }}, steers: []Steer{}}
	l := &recordingMessages{Launcher: &fakeLauncher{}}
	r := NewRunner(c, api, l, staticHealth{}, nil)
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("empty correction fetch silently completed delivery")
	}
	if !loadWorkState(t, c).Items[testItemID].CorrectionPending || len(l.messages) != 0 {
		t.Fatal("unavailable correction was forgotten")
	}
	api.steers = []Steer{{ID: testDirectiveID, TaskID: testTaskID, ItemID: testItemID, Text: "recovered correction"}}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.messages) != 1 || loadWorkState(t, c).Items[testItemID].CorrectionPending {
		t.Fatal("available correction did not recover")
	}
}

func TestRunnerSoloNudgeAndGenericUnsupportedOutcomeReplay(t *testing.T) {
	for _, solo := range []bool{true, false} {
		c := testConfig(t)
		var launcher Launcher
		var calls *fakeSoloCall
		session := "fake:session"
		if solo {
			c = testSoloConfig(t)
			var err error
			session, err = (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "opencode", AgentToolID: 9}).String()
			if err != nil {
				t.Fatal(err)
			}
			calls = &fakeSoloCall{status: soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}}
			launcher = NewSoloLauncher(calls, c.SoloReadyTimeout)
		} else {
			launcher = &fakeLauncher{}
		}
		item := seedItem(t, c, testItemID, "builder", "routed", session)
		if err := atomicPrivateWrite(filepath.Join(item.WorkDir, linksFilename), mustJSON(t, testLinks("https://capstan.example"))); err != nil {
			t.Fatal(err)
		}
		if solo {
			if err := atomicPrivateWrite(filepath.Join(item.WorkDir, gateFilename)+"."+testNonce, []byte(session+"\n")); err != nil {
				t.Fatal(err)
			}
		}
		api := renewedAPI("builder", "routed")
		api.claimFn = func(ClaimRequest) (ClaimResponse, error) {
			response := claimResponse()
			response.Directives = []Directive{{ID: testDirectiveID, ItemID: testItemID, Fence: 1, Kind: "nudge", Payload: DirectivePayload{Message: "Post work progress."}}}
			return response, nil
		}
		r := NewRunner(c, api, launcher, staticHealth{}, nil)
		for i := 0; i < 2; i++ {
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		want := "failed"
		if solo {
			want = "succeeded"
		}
		if api.claims[1].HeldItems[0].DirectiveOutcome != want {
			t.Fatal("wrong durable nudge outcome")
		}
		if solo {
			sent := 0
			for _, call := range calls.calls {
				if call.name == "send_input" {
					sent++
				}
			}
			if sent != 2 {
				t.Fatal("Solo nudge replayed or not submitted")
			}
		}
	}
}
