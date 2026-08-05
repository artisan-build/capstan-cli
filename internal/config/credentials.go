package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotLoggedIn is returned when no saved credentials exist.
var ErrNotLoggedIn = errors.New("not logged in")

// ErrNoServer is returned when no server is configured.
var ErrNoServer = errors.New("no server configured")

// Credentials are the persisted Capstan server credentials.
type Credentials struct {
	Token  string `json:"token"`
	Server string `json:"server"`
}

// String redacts the token if credentials are formatted accidentally.
func (c Credentials) String() string {
	return fmt.Sprintf(`{Token:%q Server:%q}`, "[REDACTED]", c.Server)
}

// GoString redacts the token for %#v formatting.
func (c Credentials) GoString() string {
	return fmt.Sprintf(`config.Credentials{Token:%q, Server:%q}`, "[REDACTED]", c.Server)
}

// CleanServer trims whitespace and trailing slashes from a server base URL.
func CleanServer(server string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/")
}

// ResolveServer returns the server URL using flag > env > stored credentials precedence.
func ResolveServer(flagServer string) (string, error) {
	if server := CleanServer(flagServer); server != "" {
		return validateServer(server)
	}

	if server := CleanServer(os.Getenv("CAPSTAN_SERVER")); server != "" {
		return validateServer(server)
	}

	creds, err := Load()
	if err == nil {
		if server := CleanServer(creds.Server); server != "" {
			return validateServer(server)
		}
	} else if !errors.Is(err, ErrNotLoggedIn) {
		return "", err
	}

	return "", ErrNoServer
}

// ValidateServer validates a cleaned server base URL.
func ValidateServer(server string) (string, error) {
	return validateServer(server)
}

func validateServer(server string) (string, error) {
	parsed, err := url.Parse(server)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid server URL %q", server)
	}

	if parsed.Scheme == "https" {
		return server, nil
	}

	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		if host == "127.0.0.1" || host == "localhost" || host == "::1" {
			return server, nil
		}
	}

	return "", fmt.Errorf("invalid server URL %q: use https unless targeting localhost", server)
}

// Path returns the credentials file path for the current environment.
func Path() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("HOME is not set")
		}

		configHome = filepath.Join(home, ".config")
	}

	return filepath.Join(configHome, "capstan", "credentials"), nil
}

// Save writes credentials atomically using user-only file permissions.
func Save(creds Credentials) error {
	path, err := Path()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}

	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return err
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	committed = true

	return nil
}

// Load reads saved credentials.
func Load() (Credentials, error) {
	path, err := Path()
	if err != nil {
		return Credentials{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, ErrNotLoggedIn
		}

		return Credentials{}, err
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return Credentials{}, fmt.Errorf("read credentials: %w", err)
	}

	return creds, nil
}

// Delete removes saved credentials. Missing credentials are not an error.
func Delete() error {
	path, err := Path()
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}
