//go:build darwin

package work

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var numberPattern = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)

// SystemHealth measures macOS load and memory using sysctl and vm_stat.
type SystemHealth struct{}

// Measure reads one-minute load and used memory percentage.
func (SystemHealth) Measure(ctx context.Context) (HealthSnapshot, error) {
	loadOutput, err := exec.CommandContext(ctx, "sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return HealthSnapshot{}, errors.New("load average could not be read")
	}
	loadValues := numberPattern.FindAllString(string(loadOutput), -1)
	if len(loadValues) == 0 {
		return HealthSnapshot{}, errors.New("load average is malformed")
	}
	load, err := strconv.ParseFloat(loadValues[0], 64)
	if err != nil {
		return HealthSnapshot{}, errors.New("load average is malformed")
	}
	totalOutput, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return HealthSnapshot{}, errors.New("memory size could not be read")
	}
	total, err := strconv.ParseFloat(strings.TrimSpace(string(totalOutput)), 64)
	if err != nil || total <= 0 {
		return HealthSnapshot{}, errors.New("memory size is malformed")
	}
	vmOutput, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return HealthSnapshot{}, errors.New("memory usage could not be read")
	}
	pageMatch := regexp.MustCompile(`page size of ([0-9]+) bytes`).FindStringSubmatch(string(vmOutput))
	if len(pageMatch) != 2 {
		return HealthSnapshot{}, errors.New("memory usage is malformed")
	}
	pageSize, _ := strconv.ParseFloat(pageMatch[1], 64)
	availablePages := float64(0)
	for _, name := range []string{"Pages free", "Pages inactive", "Pages speculative", "Pages purgeable"} {
		match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `:\s+([0-9]+)\.`).FindStringSubmatch(string(vmOutput))
		if len(match) == 2 {
			pages, _ := strconv.ParseFloat(match[1], 64)
			availablePages += pages
		}
	}

	return HealthSnapshot{Load1m: load, MemoryPercent: 100 * (total - availablePages*pageSize) / total}, nil
}
