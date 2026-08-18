package runners

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	inboxStatusOpen     = "open"
	inboxStatusResolved = "resolved"
)

var inboxFailureKinds = []FailureKind{FailureNotLoaded, FailureNonzeroExit, FailureStale}

// InboxChange records one durable lifecycle change made by Reconcile.
type InboxChange struct {
	Action   string
	Filename string
}

// InboxWriter reconciles runner classifications into durable inbox files.
type InboxWriter struct {
	dir string
}

type inboxFrontmatter struct {
	Fingerprint string         `yaml:"fingerprint"`
	Runner      string         `yaml:"runner"`
	FailureKind FailureKind    `yaml:"failure_kind"`
	Status      string         `yaml:"status"`
	Assignee    string         `yaml:"assignee"`
	FirstSeen   string         `yaml:"first_seen"`
	LastSeen    string         `yaml:"last_seen"`
	Count       int            `yaml:"count"`
	ResolvedAt  string         `yaml:"resolved_at"`
	Evidence    inboxEvidence  `yaml:"evidence"`
	Extra       map[string]any `yaml:",inline"`
}

type rawInboxFrontmatter struct {
	Fingerprint string         `yaml:"fingerprint"`
	Runner      string         `yaml:"runner"`
	FailureKind FailureKind    `yaml:"failure_kind"`
	Status      string         `yaml:"status"`
	Assignee    string         `yaml:"assignee"`
	FirstSeen   string         `yaml:"first_seen"`
	LastSeen    string         `yaml:"last_seen"`
	Count       any            `yaml:"count"`
	ResolvedAt  string         `yaml:"resolved_at"`
	Evidence    inboxEvidence  `yaml:"evidence"`
	Extra       map[string]any `yaml:",inline"`
}

type inboxEvidence struct {
	Checked             string   `yaml:"checked"`
	FreshnessSource     string   `yaml:"freshness_source"`
	LastExitStatus      *int     `yaml:"last_exit_status,omitempty"`
	ExitReason          string   `yaml:"exit_reason,omitempty"`
	RunCount            *int     `yaml:"run_count,omitempty"`
	ActivitySource      string   `yaml:"activity_source,omitempty"`
	ActivitySourceState string   `yaml:"activity_source_state,omitempty"`
	LastActivity        string   `yaml:"last_activity,omitempty"`
	ExpectedCadence     string   `yaml:"expected_cadence,omitempty"`
	StalenessThreshold  string   `yaml:"staleness_threshold,omitempty"`
	StaleAfter          string   `yaml:"stale_after,omitempty"`
	Label               string   `yaml:"label,omitempty"`
	DomainsProbed       []string `yaml:"domains_probed,omitempty"`
}

// NewInboxWriter returns a writer rooted at path.
func NewInboxWriter(path string) *InboxWriter {
	return &InboxWriter{dir: path}
}

// Fingerprint returns the durable identity for one runner failure kind.
func Fingerprint(runner string, kind FailureKind) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(runner+"\x00"+string(kind))))
}

// Reconcile updates the current failure and resolves any other open failure
// items for the runner. now is supplied by the caller so writes are testable.
func (w *InboxWriter) Reconcile(
	runner Runner,
	classification Classification,
	health Health,
	state RunnerState,
	now time.Time,
) ([]InboxChange, error) {
	if classification.Runner != runner.Name {
		return nil, fmt.Errorf("classification runner %q does not match runner %q", classification.Runner, runner.Name)
	}
	if classification.Kind != FailureHealthy && !isInboxFailureKind(classification.Kind) {
		return nil, fmt.Errorf("runner %q has unsupported failure kind %q", runner.Name, classification.Kind)
	}
	if err := validateInboxRunnerName(runner.Name); err != nil {
		return nil, err
	}
	if err := removeTemporaryFiles(w.dir, ".capstan-inbox-*"); err != nil {
		return nil, fmt.Errorf("clean stale inbox files: %w", err)
	}

	changes := make([]InboxChange, 0, 2)
	var reconcileErrors []error
	for _, kind := range inboxFailureKinds {
		if classification.Kind == kind {
			change, changed, err := w.upsert(runner, kind, health, state, now)
			if changed {
				changes = append(changes, change)
			}
			if err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}

			continue
		}

		change, changed, err := w.resolve(runner, kind, now)
		if err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
		if changed {
			changes = append(changes, change)
		}
	}

	return changes, errors.Join(reconcileErrors...)
}

