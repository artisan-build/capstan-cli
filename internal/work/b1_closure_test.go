package work

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type b1RecoveryCommands struct {
	fakeAmpCommands
	native nativeAmpCommands
	nonces []string
}

func (c *b1RecoveryCommands) Start(ctx context.Context, path string, args []string, dir, nonce string) ([]byte, error) {
	c.nonces = append(c.nonces, nonce)
	if nonce == testNonce {
		return c.native.Start(ctx, path, args, dir, nonce)
	}
	return c.fakeAmpCommands.Start(ctx, path, args, dir, nonce)
}

func TestB1AmbiguousRecoveryKeepsHeartbeatAndAllowsRotatedRespawn(t *testing.T) {
	c := testHybridConfig(t)
	siblingRef := seedReviewR1Siblings(t, c)
	s := loadWorkState(t, c)
	ambiguous := s.Items[testItemID]
	ambiguous.Phase = "claimed"
	ambiguous.SessionRef = ""
	ambiguous.PendingLaunchKey = "initial:" + testNonce
	original := *ambiguous
	marker := filepath.Join(ambiguous.WorkDir, "amp-"+testNonce+".started")
	const markerContents = "durable start marker\n"
	if err := os.WriteFile(marker, []byte(markerContents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewStateStore(c.StateFile, c.WorkDir).Save(s); err != nil {
		t.Fatal(err)
	}

	commands := &b1RecoveryCommands{native: nativeAmpCommands{
		startupTimeout: time.Millisecond,
		helperCommand: func(string, ...string) *exec.Cmd {
			t.Fatal("ambiguous nonce dispatched a second helper")
			return nil
		},
	}}
	newThread := strings.TrimSuffix(testAmpThread, "4") + "6"
	commands.start = func([]string) ([]byte, error) {
		return []byte(strings.ReplaceAll(string(ampInitJSON()), testAmpThread, newThread)), nil
	}
	api := &reviewR1API{fakeAPI: &fakeAPI{}}
	api.renewFn = func(request []RenewRequestItem) ([]RenewResult, error) {
		results := reviewR1Renew(request)
		results[0].Item.State = "claimed"
		return results, nil
	}
	api.fetch = func(link string) ([]Steer, error) {
		if !strings.HasSuffix(link, testItemID2) {
			t.Fatal("ambiguous holding polled steers")
		}
		return []Steer{{ID: testDirectiveID2, TaskID: testTaskID, ItemID: testItemID2, Text: "healthy sibling steer"}}, nil
	}
	api.claimFn = func(request ClaimRequest) (ClaimResponse, error) {
		if len(request.HeldItems) != 2 {
			t.Fatal("heartbeat omitted a holding")
		}
		if request.HeldItems[0].ItemID != testItemID || request.HeldItems[0].Fence != original.Fence || request.HeldItems[0].SessionRef != "" || request.HeldItems[0].LaunchNonce != "" {
			t.Fatal("ambiguous heartbeat invented launch/receipt metadata")
		}
		if request.HeldItems[1].ItemID != testItemID2 || request.HeldItems[1].SessionRef != siblingRef {
			t.Fatal("healthy sibling heartbeat changed")
		}
		response := claimResponse()
		if len(api.claims) == 3 {
			response.Directives = []Directive{{ID: testDirectiveID, ItemID: testItemID, Fence: original.Fence, Kind: "respawn", Payload: DirectivePayload{LaunchNonce: testRespawnNonce}, Links: testLinks("https://capstan.example")}}
		}
		return response, nil
	}
	var logs []string
	runner := NewRunner(c, api, NewHybridLauncher(&fakeLauncher{}, NewAmpLauncher(c.AmpPath, commands), c.AmpRoles), staticHealth{}, func(line string) { logs = append(logs, line) })
	for tick := 1; tick <= 3; tick++ {
		if err := runner.Run(context.Background()); err != nil {
			t.Fatalf("tick %d aborted before heartbeat: %v", tick, err)
		}
		if len(api.claims) != tick || len(api.fetches) != tick {
			t.Fatal("ambiguous holding suppressed sibling recovery or Claim")
		}
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != markerContents {
			t.Fatal("ambiguous start marker changed")
		}
		if tick < 3 && !reflect.DeepEqual(*loadWorkState(t, c).Items[testItemID], original) {
			t.Fatal("ambiguous holding changed without repair authority")
		}
	}
	if !reflect.DeepEqual(commands.nonces, []string{testNonce, testNonce, testNonce, testRespawnNonce}) {
		t.Fatal("unexpected nonce dispatch path")
	}
	continues, starts := 0, 0
	for _, call := range commands.calls {
		if call.start {
			starts++
			if call.nonce != testRespawnNonce {
				t.Fatal("same-nonce duplicate dispatch")
			}
		}
		if len(call.args) > 2 && call.args[0] == "threads" {
			continues++
			ref, err := ParseAmpSessionRef(siblingRef)
			if err != nil || call.args[2] != ref.ThreadID {
				t.Fatal("sibling steer reached another thread")
			}
		}
	}
	if starts != 1 || continues != 1 {
		t.Fatal("repair was filtered or sibling delivery replayed")
	}
	repaired := loadWorkState(t, c).Items[testItemID]
	ref, err := ParseAmpSessionRef(repaired.SessionRef)
	if err != nil || ref.ThreadID != newThread || repaired.Fence != original.Fence || repaired.Repository != original.Repository || repaired.LaunchNonce != testRespawnNonce || repaired.Directive == nil || repaired.Directive.Status != "succeeded" || !repaired.Directive.ReportPending {
		t.Fatal("rotated-nonce respawn did not reach the existing repair outcome path")
	}
	if len(logs) != 3 {
		t.Fatal("missing per-tick ambiguous holding log")
	}
	for _, line := range logs {
		if !strings.Contains(line, testItemID) || !strings.Contains(line, "fence=1") {
			t.Fatal("ambiguous log omitted identifiers")
		}
		for _, forbidden := range []string{"signature=", testNonce, testRepository, "healthy sibling steer"} {
			if strings.Contains(line, forbidden) {
				t.Fatal("ambiguous log disclosed non-identifier data")
			}
		}
	}
}
