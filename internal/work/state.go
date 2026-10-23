package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const stateVersion = 1

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// State is the global recovery record. It deliberately contains no bearer, prompt, or signed URL.
type State struct {
	Version int                   `json:"version"`
	Items   map[string]*ItemState `json:"items"`
}

// ItemState contains the minimum fenced local session data needed after a crash.
type ItemState struct {
	TaskID           string          `json:"task_id"`
	Role             string          `json:"role"`
	Fence            int             `json:"fence"`
	Phase            string          `json:"phase"`
	WorkDir          string          `json:"work_dir"`
	LaunchNonce      string          `json:"launch_nonce"`
	SessionRef       string          `json:"session_ref,omitempty"`
	SeatFence        int             `json:"seat_fence,omitempty"`
	PendingLaunchKey string          `json:"pending_launch_key,omitempty"`
	Directive        *DirectiveState `json:"directive,omitempty"`
}

// DirectiveState records a directive before execution and its one later-tick outcome.
type DirectiveState struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	ReportPending bool   `json:"report_pending,omitempty"`
}

// StateStore atomically persists the recovery record.
type StateStore struct {
	path    string
	workDir string
}

// NewStateStore creates a state store for one configured drain.
func NewStateStore(path, workDir string) *StateStore {
	return &StateStore{path: path, workDir: workDir}
}

// Load reads and validates all state. Any malformed entry fails the run closed.
func (s *StateStore) Load() (State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{Version: stateVersion, Items: make(map[string]*ItemState)}, nil
		}

		return State{}, errors.New("work state could not be read")
	}
	if err := requirePrivateRegularFile(s.path, "work state"); err != nil {
		return State{}, err
	}
	var state State
	if validateUniqueJSON(data) != nil {
		return State{}, errors.New("work state is malformed and was left unchanged")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil || expectJSONEOF(decoder) != nil {
		return State{}, errors.New("work state is malformed and was left unchanged")
	}
	if state.Version != stateVersion || state.Items == nil {
		return State{}, errors.New("work state has an unsupported shape and was left unchanged")
	}
	if len(state.Items) > MaxBatch {
		return State{}, fmt.Errorf("work state contains more than %d held items and was left unchanged", MaxBatch)
	}
	for id, item := range state.Items {
		if err := s.validateItem(id, item); err != nil {
			return State{}, fmt.Errorf("work state is malformed at item %q and was left unchanged", safeID(id))
		}
	}

	return state, nil
}

// Save writes a private fsynced temporary file, renames it, then syncs the parent directory.
func (s *StateStore) Save(state State) error {
	if state.Version != stateVersion || state.Items == nil {
		return errors.New("refusing to save invalid work state")
	}
	for id, item := range state.Items {
		if err := s.validateItem(id, item); err != nil {
			return fmt.Errorf("refusing to save invalid work state item %q", safeID(id))
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return errors.New("work state could not be encoded")
	}
	data = append(data, '\n')
	if err := atomicPrivateWrite(s.path, data); err != nil {
		return errors.New("work state could not be saved; the prior file was left unchanged")
	}

	return nil
}

func (s *StateStore) validateItem(id string, item *ItemState) error {
	if !validUUID(id) || item == nil || !validUUID(item.TaskID) || item.Fence < 1 || item.Role == "" ||
		(item.Phase != "claimed" && item.Phase != "launched" && item.Phase != "delivered" && item.Phase != "routed") ||
		!validUUID(item.LaunchNonce) {
		return errors.New("invalid item fields")
	}
	wantDir := filepath.Join(s.workDir, fmt.Sprintf("%s-f%d", id, item.Fence))
	actual, err := filepath.Abs(item.WorkDir)
	if err != nil {
		return err
	}
	want, err := filepath.Abs(wantDir)
	if err != nil || actual != want {
		return errors.New("invalid item work directory")
	}
	if item.Phase != "claimed" && item.SessionRef == "" {
		return errors.New("established phase has no session ref")
	}
	if item.Phase == "claimed" && item.SessionRef == "" && item.PendingLaunchKey == "" {
		return errors.New("pending launch has no idempotency key")
	}
	if item.SessionRef != "" && item.PendingLaunchKey != "" {
		return errors.New("established session still has a pending launch key")
	}
	if item.Directive != nil {
		if !validUUID(item.Directive.ID) || !supportedDirective(item.Directive.Kind) ||
			(item.Directive.Status != "pending" && item.Directive.Status != "succeeded" && item.Directive.Status != "failed") ||
			(item.Directive.Status == "pending" && item.Directive.ReportPending) {
			return errors.New("invalid directive state")
		}
	}

	return nil
}

func atomicPrivateWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
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
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	committed = true

	return syncDirectory(dir)
}

func writePrivate(path, content string) error {
	return atomicPrivateWrite(path, []byte(content))
}

func validUUID(value string) bool { return uuidPattern.MatchString(value) }

func supportedDirective(kind string) bool {
	return kind == "send_enter" || kind == "resend_pointer" || kind == "respawn"
}

func safeID(value string) string {
	if validUUID(value) {
		return value
	}

	return "invalid"
}
