package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSoloCall struct {
	mu           sync.Mutex
	calls        []soloCallRecord
	outputs      []string
	status       soloProcessStatus
	statuses     []soloProcessStatus
	statusErrors []error
	lastInput    string
	sendFailures int
}

type soloCallRecord struct {
	name      string
	arguments map[string]any
}

func (f *fakeSoloCall) Call(_ context.Context, name string, arguments any, target any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	args, _ := arguments.(map[string]any)
	f.calls = append(f.calls, soloCallRecord{name: name, arguments: args})
	if name == "send_input" {
		if f.sendFailures > 0 {
			f.sendFailures--
			return errors.New("test PTY failure")
		}
		if text, ok := args["input"].(string); ok {
			f.lastInput = text
		}
		return nil
	}
	var value any
	switch name {
	case "spawn_agent":
		f.status = soloProcessStatus{ID: 77, Name: args["name"].(string), Kind: "agent", Status: "Running"}
		f.status.AgentState.Idle = true
		if raw, ok := target.(*string); ok {
			*raw = `{"process_id":77}`
			return nil
		}
		value = map[string]any{"process_id": 77}
	case "get_process_status":
		if len(f.statusErrors) > 0 {
			err := f.statusErrors[0]
			f.statusErrors = f.statusErrors[1:]
			return err
		}
		value = f.status
		if len(f.statuses) > 0 {
			value = f.statuses[0]
			f.statuses = f.statuses[1:]
		}
	case "get_process_output":
		output := f.lastInput
		if len(f.outputs) > 0 {
			output = f.outputs[0]
			f.outputs = f.outputs[1:]
		}
		value = output
	case "close_process":
		return nil
	default:
		return errors.New("unexpected call")
	}
	data, _ := json.Marshal(value)
	return json.Unmarshal(data, target)
}

func (f *fakeSoloCall) Close() error { return nil }

func TestSoloLauncherTrustReadyPointerAndCanonicalIdentity(t *testing.T) {
	fake := &fakeSoloCall{outputs: []string{"Quick safety check\n❯ 1. No, exit", "┃ Ask anything, / for commands, @ for context…"}}
	launcher := NewSoloLauncher(fake, time.Second)
	launcher.pollInterval = time.Millisecond
	root := t.TempDir()
	links := filepath.Join(root, linksFilename)
	if err := atomicPrivateWrite(links, mustJSON(t, testLinks("https://server.example"))); err != nil {
		t.Fatal(err)
	}
	spec := LaunchSpec{Role: "builder", SpokeID: 42, LaunchNonce: testNonce, SoloBinding: SoloBinding{ProjectID: 8, AgentToolID: 9, Harness: "opencode", ExtraArgs: []string{"--model", "gpt"}}}
	ref, err := launcher.Launch(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseSoloSessionRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	want := SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "opencode", AgentToolID: 9}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("session ref = %#v", decoded)
	}
	if err := launcher.OpenGate(context.Background(), ref, filepath.Join(root, gateFilename), testNonce); err != nil {
		t.Fatal(err)
	}

	var sends []map[string]any
	for _, call := range fake.calls {
		if call.name == "send_input" {
			sends = append(sends, call.arguments)
		}
	}
	if len(sends) != 4 {
		t.Fatalf("send sequence = %#v", sends)
	}
	if !reflect.DeepEqual(sends[0]["bytes"], []int{27, 91, 66}) || !reflect.DeepEqual(sends[1]["bytes"], []int{13}) {
		t.Fatalf("trust sequence = %#v", sends[:2])
	}
	pointer, _ := sends[2]["input"].(string)
	if pointer != "Read and follow "+filepath.Join(root, promptFilename)+", nonce "+testNonce+"." || strings.Contains(pointer, "https://") || strings.Contains(pointer, "signature") {
		t.Fatal("pointer did not preserve the local prompt path and nonce")
	}
	if !reflect.DeepEqual(sends[3]["bytes"], []int{13}) {
		t.Fatalf("Enter was not separate: %#v", sends[3])
	}
	if submit, ok := sends[2]["submit"].(bool); !ok || submit {
		t.Fatalf("pointer text call submit = %#v", sends[2]["submit"])
	}
	for _, send := range sends {
		if reflect.DeepEqual(send["bytes"], []int{9}) {
			t.Fatal("launcher sent Tab")
		}
	}
	marker := filepath.Join(root, gateFilename) + "." + testNonce
	if data, err := os.ReadFile(marker); err != nil || string(data) != ref+"\n" {
		t.Fatalf("delivery marker = %q, %v", data, err)
	}
	assertMode(t, marker, 0o600)
	calls := len(fake.calls)
	if err := launcher.OpenGate(context.Background(), ref, filepath.Join(root, gateFilename), testNonce); err != nil || len(fake.calls) != calls {
		t.Fatal("delivery marker did not prevent duplicate delivery")
	}
}

