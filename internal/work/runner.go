package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	linksFilename  = "links.json"
	promptFilename = "prompt.md"
	gateFilename   = "launch.ready"
)

// Runner owns one complete recovery, tick, directive, and launch pass.
type Runner struct {
	config   Config
	api      WorkAPI
	launcher Launcher
	health   HealthMeasurer
	log      Logger
	store    *StateStore
}

// NewRunner builds a one-shot protocol runner.
func NewRunner(config Config, api WorkAPI, launcher Launcher, health HealthMeasurer, logger Logger) *Runner {
	if logger == nil {
		logger = func(string) {}
	}

	return &Runner{
		config: config, api: api, launcher: launcher, health: health, log: logger,
		store: NewStateStore(config.StateFile, config.WorkDir),
	}
}

// Run executes one scheduled drain tick.
func (r *Runner) Run(ctx context.Context) error {
	state, err := r.store.Load()
	if err != nil {
		return err
	}
	skipped := make(map[string]bool)
	if err := r.recover(ctx, &state, skipped); err != nil {
		return err
	}

	health, err := r.health.Measure(ctx)
	if err != nil {
		return errors.New("work admission health could not be measured")
	}
	if health.Load1m < 0 || health.MemoryPercent < 0 || health.MemoryPercent > 100 ||
		math.IsNaN(health.Load1m) || math.IsNaN(health.MemoryPercent) ||
		math.IsInf(health.Load1m, 0) || math.IsInf(health.MemoryPercent, 0) {
		return errors.New("work admission health returned invalid measurements")
	}
	leafHeld, orchestratorHeld := heldCounts(state)
	leafCapacity := max(0, r.config.MaxConcurrent-leafHeld)
	orchestratorCapacity := max(0, r.config.OrchestratorCapacity-orchestratorHeld)
	if health.Load1m > r.config.MaxLoad1m || health.MemoryPercent > r.config.MaxMemoryPercent {
		leafCapacity = 0
		orchestratorCapacity = 0
		r.log("not this round: local load or memory is above the configured ceiling")
	}
	held := reportHeld(state)
	response, err := r.api.Claim(ctx, ClaimRequest{
		ItemShape: "repository-v1",
		Capacity:  leafCapacity, OrchestratorCapacity: orchestratorCapacity,
		Roles:         append(make([]string, 0, len(r.config.Roles)), r.config.Roles...),
		Capabilities:  append(make([]string, 0, len(r.config.Capabilities)), r.config.Capabilities...),
		RoleProviders: cloneStringMap(r.config.RoleProviders), CurrentClaimedItemCount: leafHeld, HeldItems: held,
	})
	if err != nil {
		return err
	}
	if (r.config.Launcher == "solo" || r.config.Launcher == "hybrid") && (len(response.Items) > 0 || len(response.Directives) > 0) &&
		(response.SpokeID == nil || *response.SpokeID < 1) {
		return errors.New("work claim response omitted the authoritative spoke id")
	}
	if (r.config.Launcher == "solo" || r.config.Launcher == "hybrid") && response.SpokeID != nil {
		for _, item := range state.Items {
			if item.SpokeID != 0 && item.SpokeID != *response.SpokeID {
				return errors.New("work claim response changed the authoritative spoke id")
			}
		}
	}
	leafClaims, orchestratorClaims := 0, 0
	for _, item := range response.Items {
		if item.Role == "orchestrator" {
			orchestratorClaims++
		} else {
			leafClaims++
		}
	}
	if len(response.Items) > leafCapacity+orchestratorCapacity || leafClaims > leafCapacity || orchestratorClaims > orchestratorCapacity {
		return errors.New("work claim response exceeded advertised capacity")
	}
	if err := r.acknowledgeReported(&state); err != nil {
		return err
	}
	directives := make([]Directive, 0, len(response.Directives))
	for _, directive := range response.Directives {
		if skipped[directive.ItemID] {
			continue
		}
		directives = append(directives, directive)
	}
	if err := r.handleDirectives(ctx, &state, directives); err != nil {
		return err
	}
	for _, claim := range response.Items {
		spokeID := int64(0)
		if response.SpokeID != nil {
			spokeID = *response.SpokeID
		}
		if err := r.startClaim(ctx, &state, claim, spokeID); err != nil {
			r.log(fmt.Sprintf("launch failed: item_id=%s", safeID(claim.ID)))
		}
	}

	return nil
}

