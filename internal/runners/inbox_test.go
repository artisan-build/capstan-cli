package runners

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFingerprintIsStable(t *testing.T) {
	t.Parallel()

	const want = "29a971336c5736e5f49511bde0395968e7e230bc0db0a3bbe0ca79ec380da5e7"
	if got := Fingerprint("ballast-rescore", FailureStale); got != want {
		t.Fatalf("Fingerprint() = %q, want durable value %q", got, want)
	}
}

func TestInboxWriterStaleFileFormat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 8, 18, 11, 14, 22, 0, time.UTC)
	lastActivity := time.Date(2026, 8, 18, 8, 14, 22, 0, time.UTC)
	runner := Runner{
		Name:               "ballast-rescore",
		Type:               RunnerTypeLaunchd,
		Label:              "build.artisan.ballast-rescore",
		ExpectedCadence:    time.Hour,
		StalenessThreshold: 30 * time.Minute,
	}
	health := Health{Present: true, HasRun: true, Runs: 12}
	state := RunnerState{Runs: 12, RunsChangedAt: lastActivity, FirstSeenAt: lastActivity}
	classification := Classification{Runner: runner.Name, Kind: FailureStale}

	changes, err := NewInboxWriter(dir).Reconcile(runner, classification, health, state, now)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantChanges := []InboxChange{{Action: "written", Filename: "ballast-rescore-stale-29a97133.md"}}
	if !reflect.DeepEqual(changes, wantChanges) {
		t.Fatalf("Reconcile() changes = %#v, want %#v", changes, wantChanges)
	}

	data, err := os.ReadFile(filepath.Join(dir, wantChanges[0].Filename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	want := `---
fingerprint: 29a971336c5736e5f49511bde0395968e7e230bc0db0a3bbe0ca79ec380da5e7
runner: ballast-rescore
failure_kind: stale
status: open
assignee: ""
first_seen: "2026-08-18T11:14:22Z"
last_seen: "2026-08-18T11:14:22Z"
count: 1
resolved_at: ""
evidence:
    checked: last activity against expected cadence plus staleness threshold
    freshness_source: runs
    run_count: 12
    last_activity: "2026-08-18T08:14:22Z"
    expected_cadence: 1h0m0s
    staleness_threshold: 30m0s
    stale_after: "2026-08-18T09:44:22Z"
---

# Runner ballast-rescore is stale

The runner has not shown real activity within its expected cadence and staleness threshold.

## Evidence (first observed)
- Checked: last activity against expected cadence plus staleness threshold
- Freshness source: runs
- Run count: 12
- Last activity: 2026-08-18T08:14:22Z
- Expected cadence: 1h0m0s
- Staleness threshold: 30m0s
- Stale after: 2026-08-18T09:44:22Z
`
	if string(data) != want {
		t.Fatalf("inbox file mismatch\n--- got ---\n%s--- want ---\n%s", data, want)
	}
}

func TestInboxWriterDedupeLifecyclePreservesBody(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writer := NewInboxWriter(dir)
	runner := Runner{Name: "worker", Type: RunnerTypeLaunchd, Label: "test.worker"}
	failure := Classification{Runner: runner.Name, Kind: FailureNonzeroExit}
	health := Health{Present: true, HasRun: true, LastExitStatus: 17, Runs: 3}
	state := RunnerState{Runs: 3}
	firstSeen := time.Date(2026, 8, 18, 9, 14, 22, 0, time.UTC)
	secondSeen := firstSeen.Add(2 * time.Hour)
	recoveredAt := secondSeen.Add(time.Hour)
	recurredAt := recoveredAt.Add(time.Hour)
	filename := "worker-nonzero_exit-" + Fingerprint(runner.Name, FailureNonzeroExit)[:8] + ".md"
	path := filepath.Join(dir, filename)

	changes, err := writer.Reconcile(runner, failure, health, state, firstSeen)
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if want := []InboxChange{{Action: "written", Filename: filename}}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("first changes = %#v, want %#v", changes, want)
	}

	humanBody := "\n# Runner worker exited unsuccessfully\n\nHuman triage notes must survive.\n"
	handAuthored := fmt.Sprintf(`---
fingerprint: %s
runner: worker
failure_kind: nonzero_exit
status: open
assignee: human-owner
first_seen: "2026-08-18T09:14:22Z"
last_seen: "2026-08-18T09:14:22Z"
count: 1
resolved_at: ""
evidence:
  checked: original human fixture
  freshness_source: runs
owner: triage-team
triage_notes:
  context: preserve this nested map
---
%s`, Fingerprint(runner.Name, FailureNonzeroExit), humanBody)
	if err := os.WriteFile(path, []byte(handAuthored), 0o600); err != nil {
		t.Fatalf("write hand-authored inbox item: %v", err)
	}
	extra := map[string]any{
		"owner": "triage-team",
		"triage_notes": map[string]any{
			"context": "preserve this nested map",
		},
	}

	health.LastExitStatus = 23
	health.Runs = 4
	changes, err = writer.Reconcile(runner, failure, health, RunnerState{Runs: 4}, secondSeen)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if want := []InboxChange{{Action: "updated", Filename: filename}}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("second changes = %#v, want %#v", changes, want)
	}
	assertInboxState(t, path, inboxFrontmatter{
		Fingerprint: Fingerprint(runner.Name, FailureNonzeroExit),
		Runner:      runner.Name,
		FailureKind: FailureNonzeroExit,
		Status:      inboxStatusOpen,
		Assignee:    "human-owner",
		FirstSeen:   firstSeen.Format(time.RFC3339),
		LastSeen:    secondSeen.Format(time.RFC3339),
		Count:       2,
		Evidence: inboxEvidence{
			Checked:         "launchd last exit status, exit reason, and run count",
			FreshnessSource: "runs",
			LastExitStatus:  intPointer(23),
			RunCount:        intPointer(4),
		},
		Extra: extra,
	}, humanBody)
	assertOnlyInboxFile(t, dir, filename)

	healthy := Classification{Runner: runner.Name, Kind: FailureHealthy}
	changes, err = writer.Reconcile(runner, healthy, Health{Present: true, HasRun: true, Runs: 5}, RunnerState{Runs: 5}, recoveredAt)
	if err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	if want := []InboxChange{{Action: "resolved", Filename: filename}}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("recovery changes = %#v, want %#v", changes, want)
	}
	wantResolved := inboxFrontmatter{
		Fingerprint: Fingerprint(runner.Name, FailureNonzeroExit),
		Runner:      runner.Name,
		FailureKind: FailureNonzeroExit,
		Status:      inboxStatusResolved,
		Assignee:    "human-owner",
		FirstSeen:   firstSeen.Format(time.RFC3339),
		LastSeen:    secondSeen.Format(time.RFC3339),
		Count:       2,
		ResolvedAt:  recoveredAt.Format(time.RFC3339),
		Evidence: inboxEvidence{
			Checked:         "launchd last exit status, exit reason, and run count",
			FreshnessSource: "runs",
			LastExitStatus:  intPointer(23),
			RunCount:        intPointer(4),
		},
		Extra: extra,
	}
	assertInboxState(t, path, wantResolved, humanBody)
	assertOnlyInboxFile(t, dir, filename)

	health.LastExitStatus = 9
	health.Runs = 6
	changes, err = writer.Reconcile(runner, failure, health, RunnerState{Runs: 6}, recurredAt)
	if err != nil {
		t.Fatalf("recurrence Reconcile() error = %v", err)
	}
	if want := []InboxChange{{Action: "reopened", Filename: filename}}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("recurrence changes = %#v, want %#v", changes, want)
	}
	wantReopened := wantResolved
	wantReopened.Status = inboxStatusOpen
	wantReopened.LastSeen = recurredAt.Format(time.RFC3339)
	wantReopened.Count = 3
	wantReopened.ResolvedAt = ""
	wantReopened.Evidence.LastExitStatus = intPointer(9)
	wantReopened.Evidence.RunCount = intPointer(6)
	assertInboxState(t, path, wantReopened, humanBody)
	assertOnlyInboxFile(t, dir, filename)
}

