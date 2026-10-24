package work

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

var scheduleCommand = exec.Command

// WorkSchedule is a platform schedule and its dedicated install paths.
type WorkSchedule struct {
	Text  string
	Files map[string]string
}

// RenderWorkSchedule returns a one-shot launchd or systemd schedule using absolute paths.
func RenderWorkSchedule(platform, executable, configPath string) (WorkSchedule, error) {
	if platform == "" {
		platform = runtime.GOOS
	}
	executable, exeErr := filepath.Abs(executable)
	configPath, configErr := filepath.Abs(configPath)
	if exeErr != nil || configErr != nil || strings.ContainsAny(executable, "\x00\r\n") || strings.ContainsAny(configPath, "\x00\r\n") {
		return WorkSchedule{}, errors.New("schedule paths could not be resolved")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return WorkSchedule{}, errors.New("schedule home directory could not be resolved")
	}
	switch platform {
	case "darwin":
		text := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.artisan-build.capstan-work</string>
  <key>CapstanManaged</key><string>capstan-work/v1</string>
  <key>ProgramArguments</key><array><string>%s</string><string>work</string><string>drain</string><string>--config</string><string>%s</string></array>
  <key>StartInterval</key><integer>60</integer>
</dict></plist>
`, xmlEscape(executable), xmlEscape(configPath))
		path := filepath.Join(home, "Library", "LaunchAgents", "com.artisan-build.capstan-work.plist")
		return WorkSchedule{Text: text, Files: map[string]string{path: text}}, nil
	case "linux":
		service := fmt.Sprintf("[Unit]\nDescription=Capstan Work one-shot drain\nX-Capstan-Managed=capstan-work/v1\nX-Capstan-Unit=capstan-work.service\n\n[Service]\nType=oneshot\nExecStart=%s work drain --config %s\n", systemdEscape(executable), systemdEscape(configPath))
		timer := "[Unit]\nDescription=Run Capstan Work every minute\nX-Capstan-Managed=capstan-work/v1\nX-Capstan-Unit=capstan-work.timer\n\n[Timer]\nOnBootSec=1min\nOnUnitActiveSec=1min\nUnit=capstan-work.service\n\n[Install]\nWantedBy=timers.target\n"
		dir := filepath.Join(home, ".config", "systemd", "user")
		text := "# capstan-work.service\n" + service + "\n# capstan-work.timer\n" + timer
		return WorkSchedule{Text: text, Files: map[string]string{filepath.Join(dir, "capstan-work.service"): service, filepath.Join(dir, "capstan-work.timer"): timer}}, nil
	default:
		return WorkSchedule{}, errors.New("work setup supports launchd on macOS and systemd on Linux")
	}
}

// InstallWorkSchedule writes only Capstan's dedicated user schedule files.
func InstallWorkSchedule(platform string, schedule WorkSchedule) error {
	if platform == "" {
		platform = runtime.GOOS
	}
	for path, content := range schedule.Files {
		if err := atomicPrivateWrite(path, []byte(content)); err != nil {
			return errors.New("user schedule could not be installed")
		}
	}
	switch platform {
	case "darwin":
		var plist string
		for path := range schedule.Files {
			if strings.HasSuffix(path, ".plist") {
				plist = path
			}
		}
		if plist == "" {
			return errors.New("launchd schedule is incomplete")
		}
		current, err := user.Current()
		if err != nil || current.Uid == "" {
			return errors.New("launchd user domain could not be resolved")
		}
		domain := "gui/" + current.Uid
		_ = scheduleCommand("launchctl", "bootout", domain+"/com.artisan-build.capstan-work").Run()
		if err := scheduleCommand("launchctl", "bootstrap", domain, plist).Run(); err != nil {
			return errors.New("launchd schedule could not be activated")
		}
	case "linux":
		if err := scheduleCommand("systemctl", "--user", "daemon-reload").Run(); err != nil {
			return errors.New("systemd user configuration could not be reloaded")
		}
		if err := scheduleCommand("systemctl", "--user", "enable", "--now", "capstan-work.timer").Run(); err != nil {
			return errors.New("systemd timer could not be activated")
		}
	default:
		return errors.New("work setup supports launchd on macOS and systemd on Linux")
	}
	return nil
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func systemdEscape(value string) string {
	return strings.NewReplacer("\\", "\\\\", " ", "\\x20", "%", "%%").Replace(value)
}
