package work

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxPointerBytes = 299

var (
	claudeReadyMarker   = regexp.MustCompile(`(?m)^[ \t]*❯[ \t]*\r?\n(?:[^\r\n]*\r?\n){0,3}[ \t]*\?[ \t]+for shortcuts[ \t]*$`)
	codexReadyMarker    = regexp.MustCompile(`(?m)^[ \t]*›[ \t]+Ask Codex to do anything[ \t]*$`)
	openCodeReadyMarker = regexp.MustCompile(`(?m)^[ \t]*┃[ \t]+Ask anything, / for commands, @ for context…[ \t]*$`)
)

// SoloSessionRef is the canonical, non-secret identity handed to Capstan.
type SoloSessionRef struct {
	SpokeID       int64  `json:"spoke_id"`
	SoloProjectID int64  `json:"solo_project_id"`
	SoloProcessID int64  `json:"solo_process_id"`
	LaunchNonce   string `json:"launch_nonce"`
	Harness       string `json:"harness"`
	AgentToolID   int64  `json:"agent_tool_id"`
}

func (r SoloSessionRef) String() (string, error) {
	if r.SpokeID < 1 || r.SoloProjectID < 1 || r.SoloProcessID < 1 || r.AgentToolID < 1 || !validUUIDv7(r.LaunchNonce) || !supportedHarness(r.Harness) {
		return "", errors.New("solo session identity is invalid")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return "", errors.New("solo session identity could not be encoded")
	}
	return "solo:v1:" + base64.RawURLEncoding.EncodeToString(data), nil
}

