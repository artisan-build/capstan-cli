package runners

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type runCommand func(ctx context.Context, name string, args ...string) ([]byte, error)

// LaunchdReader reads launchd state without changing the job.
type LaunchdReader struct {
	uid        int
	runCommand runCommand
}

var _ HealthReader = (*LaunchdReader)(nil)

// NewLaunchdReader returns a reader for the current user and system launchd domains.
func NewLaunchdReader() *LaunchdReader {
	return &LaunchdReader{
		uid: os.Getuid(),
		runCommand: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
}

// Read reads one launchd job using the read-only print subcommand.
func (r *LaunchdReader) Read(ctx context.Context, runner Runner) (Health, error) {
	if runner.Type != RunnerTypeLaunchd {
		return Health{}, fmt.Errorf("read runner %q: unsupported type %q", runner.Name, runner.Type)
	}

	targets := []string{
		fmt.Sprintf("gui/%d/%s", r.uid, runner.Label),
		fmt.Sprintf("system/%s", runner.Label),
	}
	for i, target := range targets {
		out, err := r.runCommand(ctx, "launchctl", "print", target)
		if err != nil {
			if launchdServiceMissing(string(out)) {
				if i == len(targets)-1 {
					return Health{Present: false}, nil
				}

				continue
			}

			return Health{}, fmt.Errorf("launchctl print %q: %w: %s", target, err, strings.TrimSpace(string(out)))
		}

		health, err := parseLaunchctlPrint(string(out))
		if err != nil {
			return Health{}, fmt.Errorf("parse launchctl print %q: %w", target, err)
		}

		return health, nil
	}

	return Health{Present: false}, nil
}

func parseLaunchctlPrint(out string) (Health, error) {
	health := Health{Present: true}
	foundRuns := false
	foundTermination := false

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line, ok := launchdTopLevelField(scanner.Text())
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(line, "runs = "):
			runs, err := parseLaunchdInt("runs", strings.TrimPrefix(line, "runs = "))
			if err != nil {
				return Health{}, err
			}
			if runs < 0 {
				return Health{}, fmt.Errorf("runs must not be negative (got %d)", runs)
			}
			health.Runs = runs
			foundRuns = true
		case strings.HasPrefix(line, "last exit code = "):
			value := strings.TrimPrefix(line, "last exit code = ")
			foundTermination = true
			if value == "(never exited)" {
				health.HasRun = false
				continue
			}

			status, err := parseLaunchdInt("last exit code", value)
			if err != nil {
				return Health{}, err
			}
			health.HasRun = true
			health.LastExitStatus = status
		case strings.HasPrefix(line, "last exit reason = "):
			reason := strings.TrimSpace(strings.TrimPrefix(line, "last exit reason = "))
			if reason == "" {
				continue
			}
			health.ExitReason = reason
			health.HasRun = true
			foundTermination = true
		}
	}
	if err := scanner.Err(); err != nil {
		return Health{}, fmt.Errorf("scan output: %w", err)
	}
	if !foundRuns {
		return Health{}, fmt.Errorf("output does not contain runs")
	}
	if !foundTermination {
		return Health{}, fmt.Errorf("output does not contain last exit code or reason")
	}

	return health, nil
}

func launchdTopLevelField(line string) (string, bool) {
	if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
		return "", false
	}

	return strings.TrimSpace(line), true
}

func parseLaunchdInt(field, value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, value, err)
	}

	return parsed, nil
}

func launchdServiceMissing(output string) bool {
	normalized := strings.ToLower(output)

	return strings.Contains(normalized, "could not find service") ||
		strings.Contains(normalized, "could not find specified service") ||
		strings.Contains(normalized, "service not found")
}
