package browser

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Open opens url in the user's default browser.
func Open(url string) error {
	var command string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{url}
	case "windows":
		command = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		command = "xdg-open"
		args = []string{url}
	}

	if err := exec.Command(command, args...).Start(); err != nil {
		return fmt.Errorf("start browser command: %w", err)
	}

	return nil
}