func (r *Runner) recover(ctx context.Context, state *State, skipped map[string]bool) error {
	ids := sortedItemIDs(*state)
	for offset := 0; offset < len(ids); offset += MaxBatch {
		end := min(offset+MaxBatch, len(ids))
		batchIDs := ids[offset:end]
		requests := make([]RenewRequestItem, 0, len(batchIDs))
		for _, id := range batchIDs {
			requests = append(requests, RenewRequestItem{ItemID: id, Fence: state.Items[id].Fence})
		}
		results, err := r.api.Renew(ctx, requests)
		if err != nil {
			return err
		}
		byID := make(map[string]RenewResult, len(results))
		for _, result := range results {
			if _, duplicate := byID[result.ItemID]; duplicate {
				return errors.New("work renew response contains duplicate item results")
			}
			byID[result.ItemID] = result
		}
		for _, id := range batchIDs {
			item := state.Items[id]
			result, ok := byID[id]
			if !ok {
				return fmt.Errorf("work renew response omitted item_id=%s", id)
			}
			switch result.Status {
			case "lost":
				if item.SessionRef != "" && item.Role != "orchestrator" && !isAmpSession(item.SessionRef) {
					alive, aliveErr := r.launcher.Alive(ctx, item.SessionRef)
					if aliveErr != nil {
						return fmt.Errorf("local session health failed: item_id=%s", id)
					}
					if alive {
						if err := r.launcher.Terminate(ctx, item.SessionRef); err != nil {
							return fmt.Errorf("lost-authority session stop failed: item_id=%s", id)
						}
					}
				}
				if err := r.forget(state, id); err != nil {
					return err
				}
			case "renewed":
				if result.Item == nil || result.Item.ID != id || result.Item.Fence != item.Fence || result.Item.Role != item.Role {
					return fmt.Errorf("work renew response is invalid: item_id=%s", id)
				}
				if err := validateLinks(item.Role, result.Item.Links); err != nil {
					return fmt.Errorf("work renew links are invalid: item_id=%s", id)
				}
				if r.ampRole(item.Role) || isAmpSession(item.SessionRef) {
					if result.Item.Repository == nil || *result.Item.Repository != item.Repository || !validRepository(item.Repository) {
						skipped[id] = true
						if err := r.releaseChangedRepository(ctx, state, id); err != nil {
							return err
						}
						continue
					}
				}
				if err := r.writeLinks(item, result.Item.Links); err != nil {
					return err
				}
				if err := r.advanceFromServer(state, id, result.Item.State); err != nil {
					return err
				}
				if item.Phase == "awaiting_verification" {
					continue
				}
				if (item.Phase == "routed" && isAmpSession(item.SessionRef)) || item.CorrectionPending {
					if err := r.deliverSteers(ctx, state, id, result.Item.Links["steers"]); err != nil {
						if errors.Is(err, errSteerPersistence) {
							return err
						}
						skipped[id] = true
						r.log(fmt.Sprintf("steer delivery deferred: item_id=%s fence=%d", safeID(id), item.Fence))
						continue
					}
				}
				if item.SessionRef == "" && strings.HasPrefix(item.PendingLaunchKey, "initial:") {
					if err := r.establish(ctx, state, id, item.PendingLaunchKey, ""); err != nil {
						if errors.Is(err, errAmpDispatchUncertain) {
							r.log(fmt.Sprintf("amp initialization deferred: item_id=%s fence=%d", safeID(id), item.Fence))
							continue
						}
						return r.releaseAfterLaunchFailure(ctx, state, id, err)
					}
				}
				if item.SessionRef == "" {
					continue
				}
				alive, aliveErr := true, error(nil)
				if !isAmpSession(item.SessionRef) {
					alive, aliveErr = r.launcher.Alive(ctx, item.SessionRef)
				}
				if aliveErr != nil {
					return fmt.Errorf("local session health failed: item_id=%s", id)
				}
				if !alive {
					if err := r.release(ctx, state, id); err != nil {
						return err
					}
					continue
				}
				if err := r.ensureRouted(ctx, state, id); err != nil {
					return err
				}
			default:
				return fmt.Errorf("work renew response has invalid status: item_id=%s", id)
			}
		}
	}

	return nil
}

