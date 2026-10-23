//go:build windows

package work

import (
	"context"
	"errors"
)

// SystemHealth fails closed where a load-average equivalent is unavailable.
type SystemHealth struct{}

// Measure reports unsupported admission measurement on Windows.
func (SystemHealth) Measure(context.Context) (HealthSnapshot, error) {
	return HealthSnapshot{}, errors.New("system admission measurement is unavailable on Windows")
}
