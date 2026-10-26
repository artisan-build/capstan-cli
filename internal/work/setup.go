package work

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SoloCaller is the injected Solo MCP boundary used by setup and the launcher.
type SoloCaller interface {
	Call(context.Context, string, any, any) error
	Close() error
}

// SoloProject is one installed Solo project shown during setup.
type SoloProject struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// SoloAgentTool is one installed Solo agent tool shown during setup.
type SoloAgentTool struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ToolType string `json:"tool_type"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// SetupOptions contains scriptable setup inputs. Missing fields are prompted unless NonInteractive is true.
type SetupOptions struct {
	ConfigPath           string
	CapstanURL           string
	Team                 string
	ActorID              string
	CredentialFile       string
	SoloMCPPath          string
	AmpPath              string
	AmpRoles             []string
	Bindings             map[string]SoloBinding
	MaxConcurrent        int
	OrchestratorCapacity int
	Schedule             string
	Executable           string
	Platform             string
	NonInteractive       bool
	SelfTestTimeout      time.Duration
}

// RunSetup wires production Solo discovery and the live fail-closed self-test.
func RunSetup(ctx context.Context, options SetupOptions, input io.Reader, output io.Writer) (returnErr error) {
	if options.Platform == "" {
		options.Platform = runtime.GOOS
	}
	if options.SoloMCPPath == "" {
		options.SoloMCPPath = defaultSoloMCPPath(options.Platform)
	}
	client, err := NewStdioSoloClient(ctx, options.SoloMCPPath, 30*time.Second)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, client.Close()) }()
	tester := SoloSelfTester{Timeout: options.SelfTestTimeout}
	return Setup(ctx, options, input, output, client, tester)
}

// SelfTestRunner proves the configured server and launcher path after setup.
type SelfTestRunner interface {
	Run(context.Context, Config, io.Writer) (SelfTestEvidence, error)
}

var setupInstallWorkSchedule = InstallWorkSchedule

// Setup discovers immutable Solo ids, writes a private config, emits the schedule, and runs the self-test.
func Setup(ctx context.Context, options SetupOptions, input io.Reader, output io.Writer, solo SoloCaller, tester SelfTestRunner) (returnErr error) {
	if solo == nil || tester == nil {
		return errors.New("work setup requires Solo discovery and self-test drivers")
	}
	projects, tools, err := DiscoverSolo(ctx, solo)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "Solo projects:"); err != nil {
		return errors.New("work setup output could not be written")
	}
	for _, project := range projects {
		if _, err := fmt.Fprintf(output, "  %d\t%s\n", project.ID, project.Name); err != nil {
			return errors.New("work setup output could not be written")
		}
	}
	if _, err := fmt.Fprintln(output, "Solo agent tools:"); err != nil {
		return errors.New("work setup output could not be written")
	}
	for _, tool := range tools {
		if _, err := fmt.Fprintf(output, "  %d\t%s\t%s\n", tool.ID, tool.Name, tool.ToolType); err != nil {
			return errors.New("work setup output could not be written")
		}
	}
	if err := completeSetupOptions(&options, input, output, projects, tools); err != nil {
		return err
	}
	plan, err := stageSetupConfig(options)
	if err != nil {
		return err
	}
	defer plan.cleanup()
	schedule, err := RenderWorkSchedule(options.Platform, options.Executable, plan.configPath)
	if err != nil {
		return err
	}
	release, err := lockSetupReplacement(plan)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, release()) }()
	evidence, err := tester.Run(ctx, plan.stagedConfig, output)
	if err != nil {
		return err
	}
	if err := evidence.Validate(); err != nil {
		return err
	}
	if err := commitSetupConfig(plan); err != nil {
		return err
	}
	if options.Schedule == "install" {
		if err := setupInstallWorkSchedule(options.Platform, schedule); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(output, "Installed the Capstan Work one-shot schedule."); err != nil {
			return errors.New("work setup output could not be written")
		}
	} else if _, err := fmt.Fprint(output, schedule.Text); err != nil {
		return errors.New("work setup output could not be written")
	}
	if _, err := fmt.Fprintln(output, "Self-test passed: claim, spawn, brief fetch, milestone, and done were observed."); err != nil {
		return errors.New("work setup output could not be written")
	}
	return nil
}

// DiscoverSolo lists projects and tools without resolving either id from a display name.
func DiscoverSolo(ctx context.Context, solo SoloCaller) ([]SoloProject, []SoloAgentTool, error) {
	var projectRaw json.RawMessage
	if err := solo.Call(ctx, "list_projects", map[string]any{}, &projectRaw); err != nil {
		return nil, nil, errors.New("solo projects could not be listed")
	}
	var toolRaw json.RawMessage
	if err := solo.Call(ctx, "list_agent_tools", map[string]any{}, &toolRaw); err != nil {
		return nil, nil, errors.New("solo agent tools could not be listed")
	}
	projects, err := decodeProjects(projectRaw)
	if err != nil {
		return nil, nil, err
	}
	tools, err := decodeTools(toolRaw)
	if err != nil {
		return nil, nil, err
	}
	return projects, tools, nil
}

func decodeProjects(data []byte) ([]SoloProject, error) {
	var projects []SoloProject
	if json.Unmarshal(data, &projects) != nil {
		var envelope struct {
			Projects []SoloProject `json:"projects"`
		}
		if json.Unmarshal(data, &envelope) != nil || envelope.Projects == nil {
			return nil, errors.New("solo project list has an invalid shape")
		}
		projects = envelope.Projects
	}
	if projects == nil {
		return nil, errors.New("solo project list has an invalid shape")
	}
	for _, project := range projects {
		if project.ID < 1 || project.Name == "" {
			return nil, errors.New("solo project list contains an invalid project")
		}
	}
	return projects, nil
}

func decodeTools(data []byte) ([]SoloAgentTool, error) {
	var tools []SoloAgentTool
	if json.Unmarshal(data, &tools) != nil {
		var envelope struct {
			AgentTools []SoloAgentTool `json:"agent_tools"`
			Tools      []SoloAgentTool `json:"tools"`
		}
		if json.Unmarshal(data, &envelope) != nil {
			return nil, errors.New("solo agent-tool list has an invalid shape")
		}
		tools = envelope.AgentTools
		if tools == nil {
			tools = envelope.Tools
		}
	}
	if tools == nil {
		return nil, errors.New("solo agent-tool list has an invalid shape")
	}
	for _, tool := range tools {
		if tool.ID < 1 || tool.Name == "" {
			return nil, errors.New("solo agent-tool list contains an invalid tool")
		}
	}
	return tools, nil
}

func completeSetupOptions(options *SetupOptions, input io.Reader, output io.Writer, projects []SoloProject, tools []SoloAgentTool) error {
	if len(options.AmpRoles) == 0 && options.AmpPath != "" {
		return errors.New("--amp-path requires an explicit --amp-role")
	}
	if len(options.AmpRoles) > 0 && options.AmpPath == "" {
		path, err := exec.LookPath("amp")
		if err != nil {
			return errors.New("amp executable was not found; supply --amp-path")
		}
		options.AmpPath, err = filepath.Abs(path)
		if err != nil {
			return errors.New("amp executable path could not be resolved")
		}
	}
	if options.ConfigPath == "" {
		path, err := DefaultConfigPath()
		if err != nil {
			return err
		}
		options.ConfigPath = path
	}
	if options.SoloMCPPath == "" {
		options.SoloMCPPath = defaultSoloMCPPath(options.Platform)
	}
	if options.MaxConcurrent == 0 {
		options.MaxConcurrent = 2
	}
	if options.OrchestratorCapacity == 0 {
		options.OrchestratorCapacity = 1
	}
	if options.Schedule == "" {
		options.Schedule = "print"
	}
	if options.SelfTestTimeout == 0 {
		options.SelfTestTimeout = 5 * time.Minute
	}
	if options.NonInteractive {
		if options.CapstanURL == "" || options.Team == "" || options.ActorID == "" || options.CredentialFile == "" || len(options.Bindings) == 0 {
			return errors.New("noninteractive work setup is missing required inputs")
		}
	} else {
		scanner := bufio.NewScanner(input)
		var err error
		options.CapstanURL, err = promptValue(scanner, output, "Capstan URL", options.CapstanURL)
		if err != nil {
			return err
		}
		options.Team, err = promptValue(scanner, output, "Team id or key", options.Team)
		if err != nil {
			return err
		}
		options.ActorID, err = promptValue(scanner, output, "Actor id", options.ActorID)
		if err != nil {
			return err
		}
		options.CredentialFile, err = promptValue(scanner, output, "Work credential file", options.CredentialFile)
		if err != nil {
			return err
		}
		if len(options.Bindings) == 0 {
			roles, roleErr := promptValue(scanner, output, "Leaf roles (comma separated)", "builder")
			if roleErr != nil {
				return roleErr
			}
			options.Bindings = make(map[string]SoloBinding)
			for _, role := range append(strings.Split(roles, ","), "orchestrator") {
				role = strings.TrimSpace(role)
				project, askErr := promptValue(scanner, output, role+" Solo project id", "")
				if askErr != nil {
					return askErr
				}
				tool, askErr := promptValue(scanner, output, role+" Solo agent-tool id", "")
				if askErr != nil {
					return askErr
				}
				harness, askErr := promptValue(scanner, output, role+" harness (claude/codex/opencode)", "")
				if askErr != nil {
					return askErr
				}
				projectID, projectErr := strconv.ParseInt(project, 10, 64)
				toolID, toolErr := strconv.ParseInt(tool, 10, 64)
				if projectErr != nil || toolErr != nil {
					return errors.New("solo ids must be integers")
				}
				options.Bindings[role] = SoloBinding{ProjectID: projectID, AgentToolID: toolID, Harness: harness, ExtraArgs: []string{}}
			}
		}
	}
	if options.Schedule != "print" && options.Schedule != "install" {
		return errors.New("work setup schedule must be print or install")
	}
	if options.Executable == "" {
		executable, err := os.Executable()
		if err != nil {
			return errors.New("capstan executable path could not be resolved")
		}
		options.Executable = executable
	}
	return validateBindingsInstalled(options.Bindings, projects, tools)
}

func promptValue(scanner *bufio.Scanner, output io.Writer, label, current string) (string, error) {
	if current != "" {
		return current, nil
	}
	if _, err := fmt.Fprintf(output, "%s: ", label); err != nil {
		return "", errors.New("work setup prompt could not be written")
	}
	if !scanner.Scan() {
		return "", errors.New("work setup input ended before setup was complete")
	}
	value := strings.TrimSpace(scanner.Text())
	if value == "" {
		return "", errors.New("work setup values cannot be empty")
	}
	return value, nil
}

func validateBindingsInstalled(bindings map[string]SoloBinding, projects []SoloProject, tools []SoloAgentTool) error {
	if _, ok := bindings["orchestrator"]; !ok {
		return errors.New("work setup requires an explicit orchestrator binding")
	}
	projectIDs := make(map[int64]bool, len(projects))
	for _, project := range projects {
		projectIDs[project.ID] = true
	}
	toolIDs := make(map[int64]bool, len(tools))
	for _, tool := range tools {
		enabled := tool.Enabled == nil || *tool.Enabled
		toolIDs[tool.ID] = enabled
	}
	for role, binding := range bindings {
		if role == "" || !projectIDs[binding.ProjectID] || !toolIDs[binding.AgentToolID] || !supportedHarness(binding.Harness) {
			return fmt.Errorf("binding for role %q does not name an installed project and enabled tool by id", role)
		}
		if binding.ExtraArgs == nil {
			binding.ExtraArgs = []string{}
			bindings[role] = binding
		}
	}
	return nil
}

type setupPlan struct {
	configPath       string
	promptPath       string
	orchestratorPath string
	statePath        string
	workDir          string
	lockPath         string
	configData       []byte
	promptData       []byte
	orchestratorData []byte
	stagedConfig     Config
	cleanup          func()
}

func stageSetupConfig(options SetupOptions) (setupPlan, error) {
	abs, err := filepath.Abs(options.ConfigPath)
	if err != nil {
		return setupPlan{}, errors.New("work config path could not be resolved")
	}
	dir := filepath.Dir(abs)
	credential, err := resolveSetupPath(dir, options.CredentialFile)
	if err != nil {
		return setupPlan{}, errors.New("work credential path could not be resolved")
	}
	soloMCP, err := resolveSetupPath(dir, options.SoloMCPPath)
	if err != nil {
		return setupPlan{}, errors.New("solo MCP path could not be resolved")
	}
	roles := make([]string, 0, len(options.Bindings))
	providers := make(map[string]string, len(options.Bindings))
	for role, binding := range options.Bindings {
		roles = append(roles, role)
		providers[role] = binding.Harness
	}
	for _, role := range options.AmpRoles {
		roles = append(roles, role)
		providers[role] = "claude"
	}
	sort.Strings(roles)
	raw := rawConfig{
		Version: 2, Launcher: "solo", CapstanURL: options.CapstanURL, Team: options.Team, ActorID: options.ActorID,
		CredentialFile: credential, MaxConcurrent: &options.MaxConcurrent, OrchestratorCapacity: &options.OrchestratorCapacity,
		MaxLoad1m: 8, MaxMemoryPercent: 85, Capabilities: []string{}, Roles: roles, RoleProviders: providers,
		StateFile: filepath.Join("var", "work-state.json"), WorkDir: filepath.Join("var", "work-items"), LockFile: filepath.Join("var", "work.lock"),
		PromptTemplate: "work-prompt.md", OrchestratorPromptTemplate: "orchestrator-prompt.md", SoloMCPPath: soloMCP,
		SoloCallTimeoutSeconds: 30, SoloReadyTimeoutSeconds: 120, SoloBindings: options.Bindings, RequestTimeoutSeconds: 15,
	}
	if len(options.AmpRoles) > 0 {
		raw.Version = 3
		raw.Launcher = "hybrid"
		raw.AmpPath = options.AmpPath
		raw.AmpRoles = options.AmpRoles
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return setupPlan{}, errors.New("work config could not be encoded")
	}
	data = append(data, '\n')
	root, err := os.MkdirTemp("", "capstan-work-setup-*")
	if err != nil {
		return setupPlan{}, errors.New("work setup staging directory could not be created")
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	promptData := []byte(soloPromptTemplate("builder"))
	orchestratorData := []byte(soloPromptTemplate("orchestrator"))
	if err := atomicPrivateWrite(filepath.Join(root, "work-prompt.md"), promptData); err != nil {
		cleanup()
		return setupPlan{}, errors.New("staged work prompt could not be written")
	}
	if err := atomicPrivateWrite(filepath.Join(root, "orchestrator-prompt.md"), orchestratorData); err != nil {
		cleanup()
		return setupPlan{}, errors.New("staged orchestrator prompt could not be written")
	}
	stagedPath := filepath.Join(root, filepath.Base(abs))
	if err := atomicPrivateWrite(stagedPath, data); err != nil {
		cleanup()
		return setupPlan{}, errors.New("staged work config could not be written")
	}
	config, err := LoadConfig(stagedPath)
	if err != nil {
		cleanup()
		return setupPlan{}, err
	}
	return setupPlan{
		configPath: abs, promptPath: filepath.Join(dir, "work-prompt.md"), orchestratorPath: filepath.Join(dir, "orchestrator-prompt.md"),
		statePath: filepath.Join(dir, "var", "work-state.json"), workDir: filepath.Join(dir, "var", "work-items"), lockPath: filepath.Join(dir, "var", "work.lock"),
		configData: data, promptData: promptData, orchestratorData: orchestratorData, stagedConfig: config, cleanup: cleanup,
	}, nil
}

func resolveSetupPath(dir, value string) (string, error) {
	if strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(dir, value)
	}
	return filepath.Abs(value)
}

func lockSetupReplacement(plan setupPlan) (func() error, error) {
	var existing *Config
	if _, err := os.Stat(plan.configPath); err == nil {
		loaded, loadErr := LoadConfig(plan.configPath)
		if loadErr != nil {
			return nil, errors.New("refusing to replace an unreadable existing work config")
		}
		existing = &loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("existing work config could not be inspected")
	}
	lockPaths := []string{plan.lockPath}
	if existing != nil && existing.LockFile != plan.lockPath {
		lockPaths = append(lockPaths, existing.LockFile)
	}
	sort.Strings(lockPaths)
	locks := make([]*Lock, 0, len(lockPaths))
	release := func() error {
		var result error
		for index := len(locks) - 1; index >= 0; index-- {
			result = errors.Join(result, locks[index].Release())
		}
		return result
	}
	for _, path := range lockPaths {
		lock, err := AcquireLock(path)
		if err != nil {
			_ = release()
			if errors.Is(err, ErrLockHeld) {
				return nil, errors.New("refusing to replace work config while a drain holds the overlap lock")
			}
			return nil, err
		}
		locks = append(locks, lock)
	}
	stores := []*StateStore{NewStateStore(plan.statePath, plan.workDir)}
	if existing != nil && (existing.StateFile != plan.statePath || existing.WorkDir != plan.workDir) {
		stores = append(stores, NewStateStore(existing.StateFile, existing.WorkDir))
	}
	for _, store := range stores {
		state, err := store.Load()
		if err != nil || len(state.Items) != 0 {
			_ = release()
			return nil, errors.New("refusing to replace work config while local claims or unreadable state remain")
		}
	}
	return release, nil
}

func commitSetupConfig(plan setupPlan) error {
	if err := atomicPrivateWrite(plan.promptPath, plan.promptData); err != nil {
		return errors.New("work prompt could not be written")
	}
	if err := atomicPrivateWrite(plan.orchestratorPath, plan.orchestratorData); err != nil {
		return errors.New("orchestrator prompt could not be written")
	}
	if err := atomicPrivateWrite(plan.configPath, plan.configData); err != nil {
		return errors.New("work config could not be written")
	}
	return nil
}