func (r *Runner) releaseChangedRepository(ctx context.Context, state *State, id string) error {
	item := state.Items[id]
	err := r.api.Transition(ctx, TransitionRequest{ItemID: id, Fence: item.Fence, State: "released"})
	if err != nil {
		var apiErr APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 409 {
			r.log(fmt.Sprintf("repository binding release deferred: item_id=%s fence=%d", safeID(id), item.Fence))
			return nil
		}
	}
	if err := r.forget(state, id); err != nil {
		return err
	}
	r.log(fmt.Sprintf("repository binding released: item_id=%s fence=%d", safeID(id), item.Fence))
	return nil
}

func (r *Runner) startClaim(ctx context.Context, state *State, claim ClaimItem, spokeID int64) error {
	if err := r.validateClaim(claim); err != nil {
		return err
	}
	if _, exists := state.Items[claim.ID]; exists {
		return errors.New("claim response repeated a held item")
	}
	if r.ampRole(claim.Role) && (claim.Repository == nil || !validRepository(*claim.Repository)) {
		r.log("Amp claim released: missing or invalid explicit repository; item_id=" + safeID(claim.ID))
		return r.api.Transition(ctx, TransitionRequest{ItemID: claim.ID, Fence: claim.Fence, State: "released"})
	}
	workDir := filepath.Join(r.config.WorkDir, fmt.Sprintf("%s-f%d", claim.ID, claim.Fence))
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return errors.New("item work directory could not be created")
	}
	if err := os.Chmod(workDir, 0o700); err != nil {
		return errors.New("item work directory could not be secured")
	}
	item := &ItemState{
		TaskID: claim.TaskID, Role: claim.Role, Fence: claim.Fence, Phase: "claimed", WorkDir: workDir,
		LaunchNonce: claim.LaunchNonce, SeatFence: claim.SeatFence, SpokeID: spokeID, PendingLaunchKey: "initial:" + claim.LaunchNonce,
	}
	if r.ampRole(claim.Role) {
		item.Repository = *claim.Repository
	}
	state.Items[claim.ID] = item
	if err := r.writeLinks(item, claim.Links); err != nil {
		delete(state.Items, claim.ID)
		return r.rejectUnstartedClaim(ctx, claim, workDir, err)
	}
	if err := r.writePrompt(claim.ID, item, claim.SessionRef); err != nil {
		delete(state.Items, claim.ID)
		return r.rejectUnstartedClaim(ctx, claim, workDir, err)
	}
	if err := r.store.Save(*state); err != nil {
		delete(state.Items, claim.ID)
		return r.rejectUnstartedClaim(ctx, claim, workDir, err)
	}
	if err := r.establish(ctx, state, claim.ID, "initial:"+claim.LaunchNonce, claim.SessionRef); err != nil {
		return r.releaseAfterLaunchFailure(ctx, state, claim.ID, err)
	}
	if err := r.ensureRouted(ctx, state, claim.ID); err != nil {
		return err
	}
	r.log(fmt.Sprintf("launched item_id=%s task_id=%s role=%s", claim.ID, claim.TaskID, claim.Role))

	return nil
}

