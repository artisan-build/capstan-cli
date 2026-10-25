package work

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testAmpThread = "T-01a1734d-7a34-73f8-b2c8-1eeed10a1d14"
const testAmpProject = "00000000-0000-4000-8000-000000000001"
const testRepository = "example/work"

// Shape taken from the coordinator's redacted observed projects get fixture.
func ampProjectJSON() []byte {
	return []byte(`{"id":"` + testAmpProject + `","owner":{"type":"user","userID":"user_fixture"},"name":"work","namespace":"user_fixture","description":"","iconURL":null,"listedOnAmpStore":false,"remixable":false,"galleryMedia":[],"desktopResumeScript":"","preCloneScript":"","preSetupScript":"","allowVirtualExecutor":false,"repositoryURL":"https://github.com/example/work","repositoryMode":"mapped","orbCommitAuthor":"amp","baseBranch":"","remoteURLs":["https://github.com/example/work"],"environmentResource":"a1.small","url":"https://ampcode.com/@user_fixture/work"}`)
}

func ampInitJSON() []byte {
	return []byte(`{"type":"system","subtype":"init","session_id":"` + testAmpThread + `","agent_mode":"claude-code","cwd":"/workspace","mcp_servers":[],"subagents":[],"tools":[]}` + "\n")
}

type ampCommandCall struct {
	path       string
	args       []string
	start      bool
	dir, nonce string
}
type fakeAmpCommands struct {
	calls []ampCommandCall
	run   func([]string) ([]byte, error)
	start func([]string) ([]byte, error)
}

func (f *fakeAmpCommands) Run(_ context.Context, path string, args []string) ([]byte, error) {
	f.calls = append(f.calls, ampCommandCall{path: path, args: append([]string(nil), args...)})
	if f.run != nil {
		return f.run(args)
	}
	if args[0] == "--version" {
		return []byte("0.0.1792929650-g000512 (2026-10-25)\n"), nil
	}
	return ampProjectJSON(), nil
}
func (f *fakeAmpCommands) Start(_ context.Context, path string, args []string, dir, nonce string) ([]byte, error) {
	f.calls = append(f.calls, ampCommandCall{path: path, args: append([]string(nil), args...), start: true, dir: dir, nonce: nonce})
	if f.start != nil {
		return f.start(args)
	}
	return ampInitJSON(), nil
}

func testAmpRef(t *testing.T) string {
	t.Helper()
	ref, err := (AmpSessionRef{ProjectID: testAmpProject, ThreadID: testAmpThread, ThreadURL: "https://ampcode.com/threads/" + testAmpThread}).String()
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestAmpVersionFloorAndMalformedOutput(t *testing.T) {
	for _, good := range []string{"0.0.1792929650", "0.0.1792929650-g000512 (2026-10-25)\n", "0.0.1792929651-gabc"} {
		if err := validateAmpVersion([]byte(good)); err != nil {
			t.Fatalf("valid version %q: %v", good, err)
		}
	}
	for _, bad := range []string{"0.0.1792929649", "0.0.01792929650", "amp 0.0.1792929650", "0.0.1792929650-gBAD", "0.0.1792929650\n0.0.1792929651", "0.0.1792929650garbage", strings.Repeat("x", maxAmpJSON+1)} {
		if err := validateAmpVersion([]byte(bad)); err == nil {
			t.Fatalf("accepted malformed/old version %q", bad[:min(len(bad), 80)])
		}
	}
}

func TestAmpCanonicalReferenceAndCrossFieldRefusal(t *testing.T) {
	value := testAmpRef(t)
	ref, err := ParseAmpSessionRef(value)
	if err != nil || ref.ProjectID != testAmpProject || ref.ThreadID != testAmpThread {
		t.Fatalf("parse: %#v %v", ref, err)
	}
	got, _ := ref.String()
	if got != value {
		t.Fatal("reference changed")
	}
	data, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "amp:v1:"))
	want := `{"project_id":"` + testAmpProject + `","thread_id":"` + testAmpThread + `","thread_url":"https://ampcode.com/threads/` + testAmpThread + `"}`
	if string(data) != want {
		t.Fatalf("wire JSON = %s", data)
	}
	for _, raw := range []string{
		strings.Replace(want, testAmpThread+`"}`, "T-other\"}", 1),
		strings.Replace(want, "https://ampcode.com/", "https://evil.example/", 1),
		strings.Replace(want, "https://ampcode.com/", "https://ampcode.com:443/", 1),
		strings.Replace(want, `"project_id":`, `"extra":1,"project_id":`, 1),
		strings.Replace(want, `"project_id":`, `"project_id":"other","project_id":`, 1),
		want + ` {}`, strings.Replace(want, `"project_id":"`+testAmpProject+`"`, `"project_id":null`, 1),
	} {
		if _, err := ParseAmpSessionRef("amp:v1:" + base64.RawURLEncoding.EncodeToString([]byte(raw))); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, value := range []string{value + "=", "amp:v1:!", "solo:v1:abc"} {
		if _, err := ParseAmpSessionRef(value); err == nil {
			t.Fatal("accepted bad reference")
		}
	}
}

