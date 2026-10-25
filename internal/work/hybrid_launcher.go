package work

import (
	"context"
	"strings"
)

// HybridLauncher chooses a leaf lane only from explicit configuration.
type HybridLauncher struct {
	solo  Launcher
	amp   *AmpLauncher
	roles map[string]bool
}

func NewHybridLauncher(solo Launcher, amp *AmpLauncher, roles []string) *HybridLauncher {
	l := &HybridLauncher{solo: solo, amp: amp, roles: make(map[string]bool)}
	for _, role := range roles {
		l.roles[role] = true
	}
	return l
}
func (l *HybridLauncher) session(ref string) Launcher {
	if strings.HasPrefix(ref, "amp:") {
		return l.amp
	}
	return l.solo
}
func (l *HybridLauncher) Launch(ctx context.Context, spec LaunchSpec) (string, error) {
	if l.roles[spec.Role] {
		return l.amp.Launch(ctx, spec)
	}
	return l.solo.Launch(ctx, spec)
}
func (l *HybridLauncher) Alive(ctx context.Context, ref string) (bool, error) {
	return l.session(ref).Alive(ctx, ref)
}
func (l *HybridLauncher) Terminate(ctx context.Context, ref string) error {
	return l.session(ref).Terminate(ctx, ref)
}
func (l *HybridLauncher) OpenGate(ctx context.Context, ref, path, nonce string) error {
	return l.session(ref).OpenGate(ctx, ref, path, nonce)
}
func (l *HybridLauncher) SendEnter(ctx context.Context, ref, path, nonce, id string) error {
	return l.session(ref).SendEnter(ctx, ref, path, nonce, id)
}
func (l *HybridLauncher) ResendPointer(ctx context.Context, ref, path, nonce, id string) error {
	return l.session(ref).ResendPointer(ctx, ref, path, nonce, id)
}
func (l *HybridLauncher) Continue(ctx context.Context, ref, text string) error {
	if m, ok := l.session(ref).(MessageLauncher); ok {
		return m.Continue(ctx, ref, text)
	}
	return ErrUnsupportedAction
}