func (r *Runner) establish(ctx context.Context, state *State, id, launchKey, existingSession string) error {
	item := state.Items[id]
	spec, err := r.launchSpec(id, item, launchKey, existingSession)
	if err != nil {
		return err
	}
	sessionRef, err := r.launcher.Launch(ctx, spec)
	if err != nil {
		if errors.Is(err, errAmpDispatchUncertain) {
			return errAmpDispatchUncertain
		}
		if r.ampRole(item.Role) {
			if _, markerErr := os.Stat(filepath.Join(item.WorkDir, "amp-"+item.LaunchNonce+".started")); markerErr == nil {
				return errAmpDispatchUncertain
			}
		}
		return errors.New("launcher could not establish a session")
	}
	if sessionRef == "" || len(sessionRef) > 2048 {
		return errors.New("launcher returned an invalid session ref")
	}
	item.SessionRef = sessionRef
	item.Phase = "launched"
	item.PendingLaunchKey = ""
	if err := r.store.Save(*state); err != nil {
		_ = r.launcher.Terminate(ctx, sessionRef)
		return err
	}

	return nil
}

func (r *Runner) ensureRouted(ctx context.Context, state *State, id string) error {
	item := state.Items[id]
	if item.Phase == "launched" {
		if err := r.api.Transition(ctx, TransitionRequest{
			ItemID: id, Fence: item.Fence, State: "delivered", LaunchNonce: item.LaunchNonce, SessionRef: item.SessionRef,
		}); err != nil {
			return err
		}
		item.Phase = "delivered"
		if err := r.store.Save(*state); err != nil {
			return err
		}
	}
	if item.Phase == "delivered" {
		if err := r.api.Transition(ctx, TransitionRequest{ItemID: id, Fence: item.Fence, State: "routed"}); err != nil {
			return err
		}
		item.Phase = "routed"
		if err := r.store.Save(*state); err != nil {
			return err
		}
	}
	if item.Phase == "routed" {
		if err := r.launcher.OpenGate(ctx, item.SessionRef, filepath.Join(item.WorkDir, gateFilename), item.LaunchNonce); err != nil {
			return errors.New("launcher gate could not be opened")
		}
	}

	return nil
}

func (r *Runner) advanceFromServer(state *State, id, serverPhase string) error {
	item := state.Items[id]
	if serverPhase == "awaiting_verification" {
		if item.SessionRef == "" {
			return errors.New("waiting item has no session")
		}
		item.Phase = serverPhase
		return r.store.Save(*state)
	}
	if item.Phase == "awaiting_verification" && serverPhase == "routed" {
		item.Phase = "routed"
		item.CorrectionPending = true
		return r.store.Save(*state)
	}
	if item.Phase == "awaiting_verification" {
		return errors.New("work renew regressed a waiting item")
	}
	serverIndex := phaseIndex(serverPhase)
	localIndex := phaseIndex(item.Phase)
	if serverIndex < 0 {
		return errors.New("work renew response contains an invalid item state")
	}
	if item.SessionRef == "" && item.PendingLaunchKey != "" {
		return nil
	}
	if serverIndex > localIndex {
		item.Phase = []string{"claimed", "delivered", "routed"}[serverIndex]
		return r.store.Save(*state)
	}

	return nil
}

func phaseIndex(phase string) int {
	switch phase {
	case "claimed", "launched":
		return 0
	case "delivered":
		return 1
	case "routed":
		return 2
	default:
		return -1
	}
}

func (r *Runner) releaseAfterLaunchFailure(ctx context.Context, state *State, id string, cause error) error {
	if errors.Is(cause, errAmpDispatchUncertain) {
		return cause
	}
	item := state.Items[id]
	if item != nil && item.SessionRef != "" {
		_ = r.launcher.Terminate(ctx, item.SessionRef)
	}
	if err := r.release(ctx, state, id); err != nil {
		return errors.Join(cause, err)
	}

	return cause
}