func TestSoloLauncherIdleWakeAndDirectiveActions(t *testing.T) {
	ref, err := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 9}).String()
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSoloCall{status: soloProcessStatus{ID: 77, Name: "capstan-orchestrator-" + testNonce, Kind: "agent", Status: "Running"}}
	fake.status.AgentState.Idle = true
	launcher := NewSoloLauncher(fake, time.Second)
	launcher.pollInterval = time.Millisecond
	spec := LaunchSpec{Role: "orchestrator", SpokeID: 42, LaunchNonce: testRespawnNonce, ExistingSessionRef: ref, SoloBinding: SoloBinding{ProjectID: 8, AgentToolID: 9, Harness: "claude", ExtraArgs: []string{}}}
	if got, err := launcher.Launch(context.Background(), spec); err != nil || got != ref {
		t.Fatalf("wake = %q, %v", got, err)
	}
	for _, call := range fake.calls {
		if call.name == "spawn_agent" {
			t.Fatal("idle reusable seat spawned a replacement")
		}
	}

	links := filepath.Join(t.TempDir(), linksFilename)
	if err := atomicPrivateWrite(links, mustJSON(t, testLinks("https://server.example"))); err != nil {
		t.Fatal(err)
	}
	pointer, _ := pointerFromLinks(links, testRespawnNonce)
	fake.outputs = []string{"prompt: " + pointer}
	if err := launcher.SendEnter(context.Background(), ref, links, testRespawnNonce, testDirectiveID); err != nil {
		t.Fatal(err)
	}
	fake.outputs = []string{pointer}
	if err := launcher.ResendPointer(context.Background(), ref, links, testRespawnNonce, testDirectiveID); err != nil {
		t.Fatal(err)
	}
	var bytesSent [][]int
	for _, call := range fake.calls {
		if value, ok := call.arguments["bytes"].([]int); ok {
			bytesSent = append(bytesSent, value)
		}
	}
	wantBytes := [][]int{{13}, {27}, {13}}
	if !reflect.DeepEqual(bytesSent, wantBytes) {
		t.Fatalf("directive bytes = %#v", bytesSent)
	}
	for _, call := range fake.calls {
		if call.name == "send_input" {
			if _, isText := call.arguments["input"]; isText {
				if submit, ok := call.arguments["submit"].(bool); !ok || submit {
					t.Fatalf("pointer text call = %#v", call.arguments)
				}
			}
		}
	}
}

