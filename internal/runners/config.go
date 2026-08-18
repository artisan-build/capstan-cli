// Package runners defines configured local runners and their health-reading seam.
package runners

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	capstanconfig "github.com/artisan-build/capstan-cli/internal/config"
	"github.com/spf13/viper"
)

const configFilename = "config.yaml"

// Config is the runner monitoring configuration.
type Config struct {
	Inbox   InboxConfig
	Runners []Runner
}

// InboxConfig identifies the directory where later commands may write inbox items.
type InboxConfig struct {
	Path string
}

// RunnerType identifies the platform mechanism used to run a configured job.
type RunnerType string

const (
	// RunnerTypeLaunchd identifies a macOS launchd job.
	RunnerTypeLaunchd RunnerType = "launchd"
)

var supportedRunnerTypes = []RunnerType{RunnerTypeLaunchd}

// Runner describes one local job whose health can be read.
type Runner struct {
	Name            string
	Type            RunnerType
	Label           string
	ExpectedCadence time.Duration
	// StalenessThreshold defaults to ExpectedCadence when omitted, allowing one missed cycle of grace.
	StalenessThreshold time.Duration
	ActivitySource     string
}

type rawConfig struct {
	Inbox   rawInbox    `mapstructure:"inbox"`
	Runners []rawRunner `mapstructure:"runners"`
}

type rawInbox struct {
	Path string `mapstructure:"path"`
}

type rawRunner struct {
	Name               string `mapstructure:"name"`
	Type               string `mapstructure:"type"`
	Label              string `mapstructure:"label"`
	ExpectedCadence    string `mapstructure:"expected_cadence"`
	StalenessThreshold string `mapstructure:"staleness_threshold"`
	ActivitySource     string `mapstructure:"activity_source"`
}

// Load parses and validates runner configuration from path.
func Load(path string) (Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("read runners config %q: %w", path, err)
	}

	var raw rawConfig
	// Unknown top-level sections are deliberately ignored so server-resolved policy can be added later.
	if err := v.Unmarshal(&raw); err != nil {
		return Config{}, fmt.Errorf("decode runners config %q: %w", path, err)
	}

	return validate(raw)
}

// LoadDefault loads config.yaml from Capstan's XDG configuration directory.
func LoadDefault() (Config, error) {
	dir, err := capstanconfig.Dir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve runners config directory: %w", err)
	}

	return Load(filepath.Join(dir, configFilename))
}

func validate(raw rawConfig) (Config, error) {
	inboxPath, err := expandHome(raw.Inbox.Path)
	if err != nil {
		return Config{}, fmt.Errorf("inbox.path: %w", err)
	}
	if strings.TrimSpace(inboxPath) == "" {
		return Config{}, fmt.Errorf("inbox.path is required")
	}
	if err := validateInbox(inboxPath); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Inbox:   InboxConfig{Path: inboxPath},
		Runners: make([]Runner, 0, len(raw.Runners)),
	}
	seenNames := make(map[string]struct{}, len(raw.Runners))

	for i, candidate := range raw.Runners {
		where := runnerDescription(i, candidate.Name)
		if strings.TrimSpace(candidate.Name) == "" {
			return Config{}, fmt.Errorf("%s field name is required", where)
		}
		if _, exists := seenNames[candidate.Name]; exists {
			return Config{}, fmt.Errorf("%s field name is duplicated", where)
		}
		seenNames[candidate.Name] = struct{}{}

		runner, err := validateRunner(candidate, where)
		if err != nil {
			return Config{}, err
		}
		cfg.Runners = append(cfg.Runners, runner)
	}

	return cfg, nil
}

func validateInbox(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("inbox.path %q does not exist", path)
		}

		return fmt.Errorf("inspect inbox.path %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("inbox.path %q is not a directory", path)
	}

	probe, err := os.CreateTemp(path, ".capstan-write-test-*")
	if err != nil {
		return fmt.Errorf("inbox.path %q is not writable: %w", path, err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)

		return fmt.Errorf("inbox.path %q is not writable: %w", path, err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("clean up inbox.path write test %q: %w", path, err)
	}

	return nil
}

func validateRunner(raw rawRunner, where string) (Runner, error) {
	runnerType := RunnerType(raw.Type)
	if !isSupportedRunnerType(runnerType) {
		return Runner{}, fmt.Errorf(
			"%s field type has unsupported value %q; supported types: %s",
			where,
			raw.Type,
			supportedRunnerTypesString(),
		)
	}

	runner := Runner{
		Name:  raw.Name,
		Type:  runnerType,
		Label: raw.Label,
	}

	switch runnerType {
	case RunnerTypeLaunchd:
		if strings.TrimSpace(raw.Label) == "" {
			return Runner{}, fmt.Errorf("%s field label is required for type %q", where, runnerType)
		}

		expectedCadence, err := parseRequiredPositiveDuration(where, "expected_cadence", raw.ExpectedCadence)
		if err != nil {
			return Runner{}, err
		}
		runner.ExpectedCadence = expectedCadence

		if raw.StalenessThreshold == "" {
			runner.StalenessThreshold = expectedCadence
		} else {
			stalenessThreshold, err := parseNonNegativeDuration(where, "staleness_threshold", raw.StalenessThreshold)
			if err != nil {
				return Runner{}, err
			}
			runner.StalenessThreshold = stalenessThreshold
		}
	}

	activitySource, err := expandHome(raw.ActivitySource)
	if err != nil {
		return Runner{}, fmt.Errorf("%s field activity_source: %w", where, err)
	}
	runner.ActivitySource = activitySource

	return runner, nil
}

func parseRequiredPositiveDuration(where, field, value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, fmt.Errorf("%s field %s is required", where, field)
	}

	duration, err := parseNonNegativeDuration(where, field, value)
	if err != nil {
		return 0, err
	}
	if duration == 0 {
		return 0, fmt.Errorf("%s field %s must be greater than zero", where, field)
	}

	return duration, nil
}

func parseNonNegativeDuration(where, field, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s field %s has invalid duration %q: %w", where, field, value, err)
	}
	if duration < 0 {
		return 0, fmt.Errorf("%s field %s must not be negative (got %q)", where, field, value)
	}

	return duration, nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}

	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

func runnerDescription(index int, name string) string {
	if strings.TrimSpace(name) == "" {
		return fmt.Sprintf("runner[%d]", index)
	}

	return fmt.Sprintf("runner %q", name)
}

func isSupportedRunnerType(runnerType RunnerType) bool {
	for _, supported := range supportedRunnerTypes {
		if runnerType == supported {
			return true
		}
	}

	return false
}

func supportedRunnerTypesString() string {
	types := make([]string, len(supportedRunnerTypes))
	for i, runnerType := range supportedRunnerTypes {
		types[i] = string(runnerType)
	}

	return strings.Join(types, ", ")
}