func TestInboxWriterAcceptsHandEditedFilesAndPreservesBodies(t *testing.T) {
	t.Parallel()

	const fingerprintToken = "FINGERPRINT"
	const fingerprint = "162662436d2b73476c3c26eb2209b11e135ae6df138b7c87cf61090beeeaa293"
	tests := []struct {
		name        string
		contents    []byte
		body        []byte
		wantWarning string
		wantCount   int
		wantExtra   map[string]any
	}{
		{
			name:      "triple dash line in body",
			contents:  []byte("---\nfingerprint: FINGERPRINT\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: open\nassignee: human\nfirst_seen: 2026-08-18T09:14:22Z\nlast_seen: 2026-08-18T09:14:22Z\ncount: 1\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\n---\nNotes before\n---\nNotes after\n"),
			body:      []byte("Notes before\n---\nNotes after\n"),
			wantCount: 2,
		},
		{
			name:      "fenced YAML in body",
			contents:  []byte("---\nfingerprint: FINGERPRINT\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: open\nassignee: human\nfirst_seen: 2026-08-18T09:14:22Z\nlast_seen: 2026-08-18T09:14:22Z\ncount: 1\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\n---\n```yaml\nstatus: resolved\ncount: 99\n```\n"),
			body:      []byte("```yaml\nstatus: resolved\ncount: 99\n```\n"),
			wantCount: 2,
		},
		{
			name:      "CRLF only",
			contents:  []byte("---\r\nfingerprint: FINGERPRINT\r\nrunner: worker\r\nfailure_kind: nonzero_exit\r\nstatus: open\r\nassignee: human\r\nfirst_seen: 2026-08-18T09:14:22Z\r\nlast_seen: 2026-08-18T09:14:22Z\r\ncount: 1\r\nresolved_at: ''\r\nevidence:\r\n  checked: hand written\r\n  freshness_source: runs\r\n---\r\nHuman CRLF notes\r\nSecond line\r\n"),
			body:      []byte("Human CRLF notes\r\nSecond line\r\n"),
			wantCount: 2,
		},
		{
			name:      "no trailing newline",
			contents:  []byte("---\nfingerprint: FINGERPRINT\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: open\nassignee: human\nfirst_seen: 2026-08-18T09:14:22Z\nlast_seen: 2026-08-18T09:14:22Z\ncount: 1\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\n---\nno trailing newline"),
			body:      []byte("no trailing newline"),
			wantCount: 2,
		},
		{
			name:      "empty body",
			contents:  []byte("---\nfingerprint: FINGERPRINT\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: open\nassignee: human\nfirst_seen: 2026-08-18T09:14:22Z\nlast_seen: 2026-08-18T09:14:22Z\ncount: 1\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\n---"),
			body:      []byte{},
			wantCount: 2,
		},
		{
			name:      "unicode body",
			contents:  []byte("---\nfingerprint: FINGERPRINT\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: open\nassignee: human\nfirst_seen: 2026-08-18T09:14:22Z\nlast_seen: 2026-08-18T09:14:22Z\ncount: 1\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\n---\nTriage: café 東京 Δ\n"),
			body:      []byte("Triage: café 東京 Δ\n"),
			wantCount: 2,
		},
		{
			name:      "tab indented frontmatter",
			contents:  []byte("---\nfingerprint: FINGERPRINT\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: open\nassignee: human\nfirst_seen: 2026-08-18T09:14:22Z\nlast_seen: 2026-08-18T09:14:22Z\ncount: 1\nresolved_at: ''\nevidence:\n\tchecked: hand written\n\tfreshness_source: runs\n---\nTabs must not stop monitoring.\n"),
			body:      []byte("Tabs must not stop monitoring.\n"),
			wantCount: 2,
		},
		{
			name:      "capitalized status and unknown nested keys",
			contents:  []byte("---\nfingerprint: FINGERPRINT\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: Open\nassignee: human\nfirst_seen: 2026-08-18T09:14:22Z\nlast_seen: 2026-08-18T09:14:22Z\ncount: 1\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\nowner: ops\npriority: high\ntriage_notes:\n  reproduces: true\n  contacts:\n    - Ada\n    - Grace\n---\nUnknown keys and status case survive.\n"),
			body:      []byte("Unknown keys and status case survive.\n"),
			wantCount: 2,
			wantExtra: map[string]any{
				"owner":    "ops",
				"priority": "high",
				"triage_notes": map[string]any{
					"reproduces": true,
					"contacts":   []any{"Ada", "Grace"},
				},
			},
		},
		{
			name:        "unparseable count is warning",
			contents:    []byte("---\nfingerprint: FINGERPRINT\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: open\nassignee: human\nfirst_seen: 2026-08-18T09:14:22Z\nlast_seen: 2026-08-18T09:14:22Z\ncount: many\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\n---\nCount warning must not stop the update.\n"),
			body:        []byte("Count warning must not stop the update.\n"),
			wantWarning: "count \"many\" is not an integer",
			wantCount:   1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "worker-nonzero_exit-"+fingerprint[:8]+".md")
			contents := bytes.ReplaceAll(test.contents, []byte(fingerprintToken), []byte(fingerprint))
			if err := os.WriteFile(path, contents, 0o644); err != nil {
				t.Fatalf("write hand-authored item: %v", err)
			}

			runner := Runner{Name: "worker", Type: RunnerTypeLaunchd, Label: "test.worker"}
			changes, err := NewInboxWriter(dir).Reconcile(
				runner,
				Classification{Runner: runner.Name, Kind: FailureNonzeroExit},
				Health{Present: true, HasRun: true, LastExitStatus: 23, Runs: 4},
				RunnerState{Runs: 4},
				time.Date(2026, 8, 18, 11, 14, 22, 0, time.UTC),
			)
			if test.wantWarning == "" && err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if test.wantWarning != "" && (err == nil || !strings.Contains(err.Error(), test.wantWarning)) {
				t.Fatalf("Reconcile() error = %v, want warning %q", err, test.wantWarning)
			}
			wantChanges := []InboxChange{{Action: "updated", Filename: filepath.Base(path)}}
			if !reflect.DeepEqual(changes, wantChanges) {
				t.Fatalf("Reconcile() changes = %#v, want %#v", changes, wantChanges)
			}

			updated, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read updated item: %v", err)
			}
			_, gotBody, err := splitInboxFile(updated)
			if err != nil {
				t.Fatalf("split updated item: %v", err)
			}
			if !bytes.Equal(gotBody, test.body) {
				t.Fatalf("body changed\n got: %q\nwant: %q", gotBody, test.body)
			}
			frontmatter, _, warnings, err := parseInboxFile(updated)
			if err != nil || len(warnings) != 0 {
				t.Fatalf("parse updated item error, warnings = %v, %v", err, warnings)
			}
			if frontmatter.Status != inboxStatusOpen || frontmatter.Assignee != "human" || frontmatter.Count != test.wantCount {
				t.Fatalf("managed frontmatter = %#v, want open, human assignee, count %d", frontmatter, test.wantCount)
			}
			if !reflect.DeepEqual(frontmatter.Extra, test.wantExtra) {
				t.Fatalf("extra frontmatter = %#v, want %#v", frontmatter.Extra, test.wantExtra)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat updated item: %v", err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("updated item mode = %o, want 600", info.Mode().Perm())
			}
		})
	}
}

