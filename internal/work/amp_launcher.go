package work

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxAmpJSON = 64 << 10

var repositoryPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9._-]{1,100}$`)
var ampProjectIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,254}$`)
var ampThreadPattern = regexp.MustCompile(`^T-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var ampVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})\.(0|[1-9][0-9]{0,19})\.(0|[1-9][0-9]{0,19})(?:-g[0-9a-f]+)?(?: \([^\r\n()]+\))?(?:\r?\n)?$`)

func validRepository(value string) bool {
	if len(value) > 140 || !repositoryPattern.MatchString(value) {
		return false
	}
	owner, repo, _ := strings.Cut(value, "/")
	return !strings.Contains(owner, "--") && repo != "." && repo != ".." && !strings.HasSuffix(repo, ".git")
}

// AmpSessionRef contains only the server's canonical remote identity fields.
type AmpSessionRef struct {
	ProjectID string `json:"project_id"`
	ThreadID  string `json:"thread_id"`
	ThreadURL string `json:"thread_url"`
}

func (r AmpSessionRef) String() (string, error) {
	if !ampProjectIDPattern.MatchString(r.ProjectID) ||
		!ampThreadPattern.MatchString(r.ThreadID) || r.ThreadURL != "https://ampcode.com/threads/"+r.ThreadID {
		return "", errors.New("amp session identity is invalid")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return "", errors.New("amp session identity could not be encoded")
	}
	return "amp:v1:" + base64.RawURLEncoding.EncodeToString(data), nil
}

