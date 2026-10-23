package work

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// Setup discovers immutable Solo ids, writes a private config, emits the schedule, and runs the self-test.
func Setup(ctx context.Context, options SetupOptions, input io.Reader, output io.Writer, solo SoloCaller, tester SelfTestRunner) error {
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
	config, err := writeSetupConfig(options)
	if err != nil {
		return err
	}
	schedule, err := RenderWorkSchedule(options.Platform, options.Executable, config.Path)
	if err != nil {
		return err
	}
	if options.Schedule == "install" {
		if err := InstallWorkSchedule(options.Platform, schedule); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(output, "Installed the Capstan Work one-shot schedule."); err != nil {
			return errors.New("work setup output could not be written")
		}
	} else {
		if _, err := fmt.Fprint(output, schedule.Text); err != nil {
			return errors.New("work setup output could not be written")
		}
	}
	evidence, err := tester.Run(ctx, config, output)
	if err != nil {
		return err
	}
	if err := evidence.Validate(); err != nil {
		return err
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

func writeSetupConfig(options SetupOptions) (Config, error) {
	abs, err := filepath.Abs(options.ConfigPath)
	if err != nil {
		return Config{}, errors.New("work config path could not be resolved")
	}
	if _, statErr := os.Stat(abs); statErr == nil {
		existing, loadErr := LoadConfig(abs)
		if loadErr != nil {
			return Config{}, errors.New("refusing to replace an unreadable existing work config")
		}
		state, stateErr := NewStateStore(existing.StateFile, existing.WorkDir).Load()
		if stateErr != nil || len(state.Items) != 0 {
			return Config{}, errors.New("refusing to replace work config while local claims or unreadable state remain")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Config{}, errors.New("existing work config could not be inspected")
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Config{}, errors.New("work config directory could not be created")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Config{}, errors.New("work config directory could not be secured")
	}
	prompt := filepath.Join(dir, "work-prompt.md")
	orchestratorPrompt := filepath.Join(dir, "orchestrator-prompt.md")
	if err := atomicPrivateWrite(prompt, []byte("Fetch the current signed brief with the launch nonce from the Solo pointer.\n")); err != nil {
		return Config{}, err
	}
	if err := atomicPrivateWrite(orchestratorPrompt, []byte("Fetch the current signed orchestrator brief with the launch nonce from the Solo pointer.\n")); err != nil {
		return Config{}, err
	}
	roles := make([]string, 0, len(options.Bindings))
	providers := make(map[string]string, len(options.Bindings))
	for role, binding := range options.Bindings {
		roles = append(roles, role)
		providers[role] = binding.Harness
	}
	sort.Strings(roles)
	raw := rawConfig{
		Version: 2, Launcher: "solo", CapstanURL: options.CapstanURL, Team: options.Team, ActorID: options.ActorID,
		CredentialFile: options.CredentialFile, MaxConcurrent: &options.MaxConcurrent, OrchestratorCapacity: &options.OrchestratorCapacity,
		MaxLoad1m: 8, MaxMemoryPercent: 85, Capabilities: []string{}, Roles: roles, RoleProviders: providers,
		StateFile: filepath.Join(dir, "var", "work-state.json"), WorkDir: filepath.Join(dir, "var", "work-items"), LockFile: filepath.Join(dir, "var", "work.lock"),
		PromptTemplate: prompt, OrchestratorPromptTemplate: orchestratorPrompt, SoloMCPPath: options.SoloMCPPath,
		SoloCallTimeoutSeconds: 30, SoloReadyTimeoutSeconds: 120, SoloBindings: options.Bindings, RequestTimeoutSeconds: 15,
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return Config{}, errors.New("work config could not be encoded")
	}
	if err := atomicPrivateWrite(abs, append(data, '\n')); err != nil {
		return Config{}, errors.New("work config could not be written")
	}
	return LoadConfig(abs)
}
