package bridge

import (
	"context"
	"os/exec"
	"regexp"
	"time"
)

// Provider describes an interactive PTY-backed CLI provider.
type Provider interface {
	ID() string
	Binary() string
	PromptPattern() *regexp.Regexp
	StartupTimeout() time.Duration
	StopGrace() time.Duration
	BuildCommand(ctx context.Context, cfg SessionConfig) (*exec.Cmd, error)
	ValidateStartup(ctx context.Context) error
	Health(ctx context.Context) error
	Version(ctx context.Context) (string, error)
}

// EnvHealthProvider is implemented by providers that can evaluate runtime
// availability against a prepared session environment.
type EnvHealthProvider interface {
	HealthWithEnv(ctx context.Context, env []string) error
}

// RepoSetupRunner prepares a repository-specific environment before provider
// selection and launch.
type RepoSetupRunner interface {
	Prepare(ctx context.Context, repoPath string, baseEnv []string) ([]string, error)
}

// SessionConfig holds configuration for starting a new provider session.
type SessionConfig struct {
	ProjectID string
	SessionID string
	RepoPath  string
	Options   map[string]string
	Env       []string
	// Fallbacks is an ordered list of provider IDs to try if the primary
	// provider (Options["provider"]) is unavailable. At most 2 entries are
	// meaningful; extras are silently ignored.
	Fallbacks   []string
	InitialCols uint32
	InitialRows uint32
}

// SessionState represents the lifecycle state of a session.
type SessionState int

const (
	SessionStateStarting SessionState = iota + 1
	SessionStateRunning
	SessionStateAttached
	SessionStateStopping
	SessionStateStopped
	SessionStateFailed
)

// SessionInfo holds metadata about a running session.
type SessionInfo struct {
	SessionID        string
	ProjectID        string
	Provider         string
	RepoPath         string
	State            SessionState
	ProcessID        int
	CreatedAt        time.Time
	StoppedAt        time.Time
	Error            string
	Attached         bool
	AttachedClientID string
	Recovered        bool
	ExitRecorded     bool
	ExitCode         int
	OldestSeq        uint64
	LastSeq          uint64
	Cols             uint32
	Rows             uint32
	// ActiveWriterClientID is the client currently holding the writer slot.
	// Empty when no writer is attached.
	ActiveWriterClientID string
	// ObserverCount is the number of read-only observer clients currently attached.
	ObserverCount int
	// Interaction is the session's current authoritative interaction state.
	// It is a distinct axis from State (runtime lifecycle) and is never
	// inferred from it; see interaction.go.
	Interaction Interaction
	// InteractionCapabilities is the reporting provider's declared
	// capability for Interaction, duplicated onto SessionInfo so consumers
	// (doctor, the control client, Bridge) don't need a separate provider
	// lookup to know how much to trust Interaction.
	InteractionCapabilities InteractionCapabilities
}

// ChunkType classifies an OutputChunk's content.
type ChunkType uint8

const (
	// ChunkTypeOutput is raw terminal/text output (default, zero value).
	ChunkTypeOutput ChunkType = 0
	// ChunkTypeThinking carries a thinking block from a stream-JSON provider.
	ChunkTypeThinking ChunkType = 1
	// ChunkTypeWriterClaimed is a control event broadcast when a client claims
	// the writer role. It is never appended to the replay buffer.
	ChunkTypeWriterClaimed ChunkType = 2
	// ChunkTypeWriterReleased is a control event broadcast when the writer
	// releases its role. It is never appended to the replay buffer.
	ChunkTypeWriterReleased ChunkType = 3
)

// OutputChunk is one retained output chunk from an agent session.
type OutputChunk struct {
	Seq       uint64
	Timestamp time.Time
	Payload   []byte
	Type      ChunkType // defaults to ChunkTypeOutput
}

// StreamJSONProvider is implemented by providers that emit structured JSONL
// (e.g. claude --output-format stream-json) instead of raw PTY bytes.
// These providers run without a PTY and have their stdout parsed for typed
// events (text, thinking).
type StreamJSONProvider interface {
	IsStreamJSON() bool
}

// StripANSIProvider is implemented by providers that should have ANSI escape
// codes stripped from their PTY output before forwarding to clients.
type StripANSIProvider interface {
	IsStripANSI() bool
}