func (r *Runner) rejectUnstartedClaim(ctx context.Context, claim ClaimItem, workDir string, cause error) error {
	_ = os.RemoveAll(workDir)
	if validUUID(claim.ID) && claim.Fence > 0 {
		if err := r.api.Transition(ctx, TransitionRequest{ItemID: claim.ID, Fence: claim.Fence, State: "released"}); err != nil {
			return errors.Join(cause, err)
		}
	}

	return cause
}

func (r *Runner) release(ctx context.Context, state *State, id string) error {
	item := state.Items[id]
	err := r.api.Transition(ctx, TransitionRequest{ItemID: id, Fence: item.Fence, State: "released"})
	if err != nil {
		var apiErr APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 409 {
			return err
		}
	}
	if err := r.forget(state, id); err != nil {
		return err
	}
	r.log(fmt.Sprintf("released item_id=%s after local session exit", id))

	return nil
}

func (r *Runner) forget(state *State, id string) error {
	item := state.Items[id]
	delete(state.Items, id)
	if err := r.store.Save(*state); err != nil {
		state.Items[id] = item
		return err
	}
	if item != nil {
		_ = os.RemoveAll(item.WorkDir)
	}

	return nil
}

func (r *Runner) handleDirectives(ctx context.Context, state *State, directives []Directive) error {
	seen := make(map[string]struct{}, len(directives))
	for _, directive := range directives {
		if _, duplicate := seen[directive.ID]; duplicate {
			return errors.New("work claim response contains a duplicate directive")
		}
		seen[directive.ID] = struct{}{}
		item, ok := state.Items[directive.ItemID]
		if !ok || directive.Fence != item.Fence || !validUUID(directive.ID) || !supportedDirective(directive.Kind) {
			return errors.New("work claim response contains an invalid directive")
		}
		if item.Directive != nil && item.Directive.ID == directive.ID && item.Directive.Status != "pending" {
			continue
		}
		if r.ampRole(item.Role) && directive.Kind == "respawn" && directive.Payload.LaunchNonce == item.LaunchNonce &&
			(item.Directive == nil || item.Directive.ID != directive.ID) {
			return errors.New("amp respawn directive did not rotate the launch nonce")
		}
		item.Directive = &DirectiveState{ID: directive.ID, Kind: directive.Kind, Status: "pending"}
		if err := r.store.Save(*state); err != nil {
			return err
		}
		err := r.executeDirective(ctx, state, directive)
		if err != nil {
			item.Directive.Status = "failed"
		} else {
			item.Directive.Status = "succeeded"
		}
		item.Directive.ReportPending = true
		if saveErr := r.store.Save(*state); saveErr != nil {
			return saveErr
		}
	}

	return nil
}

func (r *Runner) executeDirective(ctx context.Context, state *State, directive Directive) error {
	item := state.Items[directive.ItemID]
	switch directive.Kind {
	case "nudge":
		if !validSessionMessage(directive.Payload.Message) {
			return errors.New("invalid nudge message")
		}
		if launcher, ok := r.launcher.(MessageLauncher); ok {
			return launcher.Continue(ctx, item.SessionRef, directive.Payload.Message)
		}
		return ErrUnsupportedAction
	case "send_enter":
		return r.launcher.SendEnter(ctx, item.SessionRef, filepath.Join(item.WorkDir, linksFilename), item.LaunchNonce, directive.ID)
	case "resend_pointer":
		if err := validateLinks(item.Role, directive.Links); err != nil {
			return err
		}
		if err := r.writeLinks(item, directive.Links); err != nil {
			return err
		}
		return r.launcher.ResendPointer(ctx, item.SessionRef, filepath.Join(item.WorkDir, linksFilename), item.LaunchNonce, directive.ID)
	case "respawn":
		if !validUUIDv7(directive.Payload.LaunchNonce) {
			return errors.New("respawn directive has an invalid launch nonce")
		}
		if err := validateLinks(item.Role, directive.Links); err != nil {
			return err
		}
		if item.SessionRef != "" {
			alive, err := r.launcher.Alive(ctx, item.SessionRef)
			if err != nil {
				return err
			}
			if alive {
				if err := r.launcher.Terminate(ctx, item.SessionRef); err != nil {
					return err
				}
			}
		}
		if err := r.writeLinks(item, directive.Links); err != nil {
			return err
		}
		item.LaunchNonce = directive.Payload.LaunchNonce
		item.SessionRef = ""
		item.Phase = "claimed"
		item.PendingLaunchKey = "directive:" + directive.ID
		if err := r.store.Save(*state); err != nil {
			return err
		}
		if err := r.establish(ctx, state, directive.ItemID, "directive:"+directive.ID, ""); err != nil {
			return err
		}
		if err := r.launcher.OpenGate(ctx, item.SessionRef, filepath.Join(item.WorkDir, gateFilename), item.LaunchNonce); err != nil {
			_ = r.launcher.Terminate(ctx, item.SessionRef)
			item.SessionRef = ""
			item.Phase = "claimed"
			item.PendingLaunchKey = "directive:" + directive.ID
			_ = r.store.Save(*state)
			return err
		}

		return nil
	default:
		return errors.New("unsupported directive")
	}
}