func (w *InboxWriter) upsert(
	runner Runner,
	kind FailureKind,
	health Health,
	state RunnerState,
	now time.Time,
) (InboxChange, bool, error) {
	fingerprint := Fingerprint(runner.Name, kind)
	path, filename, err := w.itemPath(runner.Name, kind, fingerprint)
	if err != nil {
		return InboxChange{}, false, err
	}

	evidence, err := buildInboxEvidence(runner, kind, health, state)
	if err != nil {
		return InboxChange{}, false, err
	}
	nowText := now.UTC().Format(time.RFC3339)
	frontmatter := inboxFrontmatter{
		Fingerprint: fingerprint,
		Runner:      runner.Name,
		FailureKind: kind,
		Status:      inboxStatusOpen,
		FirstSeen:   nowText,
		LastSeen:    nowText,
		Count:       1,
		Evidence:    evidence,
	}
	body := newInboxBody(runner, kind, evidence)
	action := "written"

	data, err := os.ReadFile(path)
	var itemWarnings []error
	if err == nil {
		existing, existingBody, warnings, parseErr := parseInboxFile(data)
		if parseErr != nil {
			return InboxChange{}, false, fmt.Errorf("parse inbox item %q; leaving it unchanged: %w", path, parseErr)
		}
		for _, warning := range warnings {
			itemWarnings = append(itemWarnings, fmt.Errorf("parse inbox item %q: %w", path, warning))
		}
		if existing.Fingerprint != fingerprint || existing.Runner != runner.Name || existing.FailureKind != kind {
			return InboxChange{}, false, fmt.Errorf("inbox item %q has identity that does not match its filename; leaving it unchanged", path)
		}
		existing.Status = strings.ToLower(strings.TrimSpace(existing.Status))
		if existing.Status != inboxStatusOpen && existing.Status != inboxStatusResolved {
			return InboxChange{}, false, fmt.Errorf("inbox item %q has invalid status %q; leaving it unchanged", path, existing.Status)
		}

		frontmatter.Assignee = existing.Assignee
		frontmatter.FirstSeen = existing.FirstSeen
		frontmatter.Count = existing.Count + 1
		frontmatter.Extra = existing.Extra
		body = existingBody
		if existing.Status == inboxStatusResolved {
			action = "reopened"
		} else {
			action = "updated"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return InboxChange{}, false, fmt.Errorf("read inbox item %q: %w", path, err)
	}

	if err := writeInboxFile(w.dir, path, frontmatter, body); err != nil {
		return InboxChange{}, false, err
	}

	return InboxChange{Action: action, Filename: filename}, true, errors.Join(itemWarnings...)
}

func (w *InboxWriter) resolve(runner Runner, kind FailureKind, now time.Time) (InboxChange, bool, error) {
	fingerprint := Fingerprint(runner.Name, kind)
	path, filename, err := w.itemPath(runner.Name, kind, fingerprint)
	if err != nil {
		return InboxChange{}, false, err
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return InboxChange{}, false, nil
	}
	if err != nil {
		return InboxChange{}, false, fmt.Errorf("read inbox item %q: %w", path, err)
	}

	frontmatter, body, warnings, err := parseInboxFile(data)
	if err != nil {
		return InboxChange{}, false, fmt.Errorf("parse inbox item %q; leaving it unchanged: %w", path, err)
	}
	var itemWarnings []error
	for _, warning := range warnings {
		itemWarnings = append(itemWarnings, fmt.Errorf("parse inbox item %q: %w", path, warning))
	}
	if frontmatter.Fingerprint != fingerprint || frontmatter.Runner != runner.Name || frontmatter.FailureKind != kind {
		return InboxChange{}, false, fmt.Errorf("inbox item %q has identity that does not match its filename; leaving it unchanged", path)
	}
	frontmatter.Status = strings.ToLower(strings.TrimSpace(frontmatter.Status))
	if frontmatter.Status == inboxStatusResolved {
		return InboxChange{}, false, errors.Join(itemWarnings...)
	}
	if frontmatter.Status != inboxStatusOpen {
		return InboxChange{}, false, fmt.Errorf("inbox item %q has invalid status %q; leaving it unchanged", path, frontmatter.Status)
	}

	frontmatter.Status = inboxStatusResolved
	frontmatter.ResolvedAt = now.UTC().Format(time.RFC3339)
	if err := writeInboxFile(w.dir, path, frontmatter, body); err != nil {
		return InboxChange{}, false, err
	}

	return InboxChange{Action: "resolved", Filename: filename}, true, errors.Join(itemWarnings...)
}

func buildInboxEvidence(runner Runner, kind FailureKind, health Health, state RunnerState) (inboxEvidence, error) {
	evidence := inboxEvidence{FreshnessSource: configuredFreshnessSource(runner)}

	switch kind {
	case FailureNotLoaded:
		evidence.Checked = "launchd job presence"
		evidence.FreshnessSource = "not_checked"
		evidence.Label = runner.Label
		evidence.DomainsProbed = []string{"gui/" + strconv.Itoa(os.Getuid()), "system"}
	case FailureNonzeroExit:
		evidence.Checked = "launchd last exit status, exit reason, and run count"
		if health.ExitReason == "" || health.LastExitStatus != 0 {
			status := health.LastExitStatus
			evidence.LastExitStatus = &status
		}
		evidence.ExitReason = health.ExitReason
		runs := health.Runs
		evidence.RunCount = &runs
	case FailureStale:
		evidence.Checked = "last activity against expected cadence plus staleness threshold"
		runs := health.Runs
		evidence.RunCount = &runs
		lastActivity := state.RunsChangedAt
		if lastActivity.IsZero() {
			lastActivity = state.FirstSeenAt
		}
		if runner.ActivitySource != "" {
			evidence.ActivitySource = runner.ActivitySource
			info, err := os.Stat(runner.ActivitySource)
			switch {
			case err == nil:
				lastActivity = info.ModTime()
				evidence.ActivitySourceState = "present"
			case errors.Is(err, os.ErrNotExist):
				lastActivity = state.FirstSeenAt
				evidence.ActivitySourceState = "missing"
			default:
				return inboxEvidence{}, fmt.Errorf("inspect activity source %q for runner %q: %w", runner.ActivitySource, runner.Name, err)
			}
		}
		evidence.LastActivity = lastActivity.UTC().Format(time.RFC3339)
		evidence.ExpectedCadence = runner.ExpectedCadence.String()
		evidence.StalenessThreshold = runner.StalenessThreshold.String()
		evidence.StaleAfter = lastActivity.Add(runner.ExpectedCadence + runner.StalenessThreshold).UTC().Format(time.RFC3339)
	default:
		return inboxEvidence{}, fmt.Errorf("runner %q has unsupported failure kind %q", runner.Name, kind)
	}

	return evidence, nil
}

func configuredFreshnessSource(runner Runner) string {
	if runner.ActivitySource != "" {
		return "activity_source"
	}

	return "runs"
}

func newInboxBody(runner Runner, kind FailureKind, evidence inboxEvidence) string {
	var title, description string
	switch kind {
	case FailureNotLoaded:
		title = fmt.Sprintf("Runner %s is not loaded", runner.Name)
		description = fmt.Sprintf("The launchd job for %s could not be found in any probed domain.", runner.Name)
	case FailureNonzeroExit:
		title = fmt.Sprintf("Runner %s exited unsuccessfully", runner.Name)
		description = fmt.Sprintf("The latest launchd termination for %s indicates a failure.", runner.Name)
	case FailureStale:
		title = fmt.Sprintf("Runner %s is stale", runner.Name)
		description = "The runner has not shown real activity within its expected cadence and staleness threshold."
	}

	return fmt.Sprintf("\n# %s\n\n%s\n\n## Evidence (first observed)\n%s", title, description, readableEvidence(evidence))
}

func readableEvidence(evidence inboxEvidence) string {
	var lines []string
	lines = append(lines,
		"- Checked: "+evidence.Checked,
		"- Freshness source: "+evidence.FreshnessSource,
	)
	if evidence.LastExitStatus != nil {
		lines = append(lines, fmt.Sprintf("- Last exit status: %d", *evidence.LastExitStatus))
	}
	if evidence.ExitReason != "" {
		lines = append(lines, "- Exit reason: "+evidence.ExitReason)
	}
	if evidence.RunCount != nil {
		lines = append(lines, fmt.Sprintf("- Run count: %d", *evidence.RunCount))
	}
	if evidence.ActivitySource != "" {
		lines = append(lines, "- Activity source: "+evidence.ActivitySource)
	}
	if evidence.ActivitySourceState != "" {
		lines = append(lines, "- Activity source state: "+evidence.ActivitySourceState)
	}
	if evidence.LastActivity != "" {
		lines = append(lines, "- Last activity: "+evidence.LastActivity)
	}
	if evidence.ExpectedCadence != "" {
		lines = append(lines, "- Expected cadence: "+evidence.ExpectedCadence)
	}
	if evidence.StalenessThreshold != "" {
		lines = append(lines, "- Staleness threshold: "+evidence.StalenessThreshold)
	}
	if evidence.StaleAfter != "" {
		lines = append(lines, "- Stale after: "+evidence.StaleAfter)
	}
	if evidence.Label != "" {
		lines = append(lines, "- Label: "+evidence.Label)
	}
	if len(evidence.DomainsProbed) > 0 {
		lines = append(lines, "- Domains probed: "+strings.Join(evidence.DomainsProbed, ", "))
	}

	return strings.Join(lines, "\n") + "\n"
}

func parseInboxFile(data []byte) (inboxFrontmatter, string, []error, error) {
	frontmatterData, body, err := splitInboxFile(data)
	if err != nil {
		return inboxFrontmatter{}, "", nil, err
	}

	var raw rawInboxFrontmatter
	if err := yaml.Unmarshal(normalizeFrontmatter(frontmatterData), &raw); err != nil {
		return inboxFrontmatter{}, "", nil, fmt.Errorf("decode YAML frontmatter: %w", err)
	}

	count, warning := parseInboxCount(raw.Count)
	frontmatter := inboxFrontmatter{
		Fingerprint: raw.Fingerprint,
		Runner:      raw.Runner,
		FailureKind: raw.FailureKind,
		Status:      raw.Status,
		Assignee:    raw.Assignee,
		FirstSeen:   raw.FirstSeen,
		LastSeen:    raw.LastSeen,
		Count:       count,
		ResolvedAt:  raw.ResolvedAt,
		Evidence:    raw.Evidence,
		Extra:       raw.Extra,
	}
	if warning != nil {
		return frontmatter, string(body), []error{warning}, nil
	}

	return frontmatter, string(body), nil, nil
}

func splitInboxFile(data []byte) ([]byte, []byte, error) {
	lineEnd := bytes.IndexByte(data, '\n')
	if lineEnd < 0 || string(bytes.TrimSuffix(data[:lineEnd], []byte{'\r'})) != "---" {
		return nil, nil, errors.New("missing YAML frontmatter opening delimiter")
	}

	frontmatterStart := lineEnd + 1
	lineStart := frontmatterStart
	for lineStart <= len(data) {
		relativeEnd := bytes.IndexByte(data[lineStart:], '\n')
		lineStop := len(data)
		bodyStart := len(data)
		if relativeEnd >= 0 {
			lineStop = lineStart + relativeEnd
			bodyStart = lineStop + 1
		}
		line := bytes.TrimSuffix(data[lineStart:lineStop], []byte{'\r'})
		if string(line) == "---" {
			return data[frontmatterStart:lineStart], data[bodyStart:], nil
		}
		if relativeEnd < 0 {
			break
		}
		lineStart = bodyStart
	}

	return nil, nil, errors.New("missing YAML frontmatter closing delimiter")
}

func normalizeFrontmatter(data []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, line := range lines {
		indentEnd := 0
		var indent strings.Builder
		for indentEnd < len(line) {
			switch line[indentEnd] {
			case ' ':
				indent.WriteByte(' ')
			case '\t':
				indent.WriteString("    ")
			default:
				lines[i] = indent.String() + line[indentEnd:]

				goto nextLine
			}
			indentEnd++
		}
		lines[i] = indent.String()
	nextLine:
	}

	return []byte(strings.Join(lines, "\n"))
}

func parseInboxCount(value any) (int, error) {
	switch count := value.(type) {
	case int:
		return count, nil
	case int64:
		return int(count), nil
	case uint64:
		return int(count), nil
	default:
		return 0, fmt.Errorf("count %q is not an integer; reset it to zero", fmt.Sprint(value))
	}
}

func writeInboxFile(dir, path string, frontmatter inboxFrontmatter, body string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create inbox directory %q: %w", dir, err)
	}

	yamlData, err := yaml.Marshal(frontmatter)
	if err != nil {
		return fmt.Errorf("encode inbox frontmatter: %w", err)
	}
	data := append([]byte("---\n"), yamlData...)
	data = append(data, []byte("---\n"+body)...)

	tmp, err := os.CreateTemp(dir, ".capstan-inbox-*")
	if err != nil {
		return fmt.Errorf("create temporary inbox item: %w", err)
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

		return fmt.Errorf("secure temporary inbox item: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write temporary inbox item: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary inbox item: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace inbox item %q: %w", path, err)
	}
	committed = true

	return nil
}

func (w *InboxWriter) itemPath(runner string, kind FailureKind, fingerprint string) (string, string, error) {
	filename := fmt.Sprintf("%s-%s-%s.md", runner, kind, fingerprint[:8])
	if filepath.Base(filename) != filename {
		return "", "", fmt.Errorf("runner %q produces unsafe inbox filename %q", runner, filename)
	}

	path := filepath.Join(w.dir, filename)
	relative, err := filepath.Rel(w.dir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("runner %q produces inbox path outside %q", runner, w.dir)
	}

	return path, filename, nil
}

func validateInboxRunnerName(name string) error {
	if !runnerNamePattern.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("runner name %q is unsafe for inbox filenames", name)
	}

	return nil
}

func isInboxFailureKind(kind FailureKind) bool {
	for _, candidate := range inboxFailureKinds {
		if kind == candidate {
			return true
		}
	}

	return false
}