// ParseAmpSessionRef rejects alternate encodings, unknown fields, and mismatched URLs.
func ParseAmpSessionRef(value string) (AmpSessionRef, error) {
	var ref AmpSessionRef
	encoded, ok := strings.CutPrefix(value, "amp:v1:")
	if !ok || len(value) > 2048 {
		return ref, errors.New("amp session reference is invalid")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || strictAmpJSON(data, &ref) != nil {
		return AmpSessionRef{}, errors.New("amp session reference is invalid")
	}
	canonical, err := ref.String()
	if err != nil || canonical != value {
		return AmpSessionRef{}, errors.New("amp session reference is invalid")
	}
	return ref, nil
}

func strictAmpJSON(data []byte, target any) error {
	return strictBoundedJSON(data, target, maxAmpJSON)
}

func strictBoundedJSON(data []byte, target any, limit int) error {
	if len(data) > limit || validateUniqueJSON(data) != nil {
		return errors.New("invalid Amp JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || expectJSONEOF(d) != nil {
		return errors.New("invalid Amp JSON")
	}
	return nil
}

func validateAmpVersion(data []byte) error {
	match := ampVersionPattern.FindSubmatch(data)
	if len(data) > maxAmpJSON || len(match) != 4 {
		return errors.New("amp version output is invalid")
	}
	var version [3]uint64
	for i := range version {
		v, err := strconv.ParseUint(string(match[i+1]), 10, 64)
		if err != nil {
			return errors.New("amp version output is invalid")
		}
		version[i] = v
	}
	if version[0] == 0 && version[1] == 0 && version[2] < 1792929650 {
		return errors.New("amp CLI must be at least 0.0.1792929650")
	}
	return nil
}

// AmpCommands is the bounded command/process seam. Start detaches and returns only the first event.
type AmpCommands interface {
	Run(context.Context, string, []string) ([]byte, error)
	Start(context.Context, string, []string, string, string) ([]byte, error)
}

var errAmpProjectAbsent = errors.New("amp project is absent")
var errAmpDispatchUncertain = errors.New("amp initialization is uncertain; retaining the claim for recovery")

// AmpLauncher dispatches only explicit remote leaf work. Local process exit is not remote liveness.
type AmpLauncher struct {
	path     string
	commands AmpCommands
}

func NewAmpLauncher(path string, commands AmpCommands) *AmpLauncher {
	if commands == nil {
		commands = nativeAmpCommands{}
	}
	return &AmpLauncher{path: path, commands: commands}
}

func (l *AmpLauncher) version(ctx context.Context) error {
	if !filepath.IsAbs(l.path) || strings.ContainsRune(l.path, 0) {
		return errors.New("amp executable must be absolute")
	}
	data, err := l.commands.Run(ctx, l.path, []string{"--version"})
	if err != nil {
		return errors.New("amp version check failed")
	}
	return validateAmpVersion(data)
}

// ampProject is the observed projects get JSON representation.
type ampProject struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Owner struct {
		Type   string `json:"type"`
		UserID string `json:"userID"`
	} `json:"owner"`
	Name                 string            `json:"name"`
	Namespace            string            `json:"namespace"`
	Description          string            `json:"description"`
	IconURL              *string           `json:"iconURL"`
	ListedOnAmpStore     bool              `json:"listedOnAmpStore"`
	Remixable            bool              `json:"remixable"`
	GalleryMedia         []json.RawMessage `json:"galleryMedia"`
	DesktopResumeScript  string            `json:"desktopResumeScript"`
	PreCloneScript       string            `json:"preCloneScript"`
	PreSetupScript       string            `json:"preSetupScript"`
	AllowVirtualExecutor bool              `json:"allowVirtualExecutor"`
	RepositoryURL        string            `json:"repositoryURL"`
	RepositoryMode       string            `json:"repositoryMode"`
	OrbCommitAuthor      string            `json:"orbCommitAuthor"`
	BaseBranch           string            `json:"baseBranch"`
	RemoteURLs           []string          `json:"remoteURLs"`
	EnvironmentResource  string            `json:"environmentResource"`
}

func (l *AmpLauncher) project(ctx context.Context, repository string) (ampProject, error) {
	var project ampProject
	if !validRepository(repository) {
		return project, errors.New("amp repository is invalid")
	}
	data, err := l.commands.Run(ctx, l.path, []string{"projects", "get", repository, "--json"})
	if errors.Is(err, errAmpProjectAbsent) {
		data, err = l.commands.Run(ctx, l.path, []string{"projects", "create", "https://github.com/" + repository, "--personal", "--json"})
		// Create's response is not the get representation. Validate its JSON envelope,
		// then always re-get, including a competing creator's nonzero result.
		if err == nil {
			var created map[string]json.RawMessage
			if strictAmpJSON(data, &created) != nil || created == nil {
				return project, errors.New("amp project creation response is invalid")
			}
		}
		data, err = l.commands.Run(ctx, l.path, []string{"projects", "get", repository, "--json"})
	}
	if err != nil || strictAmpJSON(data, &project) != nil || project.RepositoryURL != "https://github.com/"+repository || project.RepositoryMode != "mapped" || !ampProjectIDPattern.MatchString(project.ID) ||
		!validAmpProjectURL(project.URL) || project.URL != "https://ampcode.com/@"+project.Namespace+"/"+project.Name {
		return ampProject{}, errors.New("amp project resolution failed")
	}
	return project, nil
}

func validAmpProjectURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && len(value) <= 2048 && u.Scheme == "https" && u.Host == "ampcode.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		regexp.MustCompile(`^/@[a-zA-Z0-9_-]+/[a-zA-Z0-9._-]+$`).MatchString(u.Path) && u.RawPath == ""
}

func ampPointer(itemID, nonce string) (string, error) {
	if !validUUID(itemID) || !validUUIDv7(nonce) {
		return "", errors.New("amp pointer identity is invalid")
	}
	return fmt.Sprintf("Use Capstan MCP work_brief for item %s, nonce %s. Retry briefly if the new claim binding is not visible yet; follow the brief.", itemID, nonce), nil
}

