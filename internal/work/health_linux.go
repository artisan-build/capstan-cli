//go:build linux

package work

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
)

// SystemHealth measures Linux load and memory from procfs.
type SystemHealth struct{}

// Measure reads /proc/loadavg and /proc/meminfo.
func (SystemHealth) Measure(context.Context) (HealthSnapshot, error) {
	loadData, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return HealthSnapshot{}, errors.New("load average could not be read")
	}
	loadFields := strings.Fields(string(loadData))
	if len(loadFields) == 0 {
		return HealthSnapshot{}, errors.New("load average is malformed")
	}
	load, err := strconv.ParseFloat(loadFields[0], 64)
	if err != nil {
		return HealthSnapshot{}, errors.New("load average is malformed")
	}
	memoryData, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return HealthSnapshot{}, errors.New("memory usage could not be read")
	}
	values := make(map[string]float64)
	for _, line := range strings.Split(string(memoryData), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			value, parseErr := strconv.ParseFloat(fields[1], 64)
			if parseErr == nil {
				values[strings.TrimSuffix(fields[0], ":")] = value
			}
		}
	}
	total, available := values["MemTotal"], values["MemAvailable"]
	if total <= 0 || available < 0 || available > total {
		return HealthSnapshot{}, errors.New("memory usage is malformed")
	}

	return HealthSnapshot{Load1m: load, MemoryPercent: 100 * (total - available) / total}, nil
}
