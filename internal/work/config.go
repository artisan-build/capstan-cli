package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	capstanconfig "github.com/artisan-build/capstan-cli/internal/config"
)

const defaultConfigFilename = "work.json"

var (
	actorPattern    = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	teamPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	placeholderRE   = regexp.MustCompile(`\{([a-z_]+)\}`)
	knownFields     = map[string]struct{}{
		"config_dir": {}, "fence": {}, "item_id": {}, "launch_nonce": {}, "links_file": {},
		"prompt_file": {}, "role": {}, "seat_fence": {}, "session_ref": {}, "task_id": {}, "work_dir": {},
	}
)

type rawConfig struct {
	Version                    int                    `json:"version"`
	Launcher                   string                 `json:"launcher"`
	CapstanURL                 string                 `json:"capstan_url"`
	Team                       string                 `json:"team"`
	ActorID                    string                 `json:"actor_id"`
	CredentialFile             string                 `json:"credential_file"`
	MaxConcurrent              *int                   `json:"max_concurrent"`
	OrchestratorCapacity       *int                   `json:"orchestrator_capacity"`
	MaxLoad1m                  float64                `json:"max_load_1m"`
	MaxMemoryPercent           float64                `json:"max_memory_percent"`
	Capabilities               []string               `json:"capabilities"`
	Roles                      []string               `json:"roles"`
	RoleProviders              map[string]string      `json:"role_providers"`
	StateFile                  string                 `json:"state_file"`
	WorkDir                    string                 `json:"work_dir"`
	LockFile                   string                 `json:"lock_file"`
	PromptTemplate             string                 `json:"prompt_template"`
	OrchestratorPromptTemplate string                 `json:"orchestrator_prompt_template"`
	RoleCommands               map[string][]string    `json:"role_commands"`
	SoloMCPPath                string                 `json:"solo_mcp_path"`
	SoloCallTimeoutSeconds     float64                `json:"solo_call_timeout_seconds"`
	SoloReadyTimeoutSeconds    float64                `json:"solo_ready_timeout_seconds"`
	SoloBindings               map[string]SoloBinding `json:"solo_bindings"`
	RequestTimeoutSeconds      float64                `json:"request_timeout_seconds"`
}

// DefaultConfigPath returns work.json beneath Capstan's XDG config directory.
func DefaultConfigPath() (string, error) {
	dir, err := capstanconfig.Dir()
	if err != nil {
		return "", fmt.Errorf("resolve work config directory: %w", err)
	}

	return filepath.Join(dir, defaultConfigFilename), nil
}