func TestInboxWriterLeavesCorruptItemsUnchanged(t *testing.T) {
	t.Parallel()

	const fingerprint = "162662436d2b73476c3c26eb2209b11e135ae6df138b7c87cf61090beeeaa293"
	tests := []struct {
		name     string
		contents string
		wantErr  string
	}{
		{
			name:     "malformed YAML",
			contents: "---\nfingerprint: [unterminated\n---\nHuman body\n",
			wantErr:  "decode YAML frontmatter",
		},
		{
			name:     "identity mismatch",
			contents: "---\nfingerprint: edited-by-human\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: open\ncount: 1\nevidence: {}\n---\nHuman body\n",
			wantErr:  "identity that does not match",
		},
		{
			name:     "invalid status",
			contents: "---\nfingerprint: " + fingerprint + "\nrunner: worker\nfailure_kind: nonzero_exit\nstatus: investigating\ncount: 1\nevidence: {}\n---\nHuman body\n",
			wantErr:  "invalid status",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "worker-nonzero_exit-"+fingerprint[:8]+".md")
			before := []byte(test.contents)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatalf("write corrupt item: %v", err)
			}
			runner := Runner{Name: "worker", Type: RunnerTypeLaunchd, Label: "test.worker"}
			changes, err := NewInboxWriter(dir).Reconcile(
				runner,
				Classification{Runner: runner.Name, Kind: FailureNonzeroExit},
				Health{Present: true, HasRun: true, LastExitStatus: 23, Runs: 4},
				RunnerState{Runs: 4},
				time.Date(2026, 8, 18, 11, 14, 22, 0, time.UTC),
			)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Reconcile() error = %v, want %q", err, test.wantErr)
			}
			if len(changes) != 0 {
				t.Fatalf("Reconcile() changes = %#v, want none", changes)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read corrupt item after reconcile: %v", err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("corrupt item was overwritten\n got: %q\nwant: %q", after, before)
			}
		})
	}
}

