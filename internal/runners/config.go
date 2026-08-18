// Package runners defines configured local runners and their health-reading seam.
package runners

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	capstanconfig "github.com/artisan-build/capstan-cli/internal/config"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

const configFilename = "config.yaml"

// Config is the runner monitoring configuration.
type Config struct {
	Inbox   InboxConfig
	State   StateConfig
	Runners []Runner
}

// InboxConfig identifies the directory where later commands may write inbox items.
type InboxConfig struct {
	Path string
}

// StateConfig identifies the directory reserved for runner observation state.
type StateConfig struct {
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
	Name  string
	Type  RunnerType
	Label string
	// ExpectedCadence is zero for on-demand runners, which must never be checked for staleness.
	ExpectedCadence time.Duration
	// StalenessThreshold defaults to ExpectedCadence when omitted, allowing one missed cycle of grace.
	StalenessThreshold time.Duration
	ActivitySource     string
}

type rawConfig struct {
	Inbox   rawInbox    `mapstructure:"inbox"`
	State   rawState    `mapstructure:"state"`
	Runners []rawRunner `mapstructure:"runners"`
}

type rawInbox struct {
	Path string `mapstructure:"path"`
}

type rawState struct {
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

var (
	runnerNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	runnerIndexPattern = regexp.MustCompile(`\[(\d+)\]`)
)

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
	if err := decodeKnownConfig(v, &raw); err != nil {
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
	if err := validateDirectory("inbox.path", inboxPath); err != nil {
		return Config{}, err
	}

	statePath, err := expandHome(raw.State.Path)
	if err != nil {
		return Config{}, fmt.Errorf("state.path: %w", err)
	}
	if strings.TrimSpace(statePath) == "" {
		return Config{}, fmt.Errorf("state.path is required")
	}
	if err := validateDirectory("state.path", statePath); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Inbox:   InboxConfig{Path: inboxPath},
		State:   StateConfig{Path: statePath},
		Runners: make([]Runner, 0, len(raw.Runners)),
	}
	seenNames := make(map[string]string, len(raw.Runners))
	seenLabels := make(map[RunnerType]map[string]string)

	for i, candidate := range raw.Runners {
		where := runnerDescription(i, candidate.Name)
		if strings.TrimSpace(candidate.Name) == "" {
			return Config{}, fmt.Errorf("%s field name is required", where)
		}
		if !runnerNamePattern.MatchString(candidate.Name) {
			return Config{}, fmt.Errorf(
				"%s field name must be 1-64 ASCII letters, digits, dots, underscores, or hyphens and start with a letter or digit",
				where,
			)
		}
		nameKey := strings.ToLower(candidate.Name)
		if prior, exists := seenNames[nameKey]; exists {
			return Config{}, fmt.Errorf(
				"%s field name duplicates runner %q (names are compared case-insensitively)",
				where,
				prior,
			)
		}
		seenNames[nameKey] = candidate.Name

		runner, err := validateRunner(candidate, where)
		if err != nil {
			return Config{}, err
		}
		labels := seenLabels[runner.Type]
		if labels == nil {
			labels = make(map[string]string)
			seenLabels[runner.Type] = labels
		}
		if prior, exists := labels[runner.Label]; exists {
			return Config{}, fmt.Errorf(
				"%s field label %q duplicates runner %q",
				where,
				runner.Label,
				prior,
			)
		}
		labels[runner.Label] = runner.Name
		cfg.Runners = append(cfg.Runners, runner)
	}

	return cfg, nil
}

func validateDirectory(field, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s %q does not exist", field, path)
		}

		return fmt.Errorf("inspect %s %q: %w", field, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", field, path)
	}

	probe, err := os.CreateTemp(path, ".capstan-write-test-*")
	if err != nil {
		return fmt.Errorf("%s %q is not writable: %w", field, path, err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)

		return fmt.Errorf("%s %q is not writable: %w", field, path, err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("clean up %s write test %q: %w", field, path, err)
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

		if raw.ExpectedCadence == "" {
			if raw.StalenessThreshold != "" {
				return Runner{}, fmt.Errorf(
					"%s field staleness_threshold requires expected_cadence",
					where,
				)
			}
		} else {
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
	}

	activitySource, err := expandHome(raw.ActivitySource)
	if err != nil {
		return Runner{}, fmt.Errorf("%s field activity_source: %w", where, err)
	}
	runner.ActivitySource = activitySource

	return runner, nil
}

func decodeKnownConfig(v *viper.Viper, raw *rawConfig) error {
	strict := func(config *mapstructure.DecoderConfig) {
		config.WeaklyTypedInput = false
		config.ErrorUnused = true
	}

	sections := []struct {
		name   string
		target any
	}{
		{name: "inbox", target: &raw.Inbox},
		{name: "state", target: &raw.State},
		{name: "runners", target: &raw.Runners},
	}
	for _, section := range sections {
		if !v.IsSet(section.name) {
			continue
		}
		if err := v.UnmarshalKey(section.name, section.target, strict); err != nil {
			message := runnerIndexPattern.ReplaceAllString(err.Error(), "runner[$1]")

			return fmt.Errorf("%s: %s", section.name, message)
		}
	}

	return nil
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
