package work

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ProcessLauncher is a non-interactive external-command adapter. Interactive repairs require PR7's launcher.
type ProcessLauncher struct {
	executable string
}

// NewProcessLauncher returns the generic gated external-command launcher.
func NewProcessLauncher() (*ProcessLauncher, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.New("capstan executable path could not be resolved")
	}

	return &ProcessLauncher{executable: executable}, nil
}

// Launch starts a gated helper and records its process session by idempotency key.
func (l *ProcessLauncher) Launch(ctx context.Context, spec LaunchSpec) (string, error) {
	_ = ctx
	if len(spec.Command) == 0 {
		return "", errors.New("launch command is empty")
	}
	marker := processMarker(spec.WorkDir, spec.LaunchKey)
	if data, err := os.ReadFile(marker); err == nil {
		var saved struct {
			SessionRef string `json:"session_ref"`
		}
		if json.Unmarshal(data, &saved) == nil {
			alive, aliveErr := l.Alive(ctx, saved.SessionRef)
			if aliveErr == nil && alive {
				return saved.SessionRef, nil
			}
		}
	}
	if err := os.Remove(spec.GatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("stale external command gate could not be removed")
	}
	prompt, err := os.Open(spec.PromptPath)
	if err != nil {
		return "", errors.New("private prompt file could not be opened")
	}
	defer func() { _ = prompt.Close() }()
	args := []string{"__work-gate", "--gate", spec.GatePath, "--workdir", spec.WorkDir, "--"}
	args = append(args, spec.Command...)
	command := exec.Command(l.executable, args...)
	command.Dir = spec.WorkDir
	command.Stdin = prompt
	command.Stdout = nil
	command.Stderr = nil
	configureProcess(command)
	if err := command.Start(); err != nil {
		return "", errors.New("external command helper could not start")
	}
	sessionRef := "process:" + strconv.Itoa(command.Process.Pid)
	data, _ := json.Marshal(struct {
		SessionRef string `json:"session_ref"`
	}{SessionRef: sessionRef})
	if err := atomicPrivateWrite(marker, append(data, '\n')); err != nil {
		_ = terminateProcess(command.Process.Pid)
		return "", errors.New("external command session marker could not be written")
	}
	_ = command.Process.Release()

	return sessionRef, nil
}

// Alive checks the process group represented by sessionRef.
func (l *ProcessLauncher) Alive(_ context.Context, sessionRef string) (bool, error) {
	pid, err := processID(sessionRef)
	if err != nil {
		return false, err
	}

	return processAlive(pid)
}

// Terminate stops the represented process group.
func (l *ProcessLauncher) Terminate(_ context.Context, sessionRef string) error {
	pid, err := processID(sessionRef)
	if err != nil {
		return err
	}

	return terminateProcess(pid)
}

// OpenGate atomically permits a prepared external command to run.
func (l *ProcessLauncher) OpenGate(_ context.Context, path string) error {
	return writePrivate(path, "ready\n")
}

// SendEnter is intentionally unsupported by the non-interactive process adapter.
func (l *ProcessLauncher) SendEnter(context.Context, string, string) error {
	return ErrUnsupportedAction
}

// ResendPointer is intentionally unsupported by the non-interactive process adapter.
func (l *ProcessLauncher) ResendPointer(context.Context, string, string, string) error {
	return ErrUnsupportedAction
}

// RunGate waits for the durable protocol gate, then runs the configured argv without a shell.
func RunGate(args []string) error {
	if len(args) < 6 || args[0] != "--gate" || args[2] != "--workdir" || args[4] != "--" || args[1] == "" || args[3] == "" {
		return errors.New("invalid internal work gate arguments")
	}
	gate, workDir, commandArgs := args[1], args[3], args[5:]
	if len(commandArgs) == 0 {
		return errors.New("invalid internal work gate command")
	}
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("internal work gate could not be inspected")
		}
		time.Sleep(50 * time.Millisecond)
	}
	command := exec.Command(commandArgs[0], commandArgs[1:]...)
	command.Dir = workDir
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("worker command exited: %w", err)
	}

	return nil
}

func processMarker(workDir, key string) string {
	digest := sha256.Sum256([]byte(key))
	return filepath.Join(workDir, fmt.Sprintf(".session-%x.json", digest[:8]))
}

func processID(sessionRef string) (int, error) {
	value, found := strings.CutPrefix(sessionRef, "process:")
	if !found {
		return 0, errors.New("external command session ref is invalid")
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid < 1 {
		return 0, errors.New("external command session ref is invalid")
	}

	return pid, nil
}