func (r *Runner) acknowledgeReported(state *State) error {
	changed := false
	for _, item := range state.Items {
		if item.Directive != nil && item.Directive.ReportPending {
			item.Directive.ReportPending = false
			changed = true
		}
	}
	if changed {
		return r.store.Save(*state)
	}

	return nil
}

func (r *Runner) validateClaim(claim ClaimItem) error {
	if !validUUID(claim.ID) || !validUUID(claim.TaskID) || !validUUIDv7(claim.LaunchNonce) || claim.Fence < 1 {
		return errors.New("claim response contains invalid item identity")
	}
	if r.config.Launcher == "solo" || r.config.Launcher == "hybrid" {
		if r.ampRole(claim.Role) {
			if claim.SessionRef != "" {
				return errors.New("claim response tried to reuse a leaf session")
			}
			return validateLinks(claim.Role, claim.Links)
		}
		if _, ok := r.config.SoloBindings[claim.Role]; !ok {
			return errors.New("claim response contains an unsupported role")
		}
		if claim.Role != "orchestrator" && claim.SessionRef != "" {
			return errors.New("claim response tried to reuse a leaf session")
		}
	} else if _, ok := r.config.RoleCommands[claim.Role]; !ok {
		return errors.New("claim response contains an unsupported role")
	}
	return validateLinks(claim.Role, claim.Links)
}

func (r *Runner) writeLinks(item *ItemState, links map[string]string) error {
	data, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		return errors.New("signed links could not be encoded")
	}
	data = append(data, '\n')
	if err := atomicPrivateWrite(filepath.Join(item.WorkDir, linksFilename), data); err != nil {
		return errors.New("private signed-link file could not be written")
	}

	return nil
}

func (r *Runner) writePrompt(id string, item *ItemState, existingSession string) error {
	templatePath := r.config.PromptTemplate
	if item.Role == "orchestrator" {
		templatePath = r.config.OrchestratorPromptTemplate
	}
	template, err := os.ReadFile(templatePath)
	if err != nil {
		return errors.New("prompt template could not be read")
	}
	values := r.values(id, item, existingSession)
	prompt, err := render(string(template), values)
	if err != nil {
		return err
	}
	// Supply the carrier contract even for templates saved by older CLI versions.
	// Amp and legacy process prompts keep their existing behavior.
	if (r.config.Launcher == "solo" || r.config.Launcher == "hybrid") && !r.ampRole(item.Role) {
		carrier, err := render(soloPromptTemplate(item.Role), values)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(prompt, carrier) {
			prompt = carrier + "\n" + prompt
		}
	}
	if err := writePrivate(filepath.Join(item.WorkDir, promptFilename), prompt); err != nil {
		return errors.New("private prompt file could not be written")
	}

	return nil
}