func TestInboxWriterIsolatesCorruptItemFromOtherFailureKinds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := Runner{Name: "worker", Type: RunnerTypeLaunchd, Label: "test.worker"}
	nonzeroFingerprint := Fingerprint(runner.Name, FailureNonzeroExit)
	nonzeroPath := filepath.Join(dir, "worker-nonzero_exit-"+nonzeroFingerprint[:8]+".md")
	corrupt := []byte("---\nfingerprint: [unterminated\n---\nDo not overwrite.\n")
	if err := os.WriteFile(nonzeroPath, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt nonzero item: %v", err)
	}
	staleFingerprint := Fingerprint(runner.Name, FailureStale)
	staleFilename := "worker-stale-" + staleFingerprint[:8] + ".md"
	stalePath := filepath.Join(dir, staleFilename)
	stale := fmt.Sprintf("---\nfingerprint: %s\nrunner: worker\nfailure_kind: stale\nstatus: open\nassignee: human\nfirst_seen: 2026-08-18T09:00:00Z\nlast_seen: 2026-08-18T09:00:00Z\ncount: 2\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\n---\nResolve this item.\n", staleFingerprint)
	if err := os.WriteFile(stalePath, []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale item: %v", err)
	}

	changes, err := NewInboxWriter(dir).Reconcile(
		runner,
		Classification{Runner: runner.Name, Kind: FailureHealthy},
		Health{Present: true, HasRun: true, Runs: 5},
		RunnerState{Runs: 5},
		time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	)
	if err == nil || !strings.Contains(err.Error(), filepath.Base(nonzeroPath)) {
		t.Fatalf("Reconcile() error = %v, want corrupt item error", err)
	}
	wantChanges := []InboxChange{{Action: "resolved", Filename: staleFilename}}
	if !reflect.DeepEqual(changes, wantChanges) {
		t.Fatalf("Reconcile() changes = %#v, want %#v", changes, wantChanges)
	}
	after, err := os.ReadFile(nonzeroPath)
	if err != nil {
		t.Fatalf("read corrupt item: %v", err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt item changed\n got: %q\nwant: %q", after, corrupt)
	}
	frontmatter, body, warnings, err := parseInboxFile(mustReadFile(t, stalePath))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("parse resolved stale item error, warnings = %v, %v", err, warnings)
	}
	if frontmatter.Status != inboxStatusResolved || frontmatter.ResolvedAt != "2026-08-18T12:00:00Z" {
		t.Fatalf("resolved stale frontmatter = %#v", frontmatter)
	}
	if body != "Resolve this item.\n" {
		t.Fatalf("resolved stale body = %q, want preserved", body)
	}
}

