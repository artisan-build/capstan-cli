package work

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// cappedAmpWriter discards excess output without allowing the stream to fill disk.
type cappedAmpWriter struct {
	mu        sync.Mutex
	w         io.Writer
	remaining int
}

func (w *cappedAmpWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if w.remaining > 0 {
		keep := min(len(p), w.remaining)
		if _, err := w.w.Write(p[:keep]); err != nil {
			return 0, err
		}
		w.remaining -= keep
	}
	return n, nil
}

type nativeAmpCommands struct {
	helperCommand  func(string, ...string) *exec.Cmd
	runCommand     func(context.Context, string, ...string) *exec.Cmd
	startupTimeout time.Duration
}

func (n nativeAmpCommands) Run(ctx context.Context, path string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := n.runCommand
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, path, args...)
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &cappedAmpWriter{w: &stdout, remaining: maxAmpJSON + 1}
	cmd.Stderr = &cappedAmpWriter{w: &stderr, remaining: maxAmpJSON + 1}
	runErr := cmd.Run()
	if stdout.Len() > maxAmpJSON || stderr.Len() > maxAmpJSON {
		return nil, errors.New("amp output exceeded limit")
	}
	if runErr != nil {
		if len(args) == 4 && args[0] == "projects" && args[1] == "get" &&
			bytes.Contains(stderr.Bytes(), []byte(`No accessible Amp project matches "`+args[2]+`"`)) {
			return nil, errAmpProjectAbsent
		}
		return nil, errors.New("amp command failed")
	}
	return stdout.Bytes(), nil
}

func (n nativeAmpCommands) Start(ctx context.Context, path string, args []string, dir, nonce string) ([]byte, error) {
	if !validUUIDv7(nonce) {
		return nil, errors.New("invalid Amp dispatch nonce")
	}
	base := filepath.Join(dir, "amp-"+nonce)
	initPath := base + ".init.json"
	if data, err := os.ReadFile(initPath); err == nil {
		if _, err := ampInitThread(data); err != nil {
			return nil, err
		}
		return data, nil
	}
	// A start marker prevents an unobserved dispatch from silently becoming a second orb.
	marker, err := os.OpenFile(base+".started", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		if err := marker.Sync(); err != nil {
			_ = marker.Close()
			return nil, errors.New("amp start marker could not be saved")
		}
		_ = marker.Close()
		if err := syncDirectory(dir); err != nil {
			return nil, errors.New("amp start marker could not be saved")
		}
		self, err := os.Executable()
		if err != nil {
			return nil, errors.New("amp helper executable unavailable")
		}
		command := n.helperCommand
		if command == nil {
			command = exec.Command
		}
		cmd := command(self, append([]string{"__work-amp-stream", path, base}, args...)...)
		configureProcess(cmd)
		if cmd.Start() != nil {
			return nil, errors.New("amp helper could not start")
		}
		_ = cmd.Process.Release()
	} else if !errors.Is(err, os.ErrExist) {
		return nil, errors.New("amp start marker unavailable")
	}
	timeout := n.startupTimeout
	if timeout == 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if data, err := os.ReadFile(initPath); err == nil {
			if _, err := ampInitThread(data); err != nil {
				return nil, err
			}
			return data, nil
		}
		select {
		case <-ctx.Done():
			return nil, errAmpDispatchUncertain
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// RunAmpStream is the detached private stream helper, not a public launch policy.
func RunAmpStream(args []string) error {
	if len(args) < 3 || !filepath.IsAbs(args[0]) || !filepath.IsAbs(args[1]) {
		return errors.New("invalid Amp stream helper arguments")
	}
	base := args[1]
	stdout, err := os.OpenFile(base+".stdout", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("amp private output unavailable")
	}
	defer func() { _ = stdout.Close() }()
	stderr, err := os.OpenFile(base+".stderr", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("amp private output unavailable")
	}
	defer func() { _ = stderr.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[2:]...)
	cmd.WaitDelay = time.Second
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("amp stream unavailable")
	}
	cmd.Stderr = &cappedAmpWriter{w: stderr, remaining: maxAmpJSON}
	if cmd.Start() != nil {
		return errors.New("amp stream could not start")
	}
	// A timer bounds the first frame independently of the detached turn lifetime.
	timer := time.AfterFunc(time.Minute, cancel)
	reader := bufio.NewReaderSize(pipe, maxAmpJSON+1)
	line, readErr := reader.ReadSlice('\n')
	if readErr != nil || len(line) > maxAmpJSON {
		timer.Stop()
		cancel()
		_ = cmd.Wait()
		return errors.New("amp initial frame unavailable")
	}
	if _, err := ampInitThread(line); err != nil {
		timer.Stop()
		cancel()
		_ = cmd.Wait()
		return err
	}
	if err := atomicPrivateWrite(base+".init.json", line); err != nil {
		timer.Stop()
		cancel()
		_ = cmd.Wait()
		return errors.New("amp initial frame could not be saved")
	}
	timer.Stop()
	w := &cappedAmpWriter{w: stdout, remaining: maxResponseBytes}
	_, _ = w.Write(line)
	_, copyErr := io.Copy(w, reader)
	waitErr := cmd.Wait()
	if copyErr != nil || waitErr != nil {
		return errors.New("amp stream ended unsuccessfully")
	}
	return nil
}
