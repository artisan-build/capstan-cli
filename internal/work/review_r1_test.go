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
	"strings"
	"testing"
)

func TestReviewR1FutureVersionsAndLegalUnicodeMessages(t *testing.T) {
	for _, version := range []string{"0.1.0", "1.0.0", "1.0.1792929650", "0.0.1792929650-g000512 (production)\r\n"} {
		t.Run(version, func(t *testing.T) {
			if err := validateAmpVersion([]byte(version)); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, version := range []string{"0.0.1792929649", "01.0.0", "0.01.0", "1.0.00", "1.0.0\njunk", "1.0.0\r", "1.0.0-gBAD", "18446744073709551616.0.0"} {
		if err := validateAmpVersion([]byte(version)); err == nil {
			t.Fatalf("invalid version accepted: %q", version)
		}
	}
	for _, version := range []string{"2.1.0", "3.0.0"} {
		frame := strings.Replace(observedOpenCodeFrame("Fix broken tests"), "2.0.26", version, 1)
		if !readyForHarness("opencode", frame) {
			t.Errorf("complete frame version %s rejected", version)
		}
		if readyForHarness("opencode", strings.Replace(frame, version, "", 1)) {
			t.Error("missing-version frame accepted")
		}
	}
	for _, text := range []string{strings.Repeat("—", 4000), strings.Repeat("界", 8000), strings.Repeat("😀", 8000)} {
		if !validSessionMessage(text) {
			t.Errorf("legal Unicode message refused: %d bytes", len(text))
		}
	}
	for _, text := range []string{strings.Repeat("x", 8001), strings.Repeat("😀", 8001), string([]byte{0xff}), "a\x00b", " "} {
		if validSessionMessage(text) {
			t.Error("invalid message accepted")
		}
	}
}

func TestReviewR1SteerEnvelopeSupportsBoundedLegalUTF8(t *testing.T) {
	steers := make([]Steer, 256)
	for i := range steers {
		steers[i] = Steer{ID: fmt.Sprintf("018f0000-0000-7000-8000-%012x", i+1), TaskID: testTaskID, ItemID: testItemID, Text: strings.Repeat("😀", 8000)}
	}
	body, err := json.Marshal(map[string]any{"steers": steers})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer leaked to signed endpoint")
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	c := testConfig(t)
	c.CapstanURL = server.URL
	client := NewClient(c, "fixture-secret")
	for _, escaped := range []bool{false, true} {
		if escaped {
			body = []byte(strings.ReplaceAll(string(body), "😀", `\ud83d\ude00`))
		}
		got, err := client.Steers(context.Background(), server.URL+"/steers?signature=fixture")
		if err != nil || len(got) != 256 || got[0].Text != steers[0].Text {
			t.Fatalf("legal envelope (%d bytes, escaped=%t) refused: %v", len(body), escaped, err)
		}
	}
	// A count beyond the durable ledger still fails this item closed.
	steers = append(steers, steers[0])
	body, _ = json.Marshal(map[string]any{"steers": steers})
	if _, err := client.Steers(context.Background(), server.URL+"/steers?signature=fixture"); err == nil {
		t.Fatal("257 steers accepted")
	}
}

type reviewR1API struct {
	*fakeAPI
	fetches []string
	fetch   func(string) ([]Steer, error)
}

func (a *reviewR1API) Steers(_ context.Context, link string) ([]Steer, error) {
	a.fetches = append(a.fetches, link)
	return a.fetch(link)
}

type reviewR1Launcher struct {
	fakeLauncher
	messages  []messageRecord
	failedRef string
}

func (l *reviewR1Launcher) Continue(_ context.Context, ref, text string) error {
	l.messages = append(l.messages, messageRecord{ref, text})
	if ref == l.failedRef {
		return errors.New("private delivery output signature=secret")
	}
	return nil
}

func seedReviewR1Siblings(t *testing.T, c Config) string {
	t.Helper()
	seedAmpItem(t, c, "routed")
	s := loadWorkState(t, c)
	second := *s.Items[testItemID]
	second.WorkDir = filepath.Join(c.WorkDir, testItemID2+"-f1")
	thread := strings.TrimSuffix(testAmpThread, "4") + "5"
	ref, err := (AmpSessionRef{ProjectID: testAmpProject, ThreadID: thread, ThreadURL: "https://ampcode.com/threads/" + thread}).String()
	if err != nil {
		t.Fatal(err)
	}
	second.SessionRef = ref
	if err := os.MkdirAll(second.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.Items[testItemID2] = &second
	if err := NewStateStore(c.StateFile, c.WorkDir).Save(s); err != nil {
		t.Fatal(err)
	}
	return ref
}

func reviewR1Renew(request []RenewRequestItem) []RenewResult {
	results := make([]RenewResult, 0, len(request))
	for _, holding := range request {
		r := ampRenew("routed")
		r.ItemID = holding.ItemID
		r.Item.ID = holding.ItemID
		r.Item.Links["steers"] += "&item=" + holding.ItemID
		results = append(results, r)
	}
	return results
}
func reviewR1Claim(ClaimRequest) (ClaimResponse, error) {
	response := claimResponse()
	response.Directives = []Directive{
		{ID: testDirectiveID, ItemID: testItemID, Fence: 1, Kind: "nudge", Payload: DirectivePayload{Message: "do not send to failed holding"}},
		{ID: testItemID2, ItemID: testItemID2, Fence: 1, Kind: "nudge", Payload: DirectivePayload{Message: "sibling deadline nudge"}},
	}
	return response, nil
}

func TestReviewR1SteerFailureIsolatesItemAndPreservesMachineHeartbeat(t *testing.T) {
	for _, failure := range []string{"fetch", "validation", "delivery", "missing_pending", "count"} {
		t.Run(failure, func(t *testing.T) {
			c := testHybridConfig(t)
			siblingRef := seedReviewR1Siblings(t, c)
			if failure == "missing_pending" {
				s := loadWorkState(t, c)
				s.Items[testItemID].CorrectionPending = true
				s.Items[testItemID].PendingSteers = []string{testDirectiveID2}
				if err := NewStateStore(c.StateFile, c.WorkDir).Save(s); err != nil {
					t.Fatal(err)
				}
			}
			api := &reviewR1API{fakeAPI: &fakeAPI{renewFn: func(req []RenewRequestItem) ([]RenewResult, error) { return reviewR1Renew(req), nil }, claimFn: reviewR1Claim}}
			api.fetch = func(link string) ([]Steer, error) {
				if strings.HasSuffix(link, testItemID2) {
					return []Steer{{ID: testRespawnNonce, TaskID: testTaskID, ItemID: testItemID2, Text: "sibling steer"}}, nil
				}
				switch failure {
				case "fetch":
					return nil, errors.New("private fetch output signature=secret")
				case "validation":
					return []Steer{{ID: testDirectiveID2, TaskID: testTaskID, ItemID: testItemID, Text: strings.Repeat("x", 8001)}}, nil
				case "missing_pending":
					return []Steer{}, nil
				case "count":
					return make([]Steer, 257), nil
				default:
					return []Steer{{ID: testDirectiveID2, TaskID: testTaskID, ItemID: testItemID, Text: "private failed steer"}}, nil
				}
			}
			launcher := &reviewR1Launcher{}
			if failure == "delivery" {
				launcher.failedRef = testAmpRef(t)
			}
			var logs []string
			if err := NewRunner(c, api, launcher, staticHealth{}, func(s string) { logs = append(logs, s) }).Run(context.Background()); err != nil {
				t.Fatalf("one item aborted machine tick: %v", err)
			}
			if len(api.fetches) != 2 || len(api.claims) != 1 || len(api.claims[0].HeldItems) != 2 {
				t.Fatal("poisoned holding suppressed sibling/claim heartbeat")
			}
			want := []messageRecord{{siblingRef, "sibling steer"}, {siblingRef, "sibling deadline nudge"}}
			got := launcher.messages
			if failure == "delivery" {
				if len(got) == 0 || got[0].ref != testAmpRef(t) {
					t.Fatal("delivery failure not exercised")
				}
				got = got[1:]
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("sibling directives/steers not isolated")
			}
			s := loadWorkState(t, c)
			if len(s.Items[testItemID].DeliveredSteers) != 0 || s.Items[testItemID].Directive != nil {
				t.Fatal("failed holding was acknowledged or directed")
			}
			if failure == "delivery" || failure == "missing_pending" {
				if !s.Items[testItemID].CorrectionPending || !reflect.DeepEqual(s.Items[testItemID].PendingSteers, []string{testDirectiveID2}) {
					t.Fatal("pending text identity lost")
				}
			}
			if len(launcher.terminated) != 0 || len(launcher.launches) != 0 {
				t.Fatal("failure replaced or killed an orb")
			}
			if len(logs) == 0 || !strings.Contains(logs[0], testItemID) {
				t.Fatal("missing item-scoped failure log")
			}
			for _, log := range logs {
				for _, secret := range []string{"private", "signature=", testRepository, "sibling steer"} {
					if strings.Contains(log, secret) {
						t.Fatal("item failure log leaked content")
					}
				}
			}
		})
	}
}

func TestReviewR1RepositoryChangeReleasesOnlyAffectedHolding(t *testing.T) {
	for _, disposition := range []string{"success", "conflict", "api_failure"} {
		t.Run(disposition, func(t *testing.T) {
			c := testHybridConfig(t)
			siblingRef := seedReviewR1Siblings(t, c)
			api := &reviewR1API{fakeAPI: &fakeAPI{claimFn: reviewR1Claim}}
			api.renewFn = func(req []RenewRequestItem) ([]RenewResult, error) {
				results := reviewR1Renew(req)
				results[0].Item.Repository = ptrString("other/repo")
				return results, nil
			}
			api.transitionFn = func(request TransitionRequest) error {
				if request.ItemID != testItemID || request.Fence != 1 || request.State != "released" {
					t.Fatal("incorrect release authority")
				}
				if disposition == "conflict" {
					return APIError{Status: 409}
				}
				if disposition == "api_failure" {
					return errors.New("private release output signature=secret")
				}
				return nil
			}
			api.fetch = func(link string) ([]Steer, error) {
				if !strings.HasSuffix(link, testItemID2) {
					t.Fatal("changed binding fetched steers")
				}
				return []Steer{}, nil
			}
			l := &reviewR1Launcher{}
			var logs []string
			if err := NewRunner(c, api, l, staticHealth{}, func(s string) { logs = append(logs, s) }).Run(context.Background()); err != nil {
				t.Fatalf("repository change aborted machine: %v", err)
			}
			if len(api.transitions) != 1 || len(api.claims) != 1 || len(api.fetches) != 1 {
				t.Fatal("repository change suppressed recovery/heartbeat")
			}
			if !reflect.DeepEqual(l.messages, []messageRecord{{siblingRef, "sibling deadline nudge"}}) || len(l.terminated) != 0 {
				t.Fatal("changed binding received text or was killed")
			}
			s := loadWorkState(t, c)
			_, held := s.Items[testItemID]
			if held != (disposition == "api_failure") || s.Items[testItemID2] == nil {
				t.Fatal("incorrect release/forget disposition")
			}
			if held && s.Items[testItemID].Repository != testRepository {
				t.Fatal("failed release overwrote binding")
			}
			for _, log := range logs {
				for _, secret := range []string{"private", "signature=", "other/repo", testRepository} {
					if strings.Contains(log, secret) {
						t.Fatal("release failure log leaked content")
					}
				}
			}
		})
	}
}