func TestSoloLauncherColdSpawnsOnlyWhenMatchingReusableProcessIsGone(t *testing.T) {
	oldRef, _ := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 66, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 9}).String()
	spec := LaunchSpec{Role: "orchestrator", SpokeID: 42, LaunchNonce: testRespawnNonce, ExistingSessionRef: oldRef, SoloBinding: SoloBinding{ProjectID: 8, AgentToolID: 9, Harness: "claude", ExtraArgs: []string{}}}
	ready := "────────────────────────────────────────────────────────────────\n❯\n────────────────────────────────────────────────────────────────\n  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agent"

	fake := &fakeSoloCall{statusErrors: []error{soloRPCError{code: -32602}}, outputs: []string{ready}}
	launcher := NewSoloLauncher(fake, time.Second)
	launcher.pollInterval = time.Millisecond
	ref, err := launcher.Launch(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseSoloSessionRef(ref)
	if err != nil || decoded.SoloProcessID != 77 || decoded.LaunchNonce != testRespawnNonce {
		t.Fatalf("cold ref = %#v, %v", decoded, err)
	}
	firstSoloCall(t, fake.calls, "spawn_agent")

	fake = &fakeSoloCall{statusErrors: []error{errors.New("transport failure")}}
	launcher = NewSoloLauncher(fake, time.Second)
	if _, err := launcher.Launch(context.Background(), spec); err == nil {
		t.Fatal("non-absence status failure cold-spawned")
	}
	for _, call := range fake.calls {
		if call.name == "spawn_agent" {
			t.Fatal("non-absence status failure reached spawn")
		}
	}
}

func TestSoloLauncherEchoAcceptsRenderedSoftWrapOnly(t *testing.T) {
	pointer := "Read and follow /private/work/item/prompt.md, nonce " + testNonce + "."
	if !renderedContains(strings.Replace(pointer, ", nonce", ",\n  nonce", 1), pointer) {
		t.Fatal("rendered soft wrap hid the complete pointer")
	}
	if !renderedContains(strings.Replace(pointer, "prompt.md", "pro\n  mpt.md", 1), pointer) {
		t.Fatal("rendered in-token soft wrap hid the complete pointer")
	}
	if renderedContains(strings.Replace(pointer, ", nonce", ", unrelated nonce", 1), pointer) {
		t.Fatal("interposed output was accepted as the complete pointer")
	}
}

func TestSoloLauncherSelectsCompleteReadyMarkerByHarness(t *testing.T) {
	claudeMarker := "────────────────────────────────────────────────────────────────\n❯\n────────────────────────────────────────────────────────────────\n  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agent"
	markers := map[string]string{
		"claude":   claudeMarker,
		"codex":    "› Ask Codex to do anything",
		"opencode": "┃ Ask anything, / for commands, @ for context…",
	}
	for harness, marker := range markers {
		t.Run(harness+" accepts its complete marker", func(t *testing.T) {
			ref := SoloSessionRef{SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: harness}
			status := soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}
			status.AgentState.Idle = true
			fake := &fakeSoloCall{outputs: []string{marker}, status: status}
			launcher := NewSoloLauncher(fake, 20*time.Millisecond)
			launcher.pollInterval = time.Millisecond
			if err := launcher.waitReady(context.Background(), ref, false); err != nil {
				t.Fatal(err)
			}
		})
		for label, output := range map[string]string{
			"claude marker": markers["claude"], "codex marker": markers["codex"], "opencode marker": markers["opencode"],
			"bare Claude glyph": "❯", "bare Codex glyph": "›", "bare ASCII glyph": ">", "blank working line": "working\n┃",
			"incomplete Claude prompt": "❯\n────────────────\n  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			"startup fragment":         "loading: bypass permissions on (shift+tab to cycle)",
		} {
			if output == marker && strings.HasSuffix(label, "marker") {
				continue
			}
			t.Run(harness+" rejects "+label, func(t *testing.T) {
				ref := SoloSessionRef{SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: harness}
				status := soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}
				status.AgentState.Idle = true
				fake := &fakeSoloCall{outputs: []string{output}, status: status}
				launcher := NewSoloLauncher(fake, 5*time.Millisecond)
				launcher.pollInterval = time.Millisecond
				if err := launcher.waitReady(context.Background(), ref, false); err == nil {
					t.Fatalf("%s output was treated as %s readiness", label, harness)
				}
			})
		}
	}

	t.Run("busy Claude prompt is rejected even with complete text", func(t *testing.T) {
		busyMarker := claudeMarker + " · esc to interrupt"
		if !readyForHarness("claude", busyMarker) {
			t.Fatal("busy fixture does not exercise the idle-status gate")
		}
		ref := SoloSessionRef{SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "claude"}
		status := soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}
		fake := &fakeSoloCall{outputs: []string{busyMarker}, status: status}
		launcher := NewSoloLauncher(fake, 5*time.Millisecond)
		launcher.pollInterval = time.Millisecond
		if err := launcher.waitReady(context.Background(), ref, false); err == nil {
			t.Fatal("busy Claude process was treated as ready")
		}
	})
}

