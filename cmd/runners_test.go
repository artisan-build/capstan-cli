package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/artisan-build/capstan-cli/internal/runners"
)

func TestRunnersCheckEndToEndLifecycleAndSuccessfulFinding(t *testing.T) {
	inbox := t.TempDir()
	state := t.TempDir()
	configPath := writeRunnersCommandConfig(t, inbox, state, []commandTestRunner{{
		Name: "worker", Label: "test.worker",
	}})
	fake := &runners.FakeReader{HealthByRunner: map[string]runners.Health{
		"worker": {Present: true, HasRun: true, LastExitStatus: 17, Runs: 3},
	}}
	checkedAt := time.Date(2026, 8, 18, 9, 14, 22, 0, time.UTC)
	now := func() time.Time { return checkedAt }
	fingerprint := runners.Fingerprint("worker", runners.FailureNonzeroExit)
	filename := "worker-nonzero_exit-" + fingerprint[:8] + ".md"

	stdout, stderr, err := executeRunnersCommand([]string{"runners", "check", "--config", configPath}, fake, now)
	if err != nil {
		t.Fatalf("first runners check error = %v", err)
	}
	if want := "worker: nonzero_exit (written " + filename + ")\n"; stdout != want {
		t.Fatalf("first stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("first stderr = %q, want empty", stderr)
	}
	assertCommandInboxEntries(t, inbox, filename)

	checkedAt = checkedAt.Add(2 * time.Hour)
	stdout, stderr, err = executeRunnersCommand([]string{"runners", "check", "--config", configPath}, fake, now)
	if err != nil {
		t.Fatalf("second runners check error = %v", err)
	}
	if want := "worker: nonzero_exit (updated " + filename + ")\n"; stdout != want {
		t.Fatalf("second stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("second stderr = %q, want empty", stderr)
	}
	assertCommandInboxEntries(t, inbox, filename)
	assertCommandFrontmatterContains(t, filepath.Join(inbox, filename), []string{
		"first_seen: \"2026-08-18T09:14:22Z\"",
		"last_seen: \"2026-08-18T11:14:22Z\"",
		"count: 2",
		"status: open",
	}, []string{"status: resolved"})

	checkedAt = checkedAt.Add(time.Hour)
	fake.HealthByRunner["worker"] = runners.Health{Present: true, HasRun: true, Runs: 4}
	stdout, stderr, err = executeRunnersCommand([]string{"runners", "check", "--config", configPath}, fake, now)
	if err != nil {
		t.Fatalf("recovery runners check error = %v", err)
	}
	if want := "worker: healthy (resolved " + filename + ")\n"; stdout != want {
		t.Fatalf("recovery stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("recovery stderr = %q, want empty", stderr)
	}
	assertCommandFrontmatterContains(t, filepath.Join(inbox, filename), []string{
		"status: resolved",
		"resolved_at: \"2026-08-18T12:14:22Z\"",
		"count: 2",
	}, []string{"status: open"})
	assertCommandInboxEntries(t, inbox, filename)

	checkedAt = checkedAt.Add(time.Hour)
	fake.HealthByRunner["worker"] = runners.Health{Present: true, HasRun: true, LastExitStatus: 9, Runs: 5}
	stdout, stderr, err = executeRunnersCommand([]string{"runners", "check", "--config", configPath}, fake, now)
	if err != nil {
		t.Fatalf("recurrence runners check error = %v", err)
	}
	if want := "worker: nonzero_exit (reopened " + filename + ")\n"; stdout != want {
		t.Fatalf("recurrence stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("recurrence stderr = %q, want empty", stderr)
	}
	assertCommandFrontmatterContains(t, filepath.Join(inbox, filename), []string{
		"status: open",
		"resolved_at: \"\"",
		"first_seen: \"2026-08-18T09:14:22Z\"",
		"last_seen: \"2026-08-18T13:14:22Z\"",
		"count: 3",
	}, []string{"status: resolved"})
	assertCommandInboxEntries(t, inbox, filename)

	wantReads := []runners.Runner{
		{Name: "worker", Type: runners.RunnerTypeLaunchd, Label: "test.worker"},
		{Name: "worker", Type: runners.RunnerTypeLaunchd, Label: "test.worker"},
		{Name: "worker", Type: runners.RunnerTypeLaunchd, Label: "test.worker"},
		{Name: "worker", Type: runners.RunnerTypeLaunchd, Label: "test.worker"},
	}
	if got := fake.Reads(); !reflect.DeepEqual(got, wantReads) {
		t.Fatalf("FakeReader reads = %#v, want %#v", got, wantReads)
	}
}

func TestRunnersCheckHealthyFromStartCreatesNoInboxFile(t *testing.T) {
	inbox := t.TempDir()
	state := t.TempDir()
	configPath := writeRunnersCommandConfig(t, inbox, state, []commandTestRunner{{Name: "healthy", Label: "test.healthy"}})
	fake := &runners.FakeReader{HealthByRunner: map[string]runners.Health{
		"healthy": {Present: true, HasRun: true, Runs: 1},
	}}

	stdout, stderr, err := executeRunnersCommand(
		[]string{"runners", "check", "--config", configPath},
		fake,
		func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) },
	)
	if err != nil {
		t.Fatalf("runners check error = %v", err)
	}
	if stdout != "healthy: healthy (no inbox change)\n" || stderr != "" {
		t.Fatalf("stdout, stderr = %q, %q, want healthy summary and empty stderr", stdout, stderr)
	}
	assertCommandInboxEntries(t, inbox)
	if _, err := os.Stat(filepath.Join(state, "runners.lock")); err != nil {
		t.Fatalf("persistent state lock path missing after successful check: %v", err)
	}
	lock, err := runners.NewStateStore(state).AcquireLock()
	if err != nil {
		t.Fatalf("state lock remained held after successful check: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("release post-check state lock: %v", err)
	}
}

func TestRunnersCheckDetectsSilentlyDeadRunnerFromPersistedState(t *testing.T) {
	inbox := t.TempDir()
	state := t.TempDir()
	configPath := writeRunnersCommandConfig(t, inbox, state, []commandTestRunner{{
		Name: "scheduled", Label: "test.scheduled", ExpectedCadence: "1h",
	}})
	fake := &runners.FakeReader{HealthByRunner: map[string]runners.Health{
		"scheduled": {Present: true, HasRun: true, Runs: 10},
	}}
	checkedAt := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return checkedAt }

	stdout, stderr, err := executeRunnersCommand([]string{"runners", "check", "--config", configPath}, fake, now)
	if err != nil {
		t.Fatalf("cold-start runners check error = %v", err)
	}
	if stdout != "scheduled: healthy (no inbox change)\n" || stderr != "" {
		t.Fatalf("cold-start stdout, stderr = %q, %q, want healthy summary and empty stderr", stdout, stderr)
	}
	assertCommandInboxEntries(t, inbox)

	checkedAt = checkedAt.Add(2*time.Hour + time.Second)
	fingerprint := runners.Fingerprint("scheduled", runners.FailureStale)
	filename := "scheduled-stale-" + fingerprint[:8] + ".md"
	stdout, stderr, err = executeRunnersCommand([]string{"runners", "check", "--config", configPath}, fake, now)
	if err != nil {
		t.Fatalf("stale runners check error = %v", err)
	}
	if want := "scheduled: stale (written " + filename + ")\n"; stdout != want || stderr != "" {
		t.Fatalf("stale stdout, stderr = %q, %q, want %q and empty stderr", stdout, stderr, want)
	}
	assertCommandInboxEntries(t, inbox, filename)
	assertCommandFrontmatterContains(t, filepath.Join(inbox, filename), []string{
		"failure_kind: stale",
		"freshness_source: runs",
		"run_count: 10",
		"last_activity: \"2026-08-18T09:00:00Z\"",
		"stale_after: \"2026-08-18T11:00:00Z\"",
	}, nil)
}

func TestRunnersCheckReaderErrorPreservesOtherRunnerStateAndFindings(t *testing.T) {
	inbox := t.TempDir()
	state := t.TempDir()
	configPath := writeRunnersCommandConfig(t, inbox, state, []commandTestRunner{
		{Name: "reader-error", Label: "test.reader-error"},
		{Name: "scheduled", Label: "test.scheduled", ExpectedCadence: "1h"},
	})
	readerErr := errors.New("launchctl transport unavailable")
	fake := &runners.FakeReader{
		HealthByRunner: map[string]runners.Health{
			"scheduled": {Present: true, HasRun: true, Runs: 3},
		},
		ErrorsByRunner: map[string]error{"reader-error": readerErr},
	}
	checkedAt := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return checkedAt }

	stdout, stderr, err := executeRunnersCommand(
		[]string{"runners", "check", "--config", configPath},
		fake,
		now,
	)
	if !errors.Is(err, readerErr) {
		t.Fatalf("runners check error = %v, want wrapped reader error", err)
	}
	if !strings.Contains(err.Error(), `read health for runner "reader-error"`) {
		t.Fatalf("reader error = %q, want monitor context", err)
	}
	if stdout != "scheduled: healthy (no inbox change)\n" {
		t.Fatalf("stdout = %q, want successful runner summary despite reader error", stdout)
	}
	if !strings.Contains(stderr, `read health for runner "reader-error"`) {
		t.Fatalf("stderr = %q, want monitor error", stderr)
	}
	assertCommandInboxEntries(t, inbox)
	loaded, err := runners.NewStateStore(state).Load()
	if err != nil {
		t.Fatalf("Load() after partial check error = %v", err)
	}
	wantState := runners.RunnerState{Runs: 3, RunsChangedAt: checkedAt, FirstSeenAt: checkedAt}
	if got := loaded.Runners["scheduled"]; got != wantState {
		t.Fatalf("saved scheduled state = %#v, want %#v", got, wantState)
	}

	checkedAt = checkedAt.Add(2*time.Hour + time.Second)
	fingerprint := runners.Fingerprint("scheduled", runners.FailureStale)
	filename := "scheduled-stale-" + fingerprint[:8] + ".md"
	stdout, stderr, err = executeRunnersCommand([]string{"runners", "check", "--config", configPath}, fake, now)
	if !errors.Is(err, readerErr) {
		t.Fatalf("second runners check error = %v, want wrapped reader error", err)
	}
	if want := "scheduled: stale (written " + filename + ")\n"; stdout != want {
		t.Fatalf("second stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, `read health for runner "reader-error"`) {
		t.Fatalf("second stderr = %q, want monitor error", stderr)
	}
	assertCommandInboxEntries(t, inbox, filename)
	wantReads := []runners.Runner{
		{Name: "reader-error", Type: runners.RunnerTypeLaunchd, Label: "test.reader-error"},
		{Name: "scheduled", Type: runners.RunnerTypeLaunchd, Label: "test.scheduled", ExpectedCadence: time.Hour, StalenessThreshold: time.Hour},
		{Name: "reader-error", Type: runners.RunnerTypeLaunchd, Label: "test.reader-error"},
		{Name: "scheduled", Type: runners.RunnerTypeLaunchd, Label: "test.scheduled", ExpectedCadence: time.Hour, StalenessThreshold: time.Hour},
	}
	if got := fake.Reads(); !reflect.DeepEqual(got, wantReads) {
		t.Fatalf("FakeReader reads = %#v, want every runner on both checks %#v", got, wantReads)
	}
}

func TestRunnersCheckIsolatesCorruptInboxItem(t *testing.T) {
	inbox := t.TempDir()
	state := t.TempDir()
	configPath := writeRunnersCommandConfig(t, inbox, state, []commandTestRunner{
		{Name: "aaa-corrupt", Label: "test.corrupt"},
		{Name: "bbb-missing", Label: "test.missing"},
	})
	corruptFingerprint := runners.Fingerprint("aaa-corrupt", runners.FailureNonzeroExit)
	corruptFilename := "aaa-corrupt-nonzero_exit-" + corruptFingerprint[:8] + ".md"
	corruptPath := filepath.Join(inbox, corruptFilename)
	corruptContents := []byte("---\nfingerprint: [unterminated\n---\nHuman notes must remain.\n")
	if err := os.WriteFile(corruptPath, corruptContents, 0o600); err != nil {
		t.Fatalf("write corrupt inbox item: %v", err)
	}
	fake := &runners.FakeReader{HealthByRunner: map[string]runners.Health{
		"aaa-corrupt": {Present: true, HasRun: true, LastExitStatus: 17, Runs: 1},
		"bbb-missing": {Present: false},
	}}

	stdout, stderr, err := executeRunnersCommand(
		[]string{"runners", "check", "--config", configPath},
		fake,
		func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) },
	)
	if err == nil || !strings.Contains(err.Error(), "leaving it unchanged") {
		t.Fatalf("runners check error = %v, want isolated corrupt-item error", err)
	}
	missingFingerprint := runners.Fingerprint("bbb-missing", runners.FailureNotLoaded)
	missingFilename := "bbb-missing-not_loaded-" + missingFingerprint[:8] + ".md"
	if want := "bbb-missing: not_loaded (written " + missingFilename + ")\n"; stdout != want {
		t.Fatalf("stdout = %q, want unaffected runner summary %q", stdout, want)
	}
	if !strings.Contains(stderr, corruptFilename) || !strings.Contains(stderr, "leaving it unchanged") {
		t.Fatalf("stderr = %q, want named corrupt-item error", stderr)
	}
	after, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Fatalf("read corrupt item after check: %v", err)
	}
	if !bytes.Equal(after, corruptContents) {
		t.Fatalf("corrupt item changed\n got: %q\nwant: %q", after, corruptContents)
	}
	assertCommandInboxEntries(t, inbox, corruptFilename, missingFilename)
}

