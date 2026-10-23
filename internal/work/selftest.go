package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SelfTestEvidence is the exact set of rungs a setup self-test must observe.
type SelfTestEvidence struct {
	Claim     bool `json:"claim"`
	Spawn     bool `json:"spawn"`
	Fetch     bool `json:"fetch"`
	Milestone bool `json:"milestone"`
	Done      bool `json:"done"`
}

// Validate fails closed unless every required rung was observed.
func (e SelfTestEvidence) Validate() error {
	if !e.Claim || !e.Spawn || !e.Fetch || !e.Milestone || !e.Done {
		return errors.New("work self-test did not observe every required rung")
	}
	return nil
}

// SoloSelfTester runs the configured Solo path against a local harmless Work stub.
type SoloSelfTester struct {
	Timeout   time.Duration
	Tick      func(context.Context, string) error
	Terminate func(context.Context, Config, string) error
}

type selfTestStub struct {
	mu       sync.Mutex
	evidence SelfTestEvidence
	claimed  bool
	nonce    string
	base     string
}

// Run observes claim, spawn, nonce fetch, milestone, and done without touching a project checkout.
func (s SoloSelfTester) Run(ctx context.Context, config Config, output io.Writer) (SelfTestEvidence, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nonce, err := newUUIDv7()
	if err != nil {
		return SelfTestEvidence{}, err
	}
	stub := &selfTestStub{nonce: nonce}
	server := httptest.NewServer(http.HandlerFunc(stub.serveHTTP))
	stub.base = server.URL
	defer server.Close()
	configPath, cleanup, err := writeSelfTestConfig(config, server.URL)
	if err != nil {
		return SelfTestEvidence{}, err
	}
	defer func() {
		s.cleanupSession(configPath, nonce)
		cleanup()
	}()
	tick := s.Tick
	if tick == nil {
		tick = func(ctx context.Context, path string) error { return Execute(ctx, path, nil, nil, nil) }
	}
	last := SelfTestEvidence{}
	for {
		if err := tick(testCtx, configPath); err != nil {
			return last, err
		}
		current := stub.snapshot()
		for _, rung := range newlyObserved(last, current) {
			if _, err := fmt.Fprintf(output, "Self-test observed: %s\n", rung); err != nil {
				return current, errors.New("work self-test output could not be written")
			}
		}
		last = current
		if current.Done {
			if err := tick(testCtx, configPath); err != nil {
				return current, err
			}
			state, loadErr := NewStateStore(filepath.Join(filepath.Dir(configPath), "state.json"), filepath.Join(filepath.Dir(configPath), "items")).Load()
			if loadErr != nil || len(state.Items) != 0 {
				return current, errors.New("work self-test local state was not cleaned up")
			}
			return current, current.Validate()
		}
		select {
		case <-testCtx.Done():
			return last, errors.New("work self-test timed out before every rung was observed")
		case <-time.After(time.Second):
		}
	}
}

func (s SoloSelfTester) cleanupSession(configPath, nonce string) {
	config, err := LoadConfig(configPath)
	if err != nil {
		return
	}
	state, err := NewStateStore(config.StateFile, config.WorkDir).Load()
	if err != nil {
		return
	}
	binding := config.SoloBindings["orchestrator"]
	for _, item := range state.Items {
		if item == nil || item.SessionRef == "" {
			continue
		}
		ref, err := ParseSoloSessionRef(item.SessionRef)
		if err != nil || ref.LaunchNonce != nonce || ref.SpokeID != item.SpokeID || ref.SoloProjectID != binding.ProjectID || ref.AgentToolID != binding.AgentToolID || ref.Harness != binding.Harness {
			continue
		}
		timeout := config.SoloCallTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if s.Terminate != nil {
			_ = s.Terminate(ctx, config, item.SessionRef)
			cancel()
			continue
		}
		client, clientErr := NewStdioSoloClient(ctx, config.SoloMCPPath, timeout)
		if clientErr == nil {
			_ = NewSoloLauncher(client, config.SoloReadyTimeout).Terminate(ctx, item.SessionRef)
			_ = client.Close()
		}
		cancel()
	}
}

func (s *selfTestStub) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/api/") && (request.Header.Get("Authorization") != "Bearer setup-self-test" || request.Header.Get("X-Capstan-Actor-ID") != "1") {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch request.URL.Path {
	case "/api/v1/work/self-test/claim":
		s.claim(writer)
	case "/api/v1/work/self-test/renew":
		s.renew(writer)
	case "/api/v1/work/self-test/transition":
		s.transition(writer, request)
	case "/brief":
		s.brief(writer, request)
	case "/milestone":
		s.milestone(writer, request)
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

func (s *selfTestStub) claim(writer http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []ClaimItem{}
	if !s.claimed {
		s.claimed = true
		s.evidence.Claim = true
		items = append(items, ClaimItem{ID: testSelfItemID, TaskID: testSelfTaskID, Role: "orchestrator", Fence: 1, LaunchNonce: s.nonce, Links: s.links()})
	}
	spoke := int64(1)
	writeSelfTestJSON(writer, ClaimResponse{Items: items, Directives: []Directive{}, SpokeID: &spoke})
}

func (s *selfTestStub) renew(writer http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evidence.Done {
		writeSelfTestJSON(writer, map[string]any{"results": []RenewResult{{ItemID: testSelfItemID, Status: "lost"}}})
		return
	}
	writeSelfTestJSON(writer, map[string]any{"results": []RenewResult{{ItemID: testSelfItemID, Status: "renewed", Item: &RenewedItem{ID: testSelfItemID, TaskID: testSelfTaskID, Role: "orchestrator", State: "routed", Fence: 1, Links: s.links()}}}})
}

