// Package work implements the one-shot Capstan Work drain protocol.
package work

import (
	"context"
	"errors"
	"time"
)

const (
	// MaxBatch is the server's claim, renew, and held-item limit.
	MaxBatch = 25
	// UserAgent identifies the Go drain at the HTTP boundary.
	UserAgent = "capstan-work-drain/1"
)

var (
	// ErrLockHeld means another one-shot drain owns the configured lock.
	ErrLockHeld = errors.New("work drain lock is already held")
	// ErrUnsupportedAction means the configured launcher cannot perform an interactive repair.
	ErrUnsupportedAction = errors.New("launcher does not support this session action")
)

// Config is the strictly validated local drain configuration.
type Config struct {
	Path                       string
	CapstanURL                 string
	Team                       string
	ActorID                    string
	CredentialFile             string
	MaxConcurrent              int
	OrchestratorCapacity       int
	MaxLoad1m                  float64
	MaxMemoryPercent           float64
	Capabilities               []string
	Roles                      []string
	RoleProviders              map[string]string
	StateFile                  string
	WorkDir                    string
	LockFile                   string
	PromptTemplate             string
	OrchestratorPromptTemplate string
	RoleCommands               map[string][]string
	RequestTimeout             time.Duration
}

// HealthSnapshot is the local admission data read once per drain run.
type HealthSnapshot struct {
	Load1m        float64
	MemoryPercent float64
}

// HealthMeasurer reads local load and memory admission data.
type HealthMeasurer interface {
	Measure(context.Context) (HealthSnapshot, error)
}

// LaunchSpec contains only local paths and process arguments. Signed links stay in LinksPath.
type LaunchSpec struct {
	Command            []string
	WorkDir            string
	PromptPath         string
	LinksPath          string
	GatePath           string
	LaunchKey          string
	ExistingSessionRef string
}

// Launcher owns local session actions. Protocol, state, API, and recovery remain in Runner.
type Launcher interface {
	Launch(context.Context, LaunchSpec) (string, error)
	Alive(context.Context, string) (bool, error)
	Terminate(context.Context, string) error
	OpenGate(context.Context, string) error
	SendEnter(context.Context, string, string) error
	ResendPointer(context.Context, string, string, string) error
}

// Logger receives redacted operational messages.
type Logger func(string)