func TestAmpDispatchExactArgvAndRemoteLifecycle(t *testing.T) {
	f := &fakeAmpCommands{}
	l := NewAmpLauncher("/opt/amp", f)
	spec := LaunchSpec{Role: "builder", Repository: testRepository, ItemID: testItemID, LaunchNonce: testNonce, WorkDir: t.TempDir()}
	ref, err := l.Launch(context.Background(), spec)
	if err != nil || ref != testAmpRef(t) {
		t.Fatalf("Launch = %q %v", ref, err)
	}
	pointer, _ := ampPointer(testItemID, testNonce)
	want := []string{"-ox", pointer, "--project", testRepository, "--mode", "claude-code", "--title", "Capstan work " + testItemID, "--stream-json", "--no-archive-after-execute"}
	if len(f.calls) != 3 || !reflect.DeepEqual(f.calls[2].args, want) || !f.calls[2].start || f.calls[2].dir != spec.WorkDir || f.calls[2].nonce != testNonce {
		t.Fatalf("calls = %#v", f.calls)
	}
	if len(pointer) > maxPointerBytes || !strings.Contains(pointer, "work_brief") || !strings.Contains(pointer, "Retry briefly") {
		t.Fatal("invalid pointer")
	}
	for _, call := range f.calls {
		if call.path != "/opt/amp" {
			t.Fatal("executable changed")
		}
		for _, arg := range call.args {
			for _, secret := range []string{"signature=", "Bearer ", "private prompt"} {
				if strings.Contains(arg, secret) {
					t.Fatal("dispatch leaked secret input")
				}
			}
		}
	}
	for i := 0; i < 3; i++ {
		if alive, err := l.Alive(context.Background(), ref); err != nil || !alive {
			t.Fatal("remote liveness followed local process")
		}
	}
	if err := l.Terminate(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := l.OpenGate(context.Background(), ref, "unused", testNonce); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatal("lifecycle called Amp")
	}
	if err := l.Continue(context.Background(), ref, "post progress"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.calls[4].args, []string{"threads", "continue", testAmpThread, "-ox", "post progress"}) {
		t.Fatalf("wrong continuation: %#v", f.calls[4])
	}
}

func TestAmpRefusesRepositoryAndExecutableInjectionBeforeArgv(t *testing.T) {
	for _, repo := range []string{"", "Owner/repo", "--help", "https://github.com/example/work", "a/b;touch x", "a/../b", "a/b\x00", "a/b c", "a-/b"} {
		f := &fakeAmpCommands{}
		l := NewAmpLauncher("/opt/amp", f)
		_, err := l.Launch(context.Background(), LaunchSpec{Role: "builder", Repository: repo, ItemID: testItemID, LaunchNonce: testNonce})
		if err == nil || len(f.calls) != 0 {
			t.Fatalf("repository reached argv: %q", repo)
		}
	}
	f := &fakeAmpCommands{}
	if err := NewAmpLauncher("amp", f).version(context.Background()); err == nil || len(f.calls) != 0 {
		t.Fatal("relative executable ran")
	}
	for _, repo := range []string{"a/.github", "a/a..b", "a/under_score", "a/-name"} {
		if !validRepository(repo) {
			t.Fatalf("canonical server repository rejected: %s", repo)
		}
	}
	for _, repo := range []string{"a--b/repo", "a/repo.git", "a/.", "a/..", strings.Repeat("a", 40) + "/b", "a/" + strings.Repeat("b", 101)} {
		if validRepository(repo) {
			t.Fatalf("noncanonical server repository accepted: %s", repo)
		}
	}
}