func TestRunnersCheckContendedLockIsOperationalFailure(t *testing.T) {
	inbox := t.TempDir()
	state := t.TempDir()
	configPath := writeRunnersCommandConfig(t, inbox, state, []commandTestRunner{{Name: "worker", Label: "test.worker"}})
	lock, err := runners.NewStateStore(state).AcquireLock()
	if err != nil {
		t.Fatalf("seed AcquireLock() error = %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	fake := &runners.FakeReader{HealthByRunner: map[string]runners.Health{
		"worker": {Present: true, HasRun: true, LastExitStatus: 17, Runs: 3},
	}}

	stdout, stderr, err := executeRunnersCommand(
		[]string{"runners", "check", "--config", configPath},
		fake,
		func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) },
	)
	if err == nil || !strings.Contains(err.Error(), "already held") {
		t.Fatalf("contended runners check error = %v, want clear lock contention", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty on lock contention", stdout)
	}
	if !strings.Contains(stderr, "already held") {
		t.Fatalf("stderr = %q, want lock contention", stderr)
	}
	if got := fake.Reads(); len(got) != 0 {
		t.Fatalf("FakeReader reads = %#v, want none while lock is held", got)
	}
	assertCommandInboxEntries(t, inbox)
}

func TestRunnerLaunchdDocumentationIsOperational(t *testing.T) {
	t.Parallel()

	plist, err := os.ReadFile(filepath.Join("..", "docs", "launchd", "com.artisan-build.capstan-runners-check.plist"))
	if err != nil {
		t.Fatalf("read sample plist: %v", err)
	}
	plistText := string(plist)
	for _, want := range []string{
		"/opt/homebrew/bin/capstan",
		"<key>StandardOutPath</key>",
		"/tmp/capstan-runners-check.stdout.log",
		"<key>StandardErrorPath</key>",
		"/tmp/capstan-runners-check.stderr.log",
	} {
		if !strings.Contains(plistText, want) {
			t.Errorf("sample plist does not contain %q", want)
		}
	}
	if strings.Contains(plistText, "/usr/local/bin/capstan") {
		t.Error("sample plist still defaults to the Intel Homebrew path")
	}

	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readmeText := string(readme)
	for _, want := range []string{
		`mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Herd/brain/inbox" "$HOME/.local/state/capstan"`,
		"frontmatter `evidence` mapping carries the current observed values",
		"/tmp/capstan-runners-check.stderr.log",
	} {
		if !strings.Contains(readmeText, want) {
			t.Errorf("README does not contain %q", want)
		}
	}
}

