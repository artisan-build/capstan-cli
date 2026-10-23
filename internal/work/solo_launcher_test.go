package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
		if raw, ok := target.(*string); ok {
			*raw = `{"process_id":77}`
			return nil
		}
		value = map[string]any{"process_id": 77}
	case "get_process_status":
		value = f.status
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
	fake := &fakeSoloCall{outputs: []string{"Quick safety check\n❯ 1. No, exit", "\n❯\n"}}
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
	if err := launcher.OpenGate(context.Background(), ref, filepath.Join(root, gateFilename)); err != nil {
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
	if len(pointer) > maxPointerBytes || !strings.Contains(pointer, "/brief?signature=brief") || !strings.Contains(pointer, testNonce) {
		t.Fatalf("pointer = %q", pointer)
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
	spec := LaunchSpec{Role: "orchestrator", SpokeID: 42, LaunchNonce: testNonce, ExistingSessionRef: ref, SoloBinding: SoloBinding{ProjectID: 8, AgentToolID: 9, Harness: "claude", ExtraArgs: []string{}}}
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
	pointer, _ := pointerFromLinks(links, testNonce)
	fake.outputs = []string{"prompt: " + pointer}
	if err := launcher.SendEnter(context.Background(), ref, links, testDirectiveID); err != nil {
		t.Fatal(err)
	}
	fake.outputs = []string{pointer}
	if err := launcher.ResendPointer(context.Background(), ref, links, testDirectiveID); err != nil {
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

func TestSoloLauncherRecognizesOnlyCompleteOpenCodeReadyMarker(t *testing.T) {
	ref := SoloSessionRef{SoloProjectID: 8, SoloProcessID: 77}

	t.Run("real marker", func(t *testing.T) {
		fake := &fakeSoloCall{outputs: []string{"┃ Ask anything, / for commands, @ for context…"}}
		launcher := NewSoloLauncher(fake, 20*time.Millisecond)
		launcher.pollInterval = time.Millisecond
		if err := launcher.waitReady(context.Background(), ref, false); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("blank working line", func(t *testing.T) {
		fake := &fakeSoloCall{outputs: []string{"working\n┃"}}
		launcher := NewSoloLauncher(fake, 5*time.Millisecond)
		launcher.pollInterval = time.Millisecond
		if err := launcher.waitReady(context.Background(), ref, false); err == nil {
			t.Fatal("blank OpenCode line was treated as ready")
		}
	})
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
	if err := launcher.ResendPointer(context.Background(), ref, links, testDirectiveID); err == nil {
		t.Fatal("unhealthy PTY action passed")
	}
	last := fake.calls[len(fake.calls)-1]
	if last.name != "close_process" || last.arguments["process_id"] != int64(77) || last.arguments["project_id"] != int64(8) {
		t.Fatalf("close call = %#v", last)
	}
}

func TestPointerRejectsOversizeWithoutLeakingIt(t *testing.T) {
	links := testLinks("https://server.example/" + strings.Repeat("x", 300))
	path := filepath.Join(t.TempDir(), linksFilename)
	if err := atomicPrivateWrite(path, mustJSON(t, links)); err != nil {
		t.Fatal(err)
	}
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