func TestAmpProjectGetCreateAndRaceAlwaysRegets(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			count := 0
			f := &fakeAmpCommands{run: func(args []string) ([]byte, error) {
				count++
				if count == 1 {
					return nil, errAmpProjectAbsent
				}
				if count == 2 {
					if !reflect.DeepEqual(args, []string{"projects", "create", "https://github.com/example/work", "--personal", "--json"}) {
						t.Fatalf("create args: %#v", args)
					}
					if race {
						return nil, errors.New("competing creator")
					}
					return []byte(`{"created":true}`), nil
				}
				return ampProjectJSON(), nil
			}}
			project, err := NewAmpLauncher("/opt/amp", f).project(context.Background(), testRepository)
			if err != nil || project.ID != testAmpProject || count != 3 || f.calls[2].args[1] != "get" {
				t.Fatalf("resolution = %#v %v, %d calls", project, err, count)
			}
		})
	}
	f := &fakeAmpCommands{run: func([]string) ([]byte, error) { return nil, errors.New("credential-secret") }}
	if _, err := NewAmpLauncher("/opt/amp", f).project(context.Background(), testRepository); err == nil || strings.Contains(err.Error(), "credential-secret") || len(f.calls) != 1 {
		t.Fatal("non-absence error created a project or leaked output")
	}
}

func TestAmpBoundedStrictProjectAndStreamParsing(t *testing.T) {
	valid := string(ampProjectJSON())
	for _, data := range []string{valid + ` {}`, strings.Replace(valid, `"id":`, `"unknown":1,"id":`, 1), strings.Replace(valid, `"id":`, `"id":"duplicate","id":`, 1), strings.Replace(valid, "https://github.com/example/work", "https://github.com/other/repo", 1), strings.Replace(valid, "https://ampcode.com/", "https://ampcode.com.evil/", 1), strings.Repeat(" ", maxAmpJSON+1), `{`, strings.Replace(valid, `"id":"`+testAmpProject+`"`, `"id":""`, 1)} {
		f := &fakeAmpCommands{run: func([]string) ([]byte, error) { return []byte(data), nil }}
		if _, err := NewAmpLauncher("/opt/amp", f).project(context.Background(), testRepository); err == nil {
			t.Fatalf("accepted malformed project: %s", data[:min(80, len(data))])
		}
	}
	for _, data := range []string{`{}`, string(ampInitJSON()) + string(ampInitJSON()), strings.Replace(string(ampInitJSON()), `"init"`, `"result"`, 1), strings.Replace(string(ampInitJSON()), testAmpThread, "T-bad", 1), strings.Repeat("x", maxAmpJSON+1), strings.Replace(string(ampInitJSON()), `"type":`, `"type":"other","type":`, 1)} {
		if _, err := ampInitThread([]byte(data)); err == nil {
			t.Fatal("accepted invalid stream event")
		}
	}
}

func TestAmpErrorsAndMessagesAreBoundedAndRedacted(t *testing.T) {
	f := &fakeAmpCommands{start: func([]string) ([]byte, error) { return nil, errors.New("credential-secret signature=private prompt") }}
	_, err := NewAmpLauncher("/opt/amp", f).Launch(context.Background(), LaunchSpec{Role: "builder", Repository: testRepository, ItemID: testItemID, LaunchNonce: testNonce, WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("failed command passed")
	}
	for _, secret := range []string{"credential-secret", "signature=", "private prompt"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error leaked output")
		}
	}
	for _, message := range []string{"", " ", "a\x00b", strings.Repeat("x", 8193)} {
		if err := NewAmpLauncher("/opt/amp", f).Continue(context.Background(), testAmpRef(t), message); err == nil {
			t.Fatal("unbounded message accepted")
		}
	}
	var output bytes.Buffer
	w := &cappedAmpWriter{w: &output, remaining: 5}
	if n, err := w.Write([]byte("123456789")); err != nil || n != 9 || output.String() != "12345" {
		t.Fatal("output was not capped")
	}
}

