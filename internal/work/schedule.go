package work

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

const scheduleOwnership = "capstan-work/v1"

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
	if err := validateWorkScheduleTargets(platform, schedule); err != nil {
		return err
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
		_ = exec.Command("launchctl", "bootout", domain+"/com.artisan-build.capstan-work").Run()
		if err := exec.Command("launchctl", "bootstrap", domain, plist).Run(); err != nil {
			return errors.New("launchd schedule could not be activated")
		}
	case "linux":
		if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
			return errors.New("systemd user configuration could not be reloaded")
		}
		if err := exec.Command("systemctl", "--user", "enable", "--now", "capstan-work.timer").Run(); err != nil {
			return errors.New("systemd timer could not be activated")
		}
	default:
		return errors.New("work setup supports launchd on macOS and systemd on Linux")
	}
	return nil
}

func validateWorkScheduleTargets(platform string, schedule WorkSchedule) error {
	if platform == "" {
		platform = runtime.GOOS
	}
	for path, content := range schedule.Files {
		if existing, err := os.ReadFile(path); err == nil && string(existing) != content {
			if !ownedScheduleFile(platform, path, existing) {
				return errors.New("refusing to replace an unrelated user schedule")
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("existing user schedule could not be inspected")
		}
	}
	return nil
}

func ownedScheduleFile(platform, path string, data []byte) bool {
	switch platform {
	case "darwin":
		if filepath.Base(path) != "com.artisan-build.capstan-work.plist" {
			return false
		}
		return ownedLaunchdPlist(data)
	case "linux":
		name := filepath.Base(path)
		if name != "capstan-work.service" && name != "capstan-work.timer" {
			return false
		}
		return ownedSystemdUnit(data, name)
	default:
		return false
	}
}

func ownedLaunchdPlist(data []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	foundRoot := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return foundRoot
		}
		if err != nil {
			return false
		}
		switch token := token.(type) {
		case xml.StartElement:
			if foundRoot || !xmlElementNamed(token.Name, "plist") || !ownedLaunchdPlistRoot(decoder, token) {
				return false
			}
			foundRoot = true
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return false
			}
		case xml.EndElement:
			return false
		}
	}
}

func ownedLaunchdPlistRoot(decoder *xml.Decoder, root xml.StartElement) bool {
	dicts := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		switch token := token.(type) {
		case xml.StartElement:
			if dicts != 0 || !xmlElementNamed(token.Name, "dict") || !ownedLaunchdDict(decoder, token) {
				return false
			}
			dicts++
		case xml.EndElement:
			return token.Name == root.Name && dicts == 1
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return false
			}
		}
	}
}

func ownedLaunchdDict(decoder *xml.Decoder, dict xml.StartElement) bool {
	expectingKey := true
	pendingKey := ""
	labelCount, managedCount := 0, 0
	validLabel, validManaged := false, false
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		switch token := token.(type) {
		case xml.StartElement:
			if expectingKey {
				if !xmlElementNamed(token.Name, "key") {
					return false
				}
				key, ok := xmlTextElement(decoder, token)
				if !ok {
					return false
				}
				pendingKey = key
				switch pendingKey {
				case "Label":
					labelCount++
					if labelCount != 1 {
						return false
					}
				case "CapstanManaged":
					managedCount++
					if managedCount != 1 {
						return false
					}
				}
				expectingKey = false
				continue
			}
			if xmlElementNamed(token.Name, "key") {
				return false
			}
			if pendingKey == "Label" && xmlElementNamed(token.Name, "string") {
				value, ok := xmlTextElement(decoder, token)
				if !ok {
					return false
				}
				validLabel = value == "com.artisan-build.capstan-work"
			} else if pendingKey == "CapstanManaged" && xmlElementNamed(token.Name, "string") {
				value, ok := xmlTextElement(decoder, token)
				if !ok {
					return false
				}
				validManaged = value == scheduleOwnership
			} else if !skipXMLElement(decoder) {
				return false
			}
			pendingKey = ""
			expectingKey = true
		case xml.EndElement:
			return token.Name == dict.Name && expectingKey && labelCount == 1 && validLabel && managedCount == 1 && validManaged
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return false
			}
		}
	}
}

func xmlElementNamed(name xml.Name, local string) bool {
	return name.Space == "" && name.Local == local
}

func xmlTextElement(decoder *xml.Decoder, element xml.StartElement) (string, bool) {
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		switch token := token.(type) {
		case xml.CharData:
			text.Write(token)
		case xml.EndElement:
			return text.String(), token.Name == element.Name
		case xml.Comment:
		default:
			return "", false
		}
	}
}

func skipXMLElement(decoder *xml.Decoder) bool {
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		switch token.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return true
}

func ownedSystemdUnit(data []byte, name string) bool {
	section := ""
	managed, identity, target := 0, 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return false
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if section == "Unit" && key == "X-Capstan-Managed" {
			managed++
			if value != scheduleOwnership {
				return false
			}
		}
		if section == "Unit" && key == "X-Capstan-Unit" {
			identity++
			if value != name {
				return false
			}
		}
		if name == "capstan-work.timer" && section == "Timer" && key == "Unit" {
			target++
			if value != "capstan-work.service" {
				return false
			}
		}
	}
	return managed == 1 && identity == 1 && (name != "capstan-work.timer" || target == 1)
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func systemdEscape(value string) string {
	return strings.NewReplacer("\\", "\\\\", " ", "\\x20", "%", "%%").Replace(value)
}