func (s *selfTestStub) transition(writer http.ResponseWriter, request *http.Request) {
	var transition TransitionRequest
	if json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&transition) != nil || transition.ItemID != testSelfItemID || transition.Fence != 1 {
		writer.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if transition.State == "delivered" {
		if transition.LaunchNonce != s.nonce || transition.SessionRef == "" {
			writer.WriteHeader(http.StatusConflict)
			return
		}
		s.evidence.Spawn = true
	} else if transition.State != "routed" && transition.State != "released" {
		writer.WriteHeader(http.StatusConflict)
		return
	}
	writeSelfTestJSON(writer, map[string]any{"item": map[string]any{"id": testSelfItemID, "task_id": testSelfTaskID, "role": "orchestrator", "state": transition.State, "fence": 1}})
}

func (s *selfTestStub) brief(writer http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.URL.Query().Get("nonce") != s.nonce {
		writer.WriteHeader(http.StatusConflict)
		return
	}
	s.evidence.Fetch = true
	writer.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = fmt.Fprintf(writer, "# Capstan setup self-test\n\nDo not edit any files. Run these two commands in order, then stop:\n\n```sh\ncurl -fsS -X POST -H 'Content-Type: application/json' --data '{\"kind\":\"progress\"}' '%s/milestone?nonce=%s'\ncurl -fsS -X POST -H 'Content-Type: application/json' --data '{\"kind\":\"done\"}' '%s/milestone?nonce=%s'\n```\n", s.base, s.nonce, s.base, s.nonce)
}

func (s *selfTestStub) milestone(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 64<<10))
	if err != nil || request.URL.Query().Get("nonce") != s.nonce {
		writer.WriteHeader(http.StatusConflict)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Contains(string(body), `"kind":"progress"`) {
		s.evidence.Milestone = true
	} else if strings.Contains(string(body), `"kind":"done"`) && s.evidence.Milestone {
		s.evidence.Done = true
	} else {
		writer.WriteHeader(http.StatusConflict)
		return
	}
	writeSelfTestJSON(writer, map[string]any{"ok": true})
}

func (s *selfTestStub) links() map[string]string {
	suffix := "?nonce=" + s.nonce + "&signature=self-test"
	return map[string]string{
		"brief": s.base + "/brief" + suffix, "milestone": s.base + "/milestone" + suffix,
		"steers": s.base + "/steers" + suffix, "evidence": s.base + "/evidence" + suffix,
		"orchestration": s.base + "/orchestration" + suffix, "orchestrate": s.base + "/orchestrate" + suffix,
	}
}

func (s *selfTestStub) snapshot() SelfTestEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evidence
}

func writeSelfTestConfig(config Config, serverURL string) (string, func(), error) {
	root, err := os.MkdirTemp("", "capstan-work-self-test-*")
	if err != nil {
		return "", nil, errors.New("work self-test directory could not be created")
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	credential := filepath.Join(root, "credential")
	if err := atomicPrivateWrite(credential, []byte("setup-self-test\n")); err != nil {
		cleanup()
		return "", nil, errors.New("work self-test credential could not be written")
	}
	maxConcurrent, orchestratorCapacity := 0, 1
	raw := rawConfig{
		Version: 2, Launcher: "solo", CapstanURL: serverURL, Team: "self-test", ActorID: "1", CredentialFile: credential,
		MaxConcurrent: &maxConcurrent, OrchestratorCapacity: &orchestratorCapacity, MaxLoad1m: config.MaxLoad1m,
		MaxMemoryPercent: config.MaxMemoryPercent, Capabilities: []string{}, Roles: []string{"orchestrator"},
		RoleProviders: map[string]string{"orchestrator": config.RoleProviders["orchestrator"]},
		StateFile:     filepath.Join(root, "state.json"), WorkDir: filepath.Join(root, "items"), LockFile: filepath.Join(root, "work.lock"),
		PromptTemplate: config.OrchestratorPromptTemplate, OrchestratorPromptTemplate: config.OrchestratorPromptTemplate,
		SoloMCPPath: config.SoloMCPPath, SoloCallTimeoutSeconds: config.SoloCallTimeout.Seconds(), SoloReadyTimeoutSeconds: config.SoloReadyTimeout.Seconds(),
		SoloBindings: map[string]SoloBinding{"orchestrator": config.SoloBindings["orchestrator"]}, RequestTimeoutSeconds: config.RequestTimeout.Seconds(),
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		cleanup()
		return "", nil, errors.New("work self-test config could not be encoded")
	}
	path := filepath.Join(root, "work.json")
	if err := atomicPrivateWrite(path, append(data, '\n')); err != nil {
		cleanup()
		return "", nil, errors.New("work self-test config could not be written")
	}
	return path, cleanup, nil
}

func writeSelfTestJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func newlyObserved(before, after SelfTestEvidence) []string {
	var rungs []string
	if !before.Claim && after.Claim {
		rungs = append(rungs, "claim")
	}
	if !before.Spawn && after.Spawn {
		rungs = append(rungs, "spawn")
	}
	if !before.Fetch && after.Fetch {
		rungs = append(rungs, "brief fetch")
	}
	if !before.Milestone && after.Milestone {
		rungs = append(rungs, "milestone")
	}
	if !before.Done && after.Done {
		rungs = append(rungs, "done")
	}
	return rungs
}

const (
	testSelfItemID = "018f0000-0000-7000-8000-0000000000a1"
	testSelfTaskID = "018f0000-0000-7000-8000-0000000000b1"
)