// ParseSoloSessionRef validates one canonical session reference.
func ParseSoloSessionRef(value string) (SoloSessionRef, error) {
	encoded, ok := strings.CutPrefix(value, "solo:v1:")
	if !ok || encoded == "" || len(value) > 2048 {
		return SoloSessionRef{}, errors.New("solo session reference is invalid")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(data) > 1024 || validateUniqueJSON(data) != nil {
		return SoloSessionRef{}, errors.New("solo session reference is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var ref SoloSessionRef
	if decoder.Decode(&ref) != nil || expectJSONEOF(decoder) != nil {
		return SoloSessionRef{}, errors.New("solo session reference is invalid")
	}
	canonical, err := ref.String()
	if err != nil || canonical != value {
		return SoloSessionRef{}, errors.New("solo session reference is invalid")
	}
	return ref, nil
}

type soloProcessStatus struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	AgentState struct {
		Idle bool `json:"idle"`
	} `json:"agent_state"`
}

// SoloLauncher implements Launcher through Solo's stdio MCP.
type SoloLauncher struct {
	client       SoloCaller
	readyTimeout time.Duration
	pollInterval time.Duration
}

// NewSoloLauncher returns a production Solo launcher over an injected MCP caller.
func NewSoloLauncher(client SoloCaller, readyTimeout time.Duration) *SoloLauncher {
	return &SoloLauncher{client: client, readyTimeout: readyTimeout, pollInterval: 200 * time.Millisecond}
}

func (l *SoloLauncher) Launch(ctx context.Context, spec LaunchSpec) (string, error) {
	binding := spec.SoloBinding
	if spec.SpokeID < 1 || spec.Role == "" || !validUUIDv7(spec.LaunchNonce) || binding.ProjectID < 1 || binding.AgentToolID < 1 || !supportedHarness(binding.Harness) {
		return "", errors.New("solo launch specification is invalid")
	}
	if spec.ExistingSessionRef != "" {
		ref, err := ParseSoloSessionRef(spec.ExistingSessionRef)
		if err != nil || ref.SpokeID != spec.SpokeID || ref.SoloProjectID != binding.ProjectID || ref.AgentToolID != binding.AgentToolID ||
			ref.Harness != binding.Harness {
			return "", errors.New("reusable Solo session identity does not match the current claim")
		}
		if err := l.waitIdle(ctx, ref); err != nil {
			return "", err
		}
		return spec.ExistingSessionRef, nil
	}
	var spawnText string
	if err := l.client.Call(ctx, "spawn_agent", map[string]any{
		"agent_tool_id": binding.AgentToolID, "project_id": binding.ProjectID,
		"name":       fmt.Sprintf("capstan-%s-%s", spec.Role, spec.LaunchNonce),
		"extra_args": binding.ExtraArgs, "include_agent_instructions": false,
	}, &spawnText); err != nil {
		return "", errors.New("solo could not spawn the configured agent")
	}
	var spawned struct {
		ProcessID int64 `json:"process_id"`
		ID        int64 `json:"id"`
	}
	if json.Unmarshal([]byte(spawnText), &spawned) != nil {
		match := regexp.MustCompile(`process_id\D+([0-9]+)`).FindStringSubmatch(spawnText)
		if len(match) != 2 {
			return "", errors.New("solo spawn result has an invalid shape")
		}
		spawned.ProcessID, _ = strconv.ParseInt(match[1], 10, 64)
	}
	processID := spawned.ProcessID
	if processID == 0 {
		processID = spawned.ID
	}
	ref := SoloSessionRef{SpokeID: spec.SpokeID, SoloProjectID: binding.ProjectID, SoloProcessID: processID, LaunchNonce: spec.LaunchNonce, Harness: binding.Harness, AgentToolID: binding.AgentToolID}
	encoded, err := ref.String()
	if err != nil {
		return "", err
	}
	if err := l.waitReady(ctx, ref, true); err != nil {
		_ = l.close(ctx, ref)
		return "", err
	}
	return encoded, nil
}

func (l *SoloLauncher) Alive(ctx context.Context, sessionRef string) (bool, error) {
	ref, err := ParseSoloSessionRef(sessionRef)
	if err != nil {
		return false, err
	}
	status, err := l.status(ctx, ref)
	if err != nil {
		return false, err
	}
	return status.Kind == "agent" && status.Status == "Running" && statusMatchesRef(status, ref), nil
}

func (l *SoloLauncher) Terminate(ctx context.Context, sessionRef string) error {
	ref, err := ParseSoloSessionRef(sessionRef)
	if err != nil {
		return err
	}
	return l.close(ctx, ref)
}

func (l *SoloLauncher) OpenGate(ctx context.Context, sessionRef, gatePath, deliveryNonce string) error {
	ref, err := ParseSoloSessionRef(sessionRef)
	if err != nil || !validUUIDv7(deliveryNonce) {
		if err == nil {
			err = errors.New("solo delivery nonce is invalid")
		}
		return err
	}
	marker := gatePath + "." + deliveryNonce
	if data, err := os.ReadFile(marker); err == nil && string(data) == sessionRef+"\n" {
		return nil
	}
	pointer, err := pointerFromLinks(filepath.Join(filepath.Dir(gatePath), linksFilename), deliveryNonce)
	if err != nil {
		return err
	}
	if err := l.sendText(ctx, ref, pointer); err != nil {
		return err
	}
	if err := l.sendBytes(ctx, ref, []int{13}); err != nil {
		return err
	}
	if err := l.waitEcho(ctx, ref, pointer); err != nil {
		return err
	}
	if err := atomicPrivateWrite(marker, []byte(sessionRef+"\n")); err != nil {
		return errors.New("solo delivery marker could not be saved")
	}
	return nil
}

func (l *SoloLauncher) SendEnter(ctx context.Context, sessionRef, linksPath, deliveryNonce, _ string) error {
	ref, err := ParseSoloSessionRef(sessionRef)
	if err != nil || !validUUIDv7(deliveryNonce) {
		if err == nil {
			err = errors.New("solo delivery nonce is invalid")
		}
		return err
	}
	pointer, err := pointerFromLinks(linksPath, deliveryNonce)
	if err != nil {
		return err
	}
	output, err := l.output(ctx, ref)
	if err != nil {
		return err
	}
	if !strings.Contains(output, pointer) {
		return errors.New("solo prompt does not contain the pending pointer")
	}
	return l.sendBytes(ctx, ref, []int{13})
}

func (l *SoloLauncher) ResendPointer(ctx context.Context, sessionRef, linksPath, deliveryNonce, _ string) error {
	ref, err := ParseSoloSessionRef(sessionRef)
	if err != nil || !validUUIDv7(deliveryNonce) {
		if err == nil {
			err = errors.New("solo delivery nonce is invalid")
		}
		return err
	}
	pointer, err := pointerFromLinks(linksPath, deliveryNonce)
	if err != nil {
		return err
	}
	if err := l.sendBytes(ctx, ref, []int{27}); err != nil {
		return err
	}
	if err := l.sendText(ctx, ref, pointer); err != nil {
		return err
	}
	if err := l.sendBytes(ctx, ref, []int{13}); err != nil {
		return err
	}
	return l.waitEcho(ctx, ref, pointer)
}

func (l *SoloLauncher) waitReady(ctx context.Context, ref SoloSessionRef, allowTrust bool) error {
	deadline := time.Now().Add(l.readyTimeout)
	trusted := false
	for time.Now().Before(deadline) {
		output, err := l.output(ctx, ref)
		if err != nil {
			return err
		}
		trust := strings.Contains(strings.ToLower(output), "trust this folder") || strings.Contains(strings.ToLower(output), "quick safety check")
		if trust && allowTrust && !trusted {
			if err := l.sendBytes(ctx, ref, []int{27, 91, 66}); err != nil {
				return err
			}
			if err := l.sendBytes(ctx, ref, []int{13}); err != nil {
				return err
			}
			trusted = true
			time.Sleep(l.pollInterval)
			continue
		}
		if readyForHarness(ref.Harness, output) {
			status, statusErr := l.currentStatus(ctx, ref)
			if statusErr != nil {
				return statusErr
			}
			if status.AgentState.Idle {
				return nil
			}
		}
		time.Sleep(l.pollInterval)
	}
	return errors.New("solo agent did not reach a ready prompt")
}

func (l *SoloLauncher) waitIdle(ctx context.Context, ref SoloSessionRef) error {
	deadline := time.Now().Add(l.readyTimeout)
	for time.Now().Before(deadline) {
		status, err := l.status(ctx, ref)
		if err != nil {
			return err
		}
		if status.Kind != "agent" || status.Status != "Running" || !statusMatchesRef(status, ref) {
			return errors.New("reusable Solo session is not alive")
		}
		if status.AgentState.Idle {
			return nil
		}
		time.Sleep(l.pollInterval)
	}
	return errors.New("reusable Solo session did not become idle")
}

func (l *SoloLauncher) waitEcho(ctx context.Context, ref SoloSessionRef, pointer string) error {
	deadline := time.Now().Add(l.readyTimeout)
	for time.Now().Before(deadline) {
		output, err := l.output(ctx, ref)
		if err != nil {
			return err
		}
		if strings.Contains(output, pointer) {
			return nil
		}
		time.Sleep(l.pollInterval)
	}
	return errors.New("solo did not echo the work pointer")
}

func (l *SoloLauncher) status(ctx context.Context, ref SoloSessionRef) (soloProcessStatus, error) {
	var status soloProcessStatus
	if err := l.client.Call(ctx, "get_process_status", map[string]any{"process_id": ref.SoloProcessID, "project_id": ref.SoloProjectID}, &status); err != nil {
		return soloProcessStatus{}, errors.New("solo process status could not be read")
	}
	return status, nil
}

func (l *SoloLauncher) output(ctx context.Context, ref SoloSessionRef) (string, error) {
	if _, err := l.currentStatus(ctx, ref); err != nil {
		return "", err
	}
	var output string
	if err := l.client.Call(ctx, "get_process_output", map[string]any{"process_id": ref.SoloProcessID, "project_id": ref.SoloProjectID, "lines": 60}, &output); err != nil {
		return "", errors.New("solo process output could not be read")
	}
	return output, nil
}

func (l *SoloLauncher) sendText(ctx context.Context, ref SoloSessionRef, text string) error {
	return l.send(ctx, ref, map[string]any{"process_id": ref.SoloProcessID, "project_id": ref.SoloProjectID, "input": text, "submit": false})
}

func (l *SoloLauncher) sendBytes(ctx context.Context, ref SoloSessionRef, value []int) error {
	return l.send(ctx, ref, map[string]any{"process_id": ref.SoloProcessID, "project_id": ref.SoloProjectID, "bytes": value})
}

func (l *SoloLauncher) send(ctx context.Context, ref SoloSessionRef, arguments map[string]any) error {
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := l.currentStatus(ctx, ref); err != nil {
			return err
		}
		if err := l.client.Call(ctx, "send_input", arguments, nil); err == nil {
			return nil
		}
	}
	_ = l.close(ctx, ref)
	return errors.New("solo PTY rejected repeated input")
}

func (l *SoloLauncher) close(ctx context.Context, ref SoloSessionRef) error {
	status, err := l.status(ctx, ref)
	if err != nil {
		return err
	}
	if !statusMatchesRef(status, ref) {
		return nil
	}
	if err := l.client.Call(ctx, "close_process", map[string]any{"process_id": ref.SoloProcessID, "project_id": ref.SoloProjectID}, nil); err != nil {
		return errors.New("solo process could not be closed")
	}
	return nil
}

func statusMatchesRef(status soloProcessStatus, ref SoloSessionRef) bool {
	return status.ID == ref.SoloProcessID && status.Kind == "agent" && strings.HasPrefix(status.Name, "capstan-") && strings.HasSuffix(status.Name, "-"+ref.LaunchNonce)
}

func (l *SoloLauncher) currentStatus(ctx context.Context, ref SoloSessionRef) (soloProcessStatus, error) {
	status, err := l.status(ctx, ref)
	if err != nil {
		return soloProcessStatus{}, err
	}
	if status.Status != "Running" || !statusMatchesRef(status, ref) {
		return soloProcessStatus{}, errors.New("solo process identity changed")
	}
	return status, nil
}

func readyForHarness(harness, output string) bool {
	switch harness {
	case "claude":
		return claudeReadyMarker.MatchString(output)
	case "codex":
		return codexReadyMarker.MatchString(output)
	case "opencode":
		return openCodeReadyMarker.MatchString(output)
	default:
		return false
	}
}

func pointerFromLinks(path, nonce string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64<<10 || validateUniqueJSON(data) != nil {
		return "", errors.New("private signed-link file could not be read")
	}
	var links map[string]string
	if json.Unmarshal(data, &links) != nil {
		return "", errors.New("private signed-link file is invalid")
	}
	brief := links["brief"]
	if brief == "" || !validUUIDv7(nonce) {
		return "", errors.New("work pointer inputs are invalid")
	}
	pointer := fmt.Sprintf("Fetch %s, nonce %s.", brief, nonce)
	if len([]byte(pointer)) > maxPointerBytes {
		return "", errors.New("work pointer exceeds the 299-byte limit")
	}
	return pointer, nil
}