func TestInboxWriterResolvesCapitalizedOpenStatus(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := Runner{Name: "worker", Type: RunnerTypeLaunchd, Label: "test.worker"}
	fingerprint := Fingerprint(runner.Name, FailureStale)
	filename := "worker-stale-" + fingerprint[:8] + ".md"
	path := filepath.Join(dir, filename)
	contents := fmt.Sprintf("---\nfingerprint: %s\nrunner: worker\nfailure_kind: stale\nstatus: OPEN\nassignee: human\nfirst_seen: 2026-08-18T09:00:00Z\nlast_seen: 2026-08-18T09:00:00Z\ncount: 2\nresolved_at: ''\nevidence:\n  checked: hand written\n  freshness_source: runs\n---\nKeep this body.\n", fingerprint)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write capitalized-status item: %v", err)
	}

	changes, err := NewInboxWriter(dir).Reconcile(
		runner,
		Classification{Runner: runner.Name, Kind: FailureHealthy},
		Health{Present: true},
		RunnerState{},
		time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantChanges := []InboxChange{{Action: "resolved", Filename: filename}}
	if !reflect.DeepEqual(changes, wantChanges) {
		t.Fatalf("Reconcile() changes = %#v, want %#v", changes, wantChanges)
	}
	frontmatter, body, warnings, err := parseInboxFile(mustReadFile(t, path))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("parse resolved item error, warnings = %v, %v", err, warnings)
	}
	if frontmatter.Status != inboxStatusResolved || body != "Keep this body.\n" {
		t.Fatalf("resolved item = %#v, body %q", frontmatter, body)
	}
}