func TestSoloReadinessWidthSafeSignatures(t *testing.T) {
	claudeFrame := "────────────────────────────────────────────────\n❯\n────────────────────────────────────────────────\n"
	for _, signature := range []struct {
		name    string
		harness string
		marker  *regexp.Regexp
		frames  []string
	}{
		{
			name: "claude", harness: "claude", marker: claudeReadyMarker,
			frames: []string{
				claudeFrame + "⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agent",
				// Observed footer from Solo processes 5196/5197, including the double space and ellipsis.
				claudeFrame + "⏵⏵ bypass permissions on (shift+tab to  · …",
				claudeFrame + "⏵⏵ bypass permissions on",
			},
		},
		{
			name: "codex", harness: "codex", marker: codexReadyMarker,
			frames: []string{"› Ask Codex to do anything", "› Ask Codex to do…", "› Ask Codex"},
		},
		{
			name: "opencode-v1", harness: "opencode", marker: openCodeReadyMarker,
			frames: []string{"┃ Ask anything, / for commands, @ for context…", "┃ Ask anything, / for…", "┃ Ask anything,"},
		},
		{
			name: "opencode-v2", harness: "opencode", marker: openCodeReadyV2Marker,
			frames: []string{
				observedOpenCodeFrame("Fix broken tests"),
				"┃\n┃ Ask anything… \"Fix bro…\n┃\n┃ Build · Fixture…\n╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\nshift+tab agents ctrl+p…\n",
				"┃\n┃ Ask anything…\n┃\n┃ Build\n╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀",
			},
		},
	} {
		t.Run(signature.name, func(t *testing.T) {
			for i, frame := range signature.frames {
				for _, crlf := range []bool{false, true} {
					output := frame
					if crlf {
						output = strings.ReplaceAll(output, "\n", "\r\n") + "\r\n"
					}
					if !signature.marker.MatchString(output) {
						t.Errorf("frame %d (CRLF %v) did not match its signature", i, crlf)
						continue
					}
					status := soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}
					status.AgentState.Idle = true
					fake := &fakeSoloCall{outputs: []string{output}, status: status}
					launcher := NewSoloLauncher(fake, 20*time.Millisecond)
					launcher.pollInterval = time.Millisecond
					ref := SoloSessionRef{SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: signature.harness}
					if err := launcher.waitReady(context.Background(), ref, false); err != nil {
						t.Fatalf("frame %d (CRLF %v): %v", i, crlf, err)
					}
				}
			}
		})
	}

	for _, footer := range []string{
		"⏵⏵ (shift+tab to cycle)", "⏵⏵ bypass permissions…", "⏵⏵ bypass permissions only",
		"⏵⏵ bypass permissions oncall", "bypass permissions on", "",
	} {
		if readyForHarness("claude", claudeFrame+footer) {
			t.Errorf("Claude accepted missing or inexact safety token: %q", footer)
		}
	}
	for harness, fragment := range map[string]string{
		"codex": "› Ask Code", "opencode": "┃ Ask anything",
	} {
		if readyForHarness(harness, fragment) {
			t.Errorf("%s accepted an ambiguous prefix", harness)
		}
	}
}