func (l *AmpLauncher) Launch(ctx context.Context, spec LaunchSpec) (string, error) {
	if spec.Role == "orchestrator" || !validRepository(spec.Repository) || spec.ExistingSessionRef != "" || !filepath.IsAbs(spec.WorkDir) {
		return "", errors.New("amp launch specification is invalid")
	}
	pointer, err := ampPointer(spec.ItemID, spec.LaunchNonce)
	if err != nil {
		return "", err
	}
	if err := l.version(ctx); err != nil {
		return "", err
	}
	project, err := l.project(ctx, spec.Repository)
	if err != nil {
		return "", err
	}
	// Preserve the project half of the identity across a crash before the delivered transition.
	bindingPath := filepath.Join(spec.WorkDir, "amp-"+spec.LaunchNonce+".project-id")
	if saved, err := os.ReadFile(bindingPath); err == nil {
		if string(saved) != project.ID+"\n" {
			return "", errors.New("amp project identity changed during dispatch recovery")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := atomicPrivateWrite(bindingPath, []byte(project.ID+"\n")); err != nil {
			return "", errors.New("amp project binding could not be saved")
		}
	} else {
		return "", errors.New("amp project binding could not be read")
	}
	args := []string{"-ox", pointer, "--project", spec.Repository, "--mode", "claude-code", "--title", "Capstan work " + spec.ItemID, "--stream-json", "--no-archive-after-execute"}
	event, err := l.commands.Start(ctx, l.path, args, spec.WorkDir, spec.LaunchNonce)
	if err != nil {
		if errors.Is(err, errAmpDispatchUncertain) {
			return "", errAmpDispatchUncertain
		}
		return "", errors.New("amp dispatch failed")
	}
	thread, err := ampInitThread(event)
	if err != nil {
		return "", err
	}
	return (AmpSessionRef{ProjectID: project.ID, ThreadID: thread, ThreadURL: "https://ampcode.com/threads/" + thread}).String()
}

func ampInitThread(data []byte) (string, error) {
	var event struct {
		Type       string          `json:"type"`
		Subtype    string          `json:"subtype"`
		SessionID  string          `json:"session_id"`
		AgentMode  json.RawMessage `json:"agent_mode"`
		CWD        json.RawMessage `json:"cwd"`
		MCPServers json.RawMessage `json:"mcp_servers"`
		Subagents  json.RawMessage `json:"subagents"`
		Tools      json.RawMessage `json:"tools"`
	}
	if strictAmpJSON(data, &event) != nil || event.Type != "system" || event.Subtype != "init" || !ampThreadPattern.MatchString(event.SessionID) {
		return "", errors.New("amp initial stream event is invalid")
	}
	return event.SessionID, nil
}

func (l *AmpLauncher) Alive(_ context.Context, value string) (bool, error) {
	_, err := ParseAmpSessionRef(value)
	return err == nil, err
}
func (l *AmpLauncher) Terminate(_ context.Context, value string) error {
	_, err := ParseAmpSessionRef(value)
	return err
}
func (l *AmpLauncher) OpenGate(_ context.Context, value, _, _ string) error {
	_, err := ParseAmpSessionRef(value)
	return err
}
func (l *AmpLauncher) Continue(ctx context.Context, value, text string) error {
	ref, err := ParseAmpSessionRef(value)
	if err != nil || !validSessionMessage(text) {
		return errors.New("amp continuation inputs are invalid")
	}
	if err := l.version(ctx); err != nil {
		return err
	}
	_, err = l.commands.Run(ctx, l.path, []string{"threads", "continue", ref.ThreadID, "-ox", text})
	if err != nil {
		return errors.New("amp continuation failed")
	}
	return nil
}

func validSessionMessage(text string) bool {
	return len(text) <= maxSessionMessageBytes && utf8.ValidString(text) && utf8.RuneCountInString(text) <= maxSteerCharacters && strings.TrimSpace(text) != "" && !strings.ContainsRune(text, 0)
}

func (l *AmpLauncher) SendEnter(ctx context.Context, value, path, nonce, id string) error {
	return l.ResendPointer(ctx, value, path, nonce, id)
}
func (l *AmpLauncher) ResendPointer(ctx context.Context, value, _, nonce, _ string) error {
	if !validUUIDv7(nonce) {
		return errors.New("amp delivery nonce is invalid")
	}
	return l.Continue(ctx, value, "Use Capstan MCP work_brief for the current claim, nonce "+nonce+". Retry briefly if the binding is not visible yet; follow the brief.")
}