func TestInboxWriterSweepsInterruptedTemporaryFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stale := filepath.Join(dir, ".capstan-inbox-interrupted")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write interrupted inbox file: %v", err)
	}
	runner := Runner{Name: "healthy", Type: RunnerTypeLaunchd, Label: "test.healthy"}
	if _, err := NewInboxWriter(dir).Reconcile(
		runner,
		Classification{Runner: runner.Name, Kind: FailureHealthy},
		Health{Present: true},
		RunnerState{},
		time.Date(2026, 8, 18, 11, 14, 22, 0, time.UTC),
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("interrupted inbox file remains or stat failed unexpectedly: %v", err)
	}
}

func TestInboxWriterItemPathRejectsSeparators(t *testing.T) {
	t.Parallel()

	_, _, err := NewInboxWriter(t.TempDir()).itemPath("../unsafe", FailureStale, strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "unsafe inbox filename") {
		t.Fatalf("itemPath() error = %v, want base-filename rejection", err)
	}
}

func TestInboxWriterCreatesPrivateFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := Runner{Name: "missing", Type: RunnerTypeLaunchd, Label: "test.missing"}
	changes, err := NewInboxWriter(dir).Reconcile(
		runner,
		Classification{Runner: runner.Name, Kind: FailureNotLoaded},
		Health{Present: false},
		RunnerState{},
		time.Date(2026, 8, 18, 11, 14, 22, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, changes[0].Filename))
	if err != nil {
		t.Fatalf("stat inbox item: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("new inbox item mode = %o, want 600", info.Mode().Perm())
	}
}