func TestSoloLauncherFencesEveryReadAndInputAfterProcessIDRecycle(t *testing.T) {
	refValue, _ := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 9}).String()
	valid := soloProcessStatus{ID: 77, Name: "capstan-orchestrator-" + testNonce, Kind: "agent", Status: "Running"}
	valid.AgentState.Idle = true
	replacement := soloProcessStatus{ID: 77, Name: "unrelated-agent", Kind: "agent", Status: "Running"}
	replacement.AgentState.Idle = true
	root := t.TempDir()
	links := filepath.Join(root, linksFilename)
	if err := atomicPrivateWrite(links, mustJSON(t, testLinks("https://server.example"))); err != nil {
		t.Fatal(err)
	}
	actions := map[string]func(*SoloLauncher) error{
		"initial delivery": func(launcher *SoloLauncher) error {
			return launcher.OpenGate(context.Background(), refValue, filepath.Join(root, gateFilename), testRespawnNonce)
		},
		"send enter": func(launcher *SoloLauncher) error {
			return launcher.SendEnter(context.Background(), refValue, links, testRespawnNonce, testDirectiveID)
		},
		"resend pointer": func(launcher *SoloLauncher) error {
			return launcher.ResendPointer(context.Background(), refValue, links, testRespawnNonce, testDirectiveID)
		},
	}
	for name, action := range actions {
		t.Run(name, func(t *testing.T) {
			fake := &fakeSoloCall{statuses: []soloProcessStatus{valid, replacement}, outputs: []string{"must not be read"}}
			launcher := NewSoloLauncher(fake, time.Second)
			if alive, err := launcher.Alive(context.Background(), refValue); err != nil || !alive {
				t.Fatalf("preliminary liveness = %v, %v", alive, err)
			}
			if err := action(launcher); err == nil {
				t.Fatal("action reached a recycled process")
			}
			for _, call := range fake.calls {
				if call.name == "get_process_output" || call.name == "send_input" || call.name == "close_process" {
					t.Fatalf("operation reached replacement: %#v", call)
				}
			}
		})
	}
}

func TestSoloLauncherFencesReusedProcessIDByName(t *testing.T) {
	ref, err := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 9}).String()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"capstan-builder-019a0000-0000-7000-8000-000000000099",
		"unrelated-builder-" + testNonce,
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeSoloCall{status: soloProcessStatus{ID: 77, Name: name, Kind: "agent", Status: "Running"}}
			fake.status.AgentState.Idle = true
			launcher := NewSoloLauncher(fake, 5*time.Millisecond)
			launcher.pollInterval = time.Millisecond

			alive, err := launcher.Alive(context.Background(), ref)
			if err != nil || alive {
				t.Fatalf("alive = %v, %v", alive, err)
			}
			spec := LaunchSpec{Role: "orchestrator", SpokeID: 42, LaunchNonce: testNonce, ExistingSessionRef: ref, SoloBinding: SoloBinding{ProjectID: 8, AgentToolID: 9, Harness: "claude"}}
			if _, err := launcher.Launch(context.Background(), spec); err == nil {
				t.Fatal("reused process id was accepted for idle reuse")
			}
			if err := launcher.Terminate(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			for _, call := range fake.calls {
				if call.name == "close_process" {
					t.Fatalf("unrelated process was closed: %#v", call.arguments)
				}
			}
		})
	}
}

func TestSoloLauncherUnhealthyPTYClosesExactProcess(t *testing.T) {
	ref, _ := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "codex", AgentToolID: 9}).String()
	fake := &fakeSoloCall{outputs: []string{"pending"}, status: soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}, sendFailures: 2}
	launcher := NewSoloLauncher(fake, time.Second)
	links := filepath.Join(t.TempDir(), linksFilename)
	if err := atomicPrivateWrite(links, mustJSON(t, testLinks("https://server.example"))); err != nil {
		t.Fatal(err)
	}
	if err := launcher.ResendPointer(context.Background(), ref, links, testNonce, testDirectiveID); err == nil {
		t.Fatal("unhealthy PTY action passed")
	}
	last := fake.calls[len(fake.calls)-1]
	if last.name != "close_process" || last.arguments["process_id"] != int64(77) || last.arguments["project_id"] != int64(8) {
		t.Fatalf("close call = %#v", last)
	}
}