// LoadConfig strictly parses and validates one private drain configuration file.
func LoadConfig(path string) (Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, errors.New("resolve work config path")
	}
	if err := requirePrivateRegularFile(abs, "work config"); err != nil {
		return Config{}, err
	}

	file, err := os.Open(abs)
	if err != nil {
		return Config{}, errors.New("work config could not be read")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() > 1<<20 {
		return Config{}, errors.New("work config exceeds the size limit")
	}

	data, err := io.ReadAll(file)
	if err != nil || validateUniqueJSON(data) != nil {
		return Config{}, errors.New("work config is not valid strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw rawConfig
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, errors.New("work config is not valid strict JSON")
	}
	if err := expectJSONEOF(decoder); err != nil {
		return Config{}, errors.New("work config contains trailing data")
	}

	return validateConfig(abs, raw)
}

func validateConfig(path string, raw rawConfig) (Config, error) {
	if raw.Version != 1 && raw.Version != 2 {
		return Config{}, errors.New("work config version must be 1 or 2")
	}
	server, err := validateDrainURL(raw.CapstanURL)
	if err != nil {
		return Config{}, err
	}
	if !teamPattern.MatchString(raw.Team) {
		return Config{}, errors.New("work config team must be a route id or key")
	}
	if !actorPattern.MatchString(raw.ActorID) {
		return Config{}, errors.New("work config actor_id must be a numeric actor id")
	}
	if raw.MaxConcurrent == nil || *raw.MaxConcurrent < 0 || *raw.MaxConcurrent > MaxBatch {
		return Config{}, fmt.Errorf("work config max_concurrent must be from 0 through %d", MaxBatch)
	}
	if raw.OrchestratorCapacity == nil || *raw.OrchestratorCapacity < 0 || *raw.OrchestratorCapacity > MaxBatch {
		return Config{}, fmt.Errorf("work config orchestrator_capacity must be from 0 through %d", MaxBatch)
	}
	if *raw.MaxConcurrent+*raw.OrchestratorCapacity > MaxBatch {
		return Config{}, fmt.Errorf("work config combined capacities must not exceed %d", MaxBatch)
	}
	if raw.MaxLoad1m <= 0 {
		return Config{}, errors.New("work config max_load_1m must be greater than zero")
	}
	if raw.MaxMemoryPercent <= 0 || raw.MaxMemoryPercent > 100 {
		return Config{}, errors.New("work config max_memory_percent must be greater than zero and at most 100")
	}
	if raw.RequestTimeoutSeconds <= 0 || raw.RequestTimeoutSeconds > 120 {
		return Config{}, errors.New("work config request_timeout_seconds must be greater than zero and at most 120")
	}
	if err := validateScalars("roles", raw.Roles, true); err != nil {
		return Config{}, err
	}
	if err := validateScalars("capabilities", raw.Capabilities, false); err != nil {
		return Config{}, err
	}
	if raw.Capabilities == nil {
		return Config{}, errors.New("work config capabilities must be an explicit JSON list")
	}
	if len(raw.RoleProviders) != len(raw.Roles) {
		return Config{}, errors.New("work config role_providers must exactly match roles")
	}
	for _, role := range raw.Roles {
		provider, ok := raw.RoleProviders[role]
		if !ok || !providerPattern.MatchString(provider) {
			return Config{}, fmt.Errorf("work config provider for role %q is invalid", role)
		}
	}
	launcher := raw.Launcher
	if raw.Version == 1 && launcher == "" {
		launcher = "process"
	}
	if raw.Version == 1 && launcher != "process" {
		return Config{}, errors.New("work config version 1 supports only the process launcher")
	}
	if raw.Version == 1 && (raw.SoloMCPPath != "" || raw.SoloCallTimeoutSeconds != 0 || raw.SoloReadyTimeoutSeconds != 0 || raw.SoloBindings != nil) {
		return Config{}, errors.New("work config version 1 cannot contain Solo launcher fields")
	}
	if raw.Version == 2 && launcher != "solo" {
		return Config{}, errors.New("work config version 2 launcher must be solo")
	}
	if launcher == "process" {
		if len(raw.RoleCommands) != len(raw.Roles) {
			return Config{}, errors.New("work config role_commands must exactly match roles")
		}
		for _, role := range raw.Roles {
			command, ok := raw.RoleCommands[role]
			if !ok || len(command) == 0 {
				return Config{}, fmt.Errorf("work config command for role %q must be a non-empty argv list", role)
			}
			for _, part := range command {
				if part == "" || strings.ContainsRune(part, 0) {
					return Config{}, fmt.Errorf("work config command for role %q contains an empty or invalid argument", role)
				}
				if err := validateTemplate(part); err != nil {
					return Config{}, fmt.Errorf("work config command for role %q: %w", role, err)
				}
			}
		}
	} else {
		if raw.RoleCommands != nil {
			return Config{}, errors.New("work config version 2 cannot contain role_commands")
		}
		if len(raw.SoloBindings) != len(raw.Roles) {
			return Config{}, errors.New("work config solo_bindings must exactly match roles")
		}
		if raw.SoloCallTimeoutSeconds <= 0 || raw.SoloCallTimeoutSeconds > 120 ||
			raw.SoloReadyTimeoutSeconds <= 0 || raw.SoloReadyTimeoutSeconds > 300 {
			return Config{}, errors.New("work config Solo timeouts are outside the allowed range")
		}
		for _, role := range raw.Roles {
			binding, ok := raw.SoloBindings[role]
			if !providerPattern.MatchString(role) || !ok || binding.ProjectID < 1 || binding.AgentToolID < 1 || !supportedHarness(binding.Harness) || binding.ExtraArgs == nil || len(binding.ExtraArgs) > 64 {
				return Config{}, fmt.Errorf("work config Solo binding for role %q is invalid", role)
			}
			for _, arg := range binding.ExtraArgs {
				if arg == "" || len(arg) > 4096 || strings.ContainsRune(arg, 0) {
					return Config{}, fmt.Errorf("work config Solo binding for role %q has an invalid argument", role)
				}
			}
		}
	}

	dir := filepath.Dir(path)
	resolve := func(value, label string) (string, error) {
		if value == "" {
			return "", fmt.Errorf("work config %s is required", label)
		}
		if strings.HasPrefix(value, "~/") {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return "", fmt.Errorf("work config %s could not expand home", label)
			}
			value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(dir, value)
		}

		return filepath.Clean(value), nil
	}
	credential, err := resolve(raw.CredentialFile, "credential_file")
	if err != nil {
		return Config{}, err
	}
	state, err := resolve(raw.StateFile, "state_file")
	if err != nil {
		return Config{}, err
	}
	workDir, err := resolve(raw.WorkDir, "work_dir")
	if err != nil {
		return Config{}, err
	}
	lock, err := resolve(raw.LockFile, "lock_file")
	if err != nil {
		return Config{}, err
	}
	prompt, err := resolve(raw.PromptTemplate, "prompt_template")
	if err != nil {
		return Config{}, err
	}
	orchestratorPrompt, err := resolve(raw.OrchestratorPromptTemplate, "orchestrator_prompt_template")
	if err != nil {
		return Config{}, err
	}
	soloMCP := ""
	if launcher == "solo" {
		if raw.SoloMCPPath == "" {
			raw.SoloMCPPath = defaultSoloMCPPath(runtime.GOOS)
		}
		soloMCP, err = resolve(raw.SoloMCPPath, "solo_mcp_path")
		if err != nil {
			return Config{}, err
		}
	}
	for _, template := range []string{prompt, orchestratorPrompt} {
		data, readErr := os.ReadFile(template)
		if readErr != nil {
			return Config{}, errors.New("work prompt template could not be read")
		}
		if err := validateTemplate(string(data)); err != nil {
			return Config{}, fmt.Errorf("work prompt template: %w", err)
		}
	}

	return Config{
		Path: path, CapstanURL: server, Team: raw.Team, ActorID: raw.ActorID, CredentialFile: credential,
		MaxConcurrent: *raw.MaxConcurrent, OrchestratorCapacity: *raw.OrchestratorCapacity,
		MaxLoad1m: raw.MaxLoad1m, MaxMemoryPercent: raw.MaxMemoryPercent,
		Capabilities:  append(make([]string, 0, len(raw.Capabilities)), raw.Capabilities...),
		Roles:         append(make([]string, 0, len(raw.Roles)), raw.Roles...),
		RoleProviders: cloneStringMap(raw.RoleProviders), StateFile: state, WorkDir: workDir, LockFile: lock,
		PromptTemplate: prompt, OrchestratorPromptTemplate: orchestratorPrompt,
		Launcher: launcher, RoleCommands: cloneCommands(raw.RoleCommands), SoloMCPPath: soloMCP,
		SoloCallTimeout:  time.Duration(raw.SoloCallTimeoutSeconds * float64(time.Second)),
		SoloReadyTimeout: time.Duration(raw.SoloReadyTimeoutSeconds * float64(time.Second)),
		SoloBindings:     cloneSoloBindings(raw.SoloBindings), RequestTimeout: time.Duration(raw.RequestTimeoutSeconds * float64(time.Second)),
	}, nil
}