func TestInboxWriterEvidenceByFailureKind(t *testing.T) {
	t.Parallel()

	activityPath := filepath.Join(t.TempDir(), "activity")
	activityTime := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	if err := os.WriteFile(activityPath, []byte("activity"), 0o600); err != nil {
		t.Fatalf("write activity source: %v", err)
	}
	if err := os.Chtimes(activityPath, activityTime, activityTime); err != nil {
		t.Fatalf("set activity source mtime: %v", err)
	}

	tests := []struct {
		name   string
		runner Runner
		kind   FailureKind
		health Health
		state  RunnerState
		want   inboxEvidence
	}{
		{
			name:   "nonzero exit",
			runner: Runner{Name: "failed", Label: "test.failed"},
			kind:   FailureNonzeroExit,
			health: Health{Present: true, HasRun: true, LastExitStatus: -9, ExitReason: "SIGNAL", Runs: 7},
			want: inboxEvidence{
				Checked:         "launchd last exit status, exit reason, and run count",
				FreshnessSource: "runs",
				LastExitStatus:  intPointer(-9),
				ExitReason:      "SIGNAL",
				RunCount:        intPointer(7),
			},
		},
		{
			name:   "not loaded",
			runner: Runner{Name: "missing", Label: "test.missing", ActivitySource: activityPath},
			kind:   FailureNotLoaded,
			health: Health{Present: false},
			want: inboxEvidence{
				Checked:         "launchd job presence",
				FreshnessSource: "not_checked",
				Label:           "test.missing",
				DomainsProbed:   []string{"gui/" + strconv.Itoa(os.Getuid()), "system"},
			},
		},
		{
			name: "stale activity source",
			runner: Runner{
				Name:               "stale",
				Label:              "test.stale",
				ActivitySource:     activityPath,
				ExpectedCadence:    time.Hour,
				StalenessThreshold: 30 * time.Minute,
			},
			kind:   FailureStale,
			health: Health{Present: true, HasRun: true, Runs: 11},
			state:  RunnerState{Runs: 11, RunsChangedAt: activityTime.Add(-time.Hour)},
			want: inboxEvidence{
				Checked:             "last activity against expected cadence plus staleness threshold",
				FreshnessSource:     "activity_source",
				RunCount:            intPointer(11),
				ActivitySource:      activityPath,
				ActivitySourceState: "present",
				LastActivity:        activityTime.Format(time.RFC3339),
				ExpectedCadence:     "1h0m0s",
				StalenessThreshold:  "30m0s",
				StaleAfter:          activityTime.Add(90 * time.Minute).Format(time.RFC3339),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := buildInboxEvidence(test.runner, test.kind, test.health, test.state)
			if err != nil {
				t.Fatalf("buildInboxEvidence() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("buildInboxEvidence() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestInboxWriterHealthyFromStartCreatesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := Runner{Name: "healthy", Type: RunnerTypeLaunchd, Label: "test.healthy"}
	changes, err := NewInboxWriter(dir).Reconcile(
		runner,
		Classification{Runner: runner.Name, Kind: FailureHealthy},
		Health{Present: true, HasRun: true, Runs: 1},
		RunnerState{Runs: 1},
		time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("Reconcile() changes = %#v, want none", changes)
	}
	assertOnlyInboxFile(t, dir)
}

func TestInboxWriterRejectsUnsafeRunnerName(t *testing.T) {
	root := t.TempDir()
	grandparent := filepath.Join(root, "grandparent")
	parent := filepath.Join(grandparent, "parent")
	dir := filepath.Join(parent, "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create controlled inbox tree: %v", err)
	}
	runner := Runner{Name: "../../escape", Type: RunnerTypeLaunchd, Label: "test.escape"}
	_, err := NewInboxWriter(dir).Reconcile(
		runner,
		Classification{Runner: runner.Name, Kind: FailureNotLoaded},
		Health{Present: false},
		RunnerState{},
		time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	)
	if err == nil || !strings.Contains(err.Error(), "runner name") || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("Reconcile() error = %v, want explicit unsafe-name error", err)
	}
	assertOnlyInboxFile(t, dir)
	assertOnlyInboxFile(t, parent, "inbox")
	assertOnlyInboxFile(t, grandparent, "parent")
	if _, err := os.Stat(filepath.Join(grandparent, "escape-not_loaded-"+Fingerprint(runner.Name, FailureNotLoaded)[:8]+".md")); !os.IsNotExist(err) {
		t.Fatalf("grandparent escape path exists or returned unexpected error: %v", err)
	}
}

func assertInboxState(t *testing.T, path string, wantFrontmatter inboxFrontmatter, wantBody string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	gotFrontmatter, gotBody, warnings, err := parseInboxFile(data)
	if err != nil {
		t.Fatalf("parseInboxFile() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("parseInboxFile() warnings = %v, want none", warnings)
	}
	if !reflect.DeepEqual(gotFrontmatter, wantFrontmatter) {
		t.Errorf("frontmatter = %#v, want %#v", gotFrontmatter, wantFrontmatter)
	}
	if gotBody != wantBody {
		t.Errorf("body = %q, want exact human-edited body %q", gotBody, wantBody)
	}
}

func assertOnlyInboxFile(t *testing.T, dir string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", dir, err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inbox entries = %#v, want %#v", got, want)
	}
}

func intPointer(value int) *int {
	return &value
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}

	return data
}
