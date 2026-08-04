package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveSetsCredentialsFilePermission0600(t *testing.T) {
	setXDGConfigHome(t)

	if err := Save(Credentials{Token: "test-token-123", Server: "https://capstan.test"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	path, err := Path()
	if err != nil {
		t.Fatalf("Path returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}

	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credentials file mode = %v, want 0600", got)
	}
}

func TestSaveLoadRoundTripTokenAndServer(t *testing.T) {
	setXDGConfigHome(t)

	want := Credentials{Token: "test-token-123", Server: "https://capstan.test"}
	if err := Save(want); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got != want {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestCredentialsPathUsesXDGConfigHome(t *testing.T) {
	configHome := setXDGConfigHome(t)

	if err := Save(Credentials{Token: "test-token-123", Server: "https://capstan.test"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	want := filepath.Join(configHome, "capstan", "credentials")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("credentials file was not written to XDG_CONFIG_HOME path: %v", err)
	}
}

func TestCredentialsPathFallsBackToHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	if err := Save(Credentials{Token: "test-token-123", Server: "https://capstan.test"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	want := filepath.Join(home, ".config", "capstan", "credentials")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("credentials file was not written to HOME fallback path: %v", err)
	}
}

func TestDeleteRemovesFileAndMissingFileIsNil(t *testing.T) {
	setXDGConfigHome(t)

	if err := Save(Credentials{Token: "test-token-123", Server: "https://capstan.test"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	path, err := Path()
	if err != nil {
		t.Fatalf("Path returned error: %v", err)
	}

	if err := Delete(); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials file exists after Delete or stat returned unexpected error: %v", err)
	}

	if err := Delete(); err != nil {
		t.Fatalf("second Delete returned error: %v", err)
	}
}

func TestLoadMissingFileReturnsErrNotLoggedIn(t *testing.T) {
	setXDGConfigHome(t)

	_, err := Load()
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Load error = %v, want ErrNotLoggedIn", err)
	}
}

func TestCredentialsFormattingRedactsToken(t *testing.T) {
	creds := Credentials{Token: "test-token-123", Server: "https://capstan.test"}

	for _, formatted := range []string{fmt.Sprintf("%v", creds), fmt.Sprintf("%#v", creds)} {
		if strings.Contains(formatted, creds.Token) {
			t.Fatalf("formatted credentials leaked token: %s", formatted)
		}

		if !strings.Contains(formatted, "[REDACTED]") {
			t.Fatalf("formatted credentials did not contain redaction marker: %s", formatted)
		}
	}
}

func setXDGConfigHome(t *testing.T) string {
	t.Helper()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", t.TempDir())

	return configHome
}