func validateDrainURL(value string) (string, error) {
	clean := strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(clean)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("work config capstan_url must be an http(s) URL without credentials, query, or fragment")
	}
	if parsed.Scheme == "https" {
		return clean, nil
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		if host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
			return clean, nil
		}
	}

	return "", errors.New("work config capstan_url must use HTTPS except for loopback development")
}

func validateScalars(label string, values []string, nonempty bool) error {
	if nonempty && len(values) == 0 {
		return fmt.Errorf("work config %s must not be empty", label)
	}
	if len(values) > MaxBatch {
		return fmt.Errorf("work config %s must contain at most %d entries", label, MaxBatch)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || len(value) > 64 {
			return fmt.Errorf("work config %s entries must be non-empty strings of at most 64 bytes", label)
		}
		if _, found := seen[value]; found {
			return fmt.Errorf("work config %s entries must be distinct", label)
		}
		seen[value] = struct{}{}
	}

	return nil
}

func validateTemplate(value string) error {
	for _, match := range placeholderRE.FindAllStringSubmatch(value, -1) {
		if _, ok := knownFields[match[1]]; !ok {
			return fmt.Errorf("contains unknown placeholder %q", match[0])
		}
	}
	withoutKnown := placeholderRE.ReplaceAllString(value, "")
	if strings.Contains(withoutKnown, "{") || strings.Contains(withoutKnown, "}") {
		return errors.New("contains an invalid placeholder")
	}

	return nil
}

func expectJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}

	return err
}

func validateUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid JSON object key")
				}
				if _, duplicate := seen[key]; duplicate {
					return errors.New("duplicate JSON object key")
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if err := walk(); err != nil {
		return err
	}

	return expectJSONEOF(decoder)
}

func cloneStringMap(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}

	return copy
}

func cloneCommands(source map[string][]string) map[string][]string {
	copy := make(map[string][]string, len(source))
	for key, value := range source {
		copy[key] = append([]string(nil), value...)
	}

	return copy
}

func cloneSoloBindings(source map[string]SoloBinding) map[string]SoloBinding {
	copy := make(map[string]SoloBinding, len(source))
	for key, value := range source {
		value.ExtraArgs = append([]string{}, value.ExtraArgs...)
		copy[key] = value
	}
	return copy
}

func supportedHarness(value string) bool {
	return value == "claude" || value == "codex" || value == "opencode"
}

func defaultSoloMCPPath(goos string) string {
	if goos == "darwin" {
		return "/Applications/Solo.app/Contents/MacOS/mcp"
	}
	if goos == "linux" {
		return "/usr/local/bin/solo-mcp"
	}
	return "solo-mcp"
}