func soloPromptTemplate(role string) string {
	prompt := "First read the brief URL from the brief key in {links_file} and fetch that signed brief with delivery nonce {launch_nonce}. Follow the brief.\n" +
		"Every other signed action URL is in the same item-local file {links_file}, keyed by action.\n" +
		"The drain renews links.json every minute. Re-read {links_file} immediately before every signed call; never cache a URL.\n" +
		"Do not re-fetch the brief as a link-refresh mechanism: brief reads settle receipt/stall evidence.\n"
	if role == "orchestrator" {
		return prompt + "Use the orchestration and orchestrate links for verdicts and child actions.\n"
	}
	return prompt + "Use the relevant milestone, steers, and evidence links for leaf work.\n"
}

func (r *Runner) launchSpec(id string, item *ItemState, key, existing string) (LaunchSpec, error) {
	values := r.values(id, item, existing)
	var command []string
	if r.config.Launcher == "process" || r.config.Launcher == "" {
		command = make([]string, len(r.config.RoleCommands[item.Role]))
		for index, part := range r.config.RoleCommands[item.Role] {
			rendered, err := render(part, values)
			if err != nil {
				return LaunchSpec{}, err
			}
			command[index] = rendered
		}
	}

	return LaunchSpec{
		Command: command, WorkDir: item.WorkDir, PromptPath: filepath.Join(item.WorkDir, promptFilename),
		LinksPath: filepath.Join(item.WorkDir, linksFilename), GatePath: filepath.Join(item.WorkDir, gateFilename),
		LaunchKey: key, ExistingSessionRef: existing, Role: item.Role, SpokeID: item.SpokeID,
		LaunchNonce: item.LaunchNonce, SoloBinding: r.config.SoloBindings[item.Role],
		Repository: item.Repository, ItemID: id,
	}, nil
}

func (r *Runner) ampRole(role string) bool {
	if r.config.Launcher != "hybrid" || role == "orchestrator" {
		return false
	}
	for _, configured := range r.config.AmpRoles {
		if configured == role {
			return true
		}
	}
	return false
}

func isAmpSession(ref string) bool { return strings.HasPrefix(ref, "amp:v1:") }

func (r *Runner) values(id string, item *ItemState, session string) map[string]string {
	return map[string]string{
		"config_dir": filepath.Dir(r.config.Path), "fence": strconv.Itoa(item.Fence), "item_id": id,
		"launch_nonce": item.LaunchNonce, "links_file": filepath.Join(item.WorkDir, linksFilename),
		"prompt_file": filepath.Join(item.WorkDir, promptFilename), "role": item.Role,
		"seat_fence": strconv.Itoa(item.SeatFence), "session_ref": session, "task_id": item.TaskID, "work_dir": item.WorkDir,
	}
}

func render(template string, values map[string]string) (string, error) {
	if err := validateTemplate(template); err != nil {
		return "", err
	}
	return placeholderRE.ReplaceAllStringFunc(template, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(match, "{"), "}")
		return values[name]
	}), nil
}

func reportHeld(state State) []HeldItem {
	ids := sortedItemIDs(state)
	held := make([]HeldItem, 0, len(ids))
	for _, id := range ids {
		item := state.Items[id]
		report := HeldItem{ItemID: id, Fence: item.Fence}
		if item.LaunchNonce != "" && item.SessionRef != "" {
			report.LaunchNonce = item.LaunchNonce
			report.SessionRef = item.SessionRef
		}
		if item.Directive != nil && item.Directive.ReportPending {
			report.DirectiveID = item.Directive.ID
			report.DirectiveOutcome = item.Directive.Status
		}
		held = append(held, report)
	}

	return held
}

func heldCounts(state State) (int, int) {
	leaf, orchestrator := 0, 0
	for _, item := range state.Items {
		if item.Role == "orchestrator" {
			orchestrator++
		} else {
			leaf++
		}
	}

	return leaf, orchestrator
}

func sortedItemIDs(state State) []string {
	ids := make([]string, 0, len(state.Items))
	for id := range state.Items {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	return ids
}
