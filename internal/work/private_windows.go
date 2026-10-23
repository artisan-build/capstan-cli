//go:build windows

package work

import (
	"fmt"
	"os"
)

func requirePrivateRegularFile(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s could not be inspected", label)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", label)
	}

	return nil
}

func syncDirectory(string) error { return nil }