type commandTestRunner struct {
	Name            string
	Label           string
	ExpectedCadence string
}

func executeRunnersCommand(args []string, reader runners.HealthReader, now func() time.Time) (string, string, error) {
	command := newRootCommandWithRunners(reader, now)
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs(args)
	err := command.Execute()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
	}

	return stdout.String(), stderr.String(), err
}

func writeRunnersCommandConfig(t *testing.T, inbox, state string, configured []commandTestRunner) string {
	t.Helper()

	var content strings.Builder
	_, _ = fmt.Fprintf(&content, "inbox:\n  path: %q\nstate:\n  path: %q\nrunners:\n", inbox, state)
	for _, runner := range configured {
		_, _ = fmt.Fprintf(&content, "  - name: %q\n    type: launchd\n    label: %q\n", runner.Name, runner.Label)
		if runner.ExpectedCadence != "" {
			_, _ = fmt.Fprintf(&content, "    expected_cadence: %q\n", runner.ExpectedCadence)
		}
	}
	path := filepath.Join(t.TempDir(), "runners.yaml")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write command config: %v", err)
	}

	return path
}

func assertCommandInboxEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(inbox) error = %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inbox entries = %#v, want %#v", got, want)
	}
}

func assertCommandFrontmatterContains(t *testing.T, path string, want, notWant []string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(inbox item) error = %v", err)
	}
	frontmatter, _, ok := strings.Cut(string(data), "---\n\n#")
	if !ok {
		t.Fatalf("inbox item has no frontmatter/body boundary: %s", data)
	}
	for _, value := range want {
		if !strings.Contains(frontmatter, value) {
			t.Errorf("frontmatter does not contain %q:\n%s", value, frontmatter)
		}
	}
	for _, value := range notWant {
		if strings.Contains(frontmatter, value) {
			t.Errorf("frontmatter unexpectedly contains %q:\n%s", value, frontmatter)
		}
	}
}