func TestAmpDispatchRecoveryRefusesChangedProjectIdentity(t *testing.T) {
	f := &fakeAmpCommands{}
	l := NewAmpLauncher("/opt/amp", f)
	spec := LaunchSpec{Role: "builder", Repository: testRepository, ItemID: testItemID, LaunchNonce: testNonce, WorkDir: t.TempDir()}
	if _, err := l.Launch(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	f.run = func(args []string) ([]byte, error) {
		if args[0] == "--version" {
			return []byte("0.0.1792929650"), nil
		}
		return []byte(strings.ReplaceAll(string(ampProjectJSON()), testAmpProject, "00000000-0000-4000-8000-000000000002")), nil
	}
	if _, err := l.Launch(context.Background(), spec); err == nil {
		t.Fatal("recovery substituted project identity")
	}
	for _, call := range f.calls {
		if call.start {
			t.Fatal("changed project reached dispatch")
		}
	}
}

// Disposable child executables exercise the actual detached helper without Amp/network/Solo.
func TestAmpExecutableChild(t *testing.T) {
	args := childAmpArgs()
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "stream":
		_, _ = os.Stdout.Write(ampInitJSON())
		time.Sleep(250 * time.Millisecond)
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxResponseBytes+100))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("e"), maxAmpJSON+100))
		if len(args) > 1 {
			_ = os.WriteFile(args[1], []byte("finished"), 0o600)
		}
	case "bad":
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"result"}`)
	case "huge":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxAmpJSON+2))
	case "output":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxAmpJSON+2))
	case "absent":
		_, _ = fmt.Fprintln(os.Stderr, `No accessible Amp project matches "example/work"`)
		os.Exit(1)
	}
	os.Exit(0)
}

func childAmpArgs() []string {
	for i, arg := range os.Args {
		if arg == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func TestAmpStreamHelperChild(t *testing.T) {
	args := childAmpArgs()
	if len(args) == 0 {
		return
	}
	if err := RunAmpStream(args[1:]); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestAmpDetachedHelperPersistsInitCapsOutputAndDoesNotRestart(t *testing.T) {
	root := t.TempDir()
	done := filepath.Join(root, "done")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	native := nativeAmpCommands{helperCommand: func(_ string, args ...string) *exec.Cmd {
		starts++
		return exec.Command(self, append([]string{"-test.run=^TestAmpStreamHelperChild$", "--"}, args...)...)
	}}
	args := []string{"-test.run=^TestAmpExecutableChild$", "--", "stream", done}
	data, err := native.Start(context.Background(), self, args, root, testNonce)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ampInitThread(data); err != nil {
		t.Fatal(err)
	}
	// The orb stream is still running when initialization has returned.
	if _, err := os.Stat(done); err == nil {
		t.Fatal("dispatch waited for the turn result")
	}
	if _, err := native.Start(context.Background(), self, args, root, testNonce); err != nil || starts != 1 {
		t.Fatal("recovery dispatched a replacement")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(done); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached process did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, suffix := range []string{".init.json", ".stdout", ".stderr"} {
		assertMode(t, filepath.Join(root, "amp-"+testNonce+suffix), 0o600)
	}
	for suffix, limit := range map[string]int64{".stdout": maxResponseBytes, ".stderr": maxAmpJSON} {
		info, err := os.Stat(filepath.Join(root, "amp-"+testNonce+suffix))
		if err != nil || info.Size() > limit {
			t.Fatalf("unbounded private output %s", suffix)
		}
	}
}

func TestAmpNativeCommandBoundsAndMissingProjectClassification(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	native := nativeAmpCommands{}
	for _, mode := range []string{"output", "absent"} {
		_, err := native.Run(context.Background(), self, []string{"-test.run=^TestAmpExecutableChild$", "--", mode})
		if err == nil {
			t.Fatal("failed/oversize command passed")
		}
		if strings.Contains(err.Error(), "xxxxx") {
			t.Fatal("command output leaked")
		}
	}
	native.runCommand = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if !reflect.DeepEqual(args, []string{"projects", "get", testRepository, "--json"}) {
			t.Fatalf("unexpected native command args: %#v", args)
		}
		return exec.CommandContext(ctx, self, "-test.run=^TestAmpExecutableChild$", "--", "absent")
	}
	if _, err := native.Run(context.Background(), "/opt/amp", []string{"projects", "get", testRepository, "--json"}); !errors.Is(err, errAmpProjectAbsent) {
		t.Fatalf("observed missing-project message not classified: %v", err)
	}
	native.runCommand = nil
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := native.Run(ctx, self, []string{"-test.run=^TestAmpExecutableChild$", "--", "stream"}); err == nil {
		t.Fatal("execution deadline did not stop native command")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "amp-"+testNonce+".started"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = (nativeAmpCommands{startupTimeout: time.Millisecond}).Start(context.Background(), self, nil, root, testNonce)
	if !errors.Is(err, errAmpDispatchUncertain) {
		t.Fatalf("uncertain dispatch = %v", err)
	}
}

func TestAmpStreamHelperRejectsMalformedAndOversizeInit(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"bad", "huge"} {
		base := filepath.Join(t.TempDir(), "stream")
		if err := RunAmpStream([]string{self, base, "-test.run=^TestAmpExecutableChild$", "--", mode}); err == nil {
			t.Fatal("bad initialization passed")
		}
		if _, err := os.Stat(base + ".init.json"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid init was published")
		}
	}
}