func TestPointerRejectsOversizeWithoutLeakingIt(t *testing.T) {
	path := filepath.Join("/private", strings.Repeat("x", 900), linksFilename)
	_, err := pointerFromLinks(path, testNonce)
	if err == nil || strings.Contains(err.Error(), "server.example") || strings.Contains(err.Error(), "signature") {
		t.Fatalf("error = %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func pointerTestRoot(t *testing.T, total int) string {
	t.Helper()
	root := t.TempDir()
	want := total - len("Read and follow /prompt.md, nonce "+testNonce+".")
	for len(root) < want {
		remaining := want - len(root)
		part := min(100, remaining-1)
		if remaining-part-1 == 1 {
			part--
		}
		root = filepath.Join(root, strings.Repeat("p", part))
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSoloPointerChunkedDelivery(t *testing.T) {
	for _, size := range []int{299, 600, 900} {
		for _, action := range []string{"gate", "resend", "send_enter"} {
			t.Run(fmt.Sprintf("%d/%s", size, action), func(t *testing.T) {
				root := pointerTestRoot(t, size)
				want := "Read and follow " + filepath.Join(root, promptFilename) + ", nonce " + testNonce + "."
				if len(want) != size {
					t.Fatal("fixture length changed")
				}
				path := filepath.Join(root, linksFilename)
				if err := atomicPrivateWrite(path, mustJSON(t, testLinks("https://server.example"))); err != nil {
					t.Fatal(err)
				}
				pointer, err := pointerFromLinks(path, testNonce)
				if err != nil || pointer != want {
					t.Fatalf("pointer did not preserve exact local path and nonce: %v", err)
				}
				ref, _ := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "codex", AgentToolID: 9}).String()
				var wrapped strings.Builder
				for i := 0; i < len(want); i += 80 {
					wrapped.WriteString(want[i:min(i+80, len(want))])
					wrapped.WriteString("\n  ")
				}
				fake := &fakeSoloCall{status: soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}, outputs: []string{wrapped.String()}}
				if action != "send_enter" {
					// A partial chunk echo must not complete delivery; wait for the full wrapped echo.
					fake.outputs = append([]string{want[:min(299, len(want)-1)]}, fake.outputs...)
				}
				launcher := NewSoloLauncher(fake, time.Second)
				launcher.pollInterval = time.Millisecond
				switch action {
				case "gate":
					err = launcher.OpenGate(context.Background(), ref, filepath.Join(root, gateFilename), testNonce)
				case "resend":
					err = launcher.ResendPointer(context.Background(), ref, path, testNonce, testDirectiveID)
				case "send_enter":
					err = launcher.SendEnter(context.Background(), ref, path, testNonce, testDirectiveID)
				}
				if err != nil {
					t.Fatal(err)
				}
				var delivered strings.Builder
				chunks, enters := 0, 0
				for _, call := range fake.calls {
					if call.name != "send_input" {
						continue
					}
					if chunk, ok := call.arguments["input"].(string); ok {
						if len(chunk) > 299 || len(chunk) == 0 || call.arguments["submit"] != false || enters != 0 {
							t.Fatal("invalid PTY chunk or premature submission")
						}
						delivered.WriteString(chunk)
						chunks++
					}
					if reflect.DeepEqual(call.arguments["bytes"], []int{13}) {
						enters++
					}
				}
				if enters != 1 || len(fake.outputs) != 0 {
					t.Fatal("missing separate Enter or full-pointer echo verification")
				}
				if action != "send_enter" && (delivered.String() != want || chunks != (len(want)+298)/299) {
					t.Fatal("pointer clipped, modified, or not chunked at the PTY boundary")
				}
				if action == "gate" {
					if data, err := os.ReadFile(filepath.Join(root, gateFilename) + "." + testNonce); err != nil || string(data) != ref+"\n" {
						t.Fatal("delivery marker missing")
					}
				}
			})
		}
	}
}

func TestSoloPointerTotalBoundAndRejectedSends(t *testing.T) {
	for _, size := range []int{600, 901} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root := pointerTestRoot(t, size)
			if err := atomicPrivateWrite(filepath.Join(root, linksFilename), mustJSON(t, testLinks("https://server.example"))); err != nil {
				t.Fatal(err)
			}
			ref, _ := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "codex", AgentToolID: 9}).String()
			fake := &fakeSoloCall{status: soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}, sendFailures: 2}
			err := NewSoloLauncher(fake, time.Second).OpenGate(context.Background(), ref, filepath.Join(root, gateFilename), testNonce)
			if err == nil {
				t.Fatal("invalid delivery succeeded")
			}
			if size == 901 {
				if err.Error() != "work pointer exceeds the 900-byte limit" || len(fake.calls) != 0 {
					t.Fatalf("over-total-bound pointer reached Solo or leaked inputs: %v", err)
				}
			} else {
				sends := 0
				for _, call := range fake.calls {
					if call.name == "send_input" {
						sends++
						if _, ok := call.arguments["input"]; !ok {
							t.Fatal("Enter sent after rejected text")
						}
					}
				}
				if sends != 2 {
					t.Fatalf("expected two rejected text sends, got %d (delivery error: %v)", sends, err)
				}
				last := fake.calls[len(fake.calls)-1]
				if last.name != "close_process" || last.arguments["process_id"] != int64(77) || last.arguments["project_id"] != int64(8) {
					t.Fatal("two rejected sends did not close exact process")
				}
			}
			if _, err := os.Stat(filepath.Join(root, gateFilename) + "." + testNonce); !os.IsNotExist(err) {
				t.Fatal("failed delivery wrote a marker")
			}
		})
	}
}

