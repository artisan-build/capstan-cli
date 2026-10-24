package work

import (
	"context"
	"errors"
	"os"
	"strings"
)

// Execute loads one config, takes its overlap lock, then reads the credential and runs one tick.
func Execute(ctx context.Context, configPath string, launcher Launcher, health HealthMeasurer, logger Logger) (returnErr error) {
	if configPath == "" {
		var err error
		configPath, err = DefaultConfigPath()
		if err != nil {
			return err
		}
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	lock, err := AcquireLock(config.LockFile)
	if errors.Is(err, ErrLockHeld) {
		if logger != nil {
			logger("another work drain run holds the lock; exiting successfully")
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Release()) }()

	bearer, err := readCredential(config.CredentialFile)
	if err != nil {
		return err
	}
	if launcher == nil {
		if config.Launcher == "solo" {
			var client *StdioSoloClient
			client, err = NewStdioSoloClient(ctx, config.SoloMCPPath, config.SoloCallTimeout)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, client.Close()) }()
			launcher = NewSoloLauncher(client, config.SoloReadyTimeout)
		} else {
			launcher, err = NewProcessLauncher()
			if err != nil {
				return err
			}
		}
	}
	if health == nil {
		health = SystemHealth{}
	}

	return NewRunner(config, NewClient(config, bearer), launcher, health, logger).Run(ctx)
}

func readCredential(path string) (string, error) {
	if err := requirePrivateRegularFile(path, "work credential"); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("work credential could not be read")
	}
	bearer := strings.TrimSpace(string(data))
	if bearer == "" || len(bearer) > 16384 || strings.IndexFunc(bearer, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return "", errors.New("work credential must contain one non-empty bearer without whitespace")
	}

	return bearer, nil
}