func TestSoloPointerRequiresFullEcho(t *testing.T) {
	for _, action := range []string{"gate", "resend", "send_enter"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			links := filepath.Join(root, linksFilename)
			pointer, err := pointerFromLinks(links, testNonce)
			if err != nil {
				t.Fatal(err)
			}
			ref, _ := (SoloSessionRef{SpokeID: 42, SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "codex", AgentToolID: 9}).String()
			partial := pointer[:len(pointer)-1]
			fake := &fakeSoloCall{status: soloProcessStatus{ID: 77, Name: "capstan-builder-" + testNonce, Kind: "agent", Status: "Running"}, outputs: []string{partial}, lastInput: partial}
			launcher := NewSoloLauncher(fake, 5*time.Millisecond)
			launcher.pollInterval = time.Millisecond
			// Keep returning a clipped echo even after the complete text was sent.
			fake.outputs = []string{partial, partial, partial, partial, partial, partial, partial, partial, partial, partial}
			switch action {
			case "gate":
				err = launcher.OpenGate(context.Background(), ref, filepath.Join(root, gateFilename), testNonce)
			case "resend":
				err = launcher.ResendPointer(context.Background(), ref, links, testNonce, testDirectiveID)
			case "send_enter":
				err = launcher.SendEnter(context.Background(), ref, links, testNonce, testDirectiveID)
			}
			if err == nil {
				t.Fatal("clipped pointer echo completed delivery")
			}
			if _, err := os.Stat(filepath.Join(root, gateFilename) + "." + testNonce); !os.IsNotExist(err) {
				t.Fatal("clipped echo wrote a delivery marker")
			}
			if action == "send_enter" {
				for _, call := range fake.calls {
					if call.name == "send_input" {
						t.Fatal("send_enter submitted a clipped pointer")
					}
				}
			}
		})
	}
}
