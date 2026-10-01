package bridgelib

import (
	"context"
	"fmt"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/provider"
)

type ProviderConfig struct {
	ID             string
	Binary         string
	Args           []string
	StartupTimeout time.Duration
	StopGrace      time.Duration
	PromptPattern  string
	RequiredEnv    []string
}

type Config struct {
	Providers             []ProviderConfig
	AllowedPaths          []string
	MaxSessions           int
	MaxSessionsPerProject int
	IdleTimeout           time.Duration
	OutputBufferBytes     int
}

type SessionInfo = bridge.SessionInfo
type OutputChunk = bridge.OutputChunk

type AttachState struct {
	ClientID  string
	Replay    []OutputChunk
	Live      <-chan OutputChunk
	ReplayGap bool
}

type Bridge struct {
	supervisor *bridge.Supervisor
	registry   *bridge.Registry
}

func New(cfg Config) (*Bridge, error) {
	registry := bridge.NewRegistry()
	providers := cfg.Providers
	if len(providers) == 0 {
		providers = []ProviderConfig{{ID: "claude"}}
	}
	for _, pc := range providers {
		prov := provider.NewStdioProvider(provider.StdioConfig{
			ProviderID:     pc.ID,
			Binary:         pc.Binary,
			DefaultArgs:    pc.Args,
			StartupTimeout: pc.StartupTimeout,
			StopGrace:      pc.StopGrace,
			StartupProbe:   "prompt",
			PromptPattern:  pc.PromptPattern,
			RequiredEnv:    pc.RequiredEnv,
		})
		if err := registry.Register(prov); err != nil {
			return nil, fmt.Errorf("register provider %q: %w", pc.ID, err)
		}
	}

	policy := bridge.Policy{
		MaxPerProject: cfg.MaxSessionsPerProject,
		MaxGlobal:     cfg.MaxSessions,
		MaxInputBytes: 65536,
		AllowedPaths:  cfg.AllowedPaths,
	}
	if policy.MaxPerProject == 0 {
		policy.MaxPerProject = 5
	}
	if policy.MaxGlobal == 0 {
		policy.MaxGlobal = 20
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 24 * time.Hour
	}
	if cfg.OutputBufferBytes == 0 {
		cfg.OutputBufferBytes = 8 << 20
	}
	return &Bridge{
		supervisor: bridge.NewSupervisor(registry, policy, cfg.OutputBufferBytes, cfg.IdleTimeout),
		registry:   registry,
	}, nil
}

func (b *Bridge) StartSession(ctx context.Context, projectID, sessionID, repoPath, providerID string, opts map[string]string) (*SessionInfo, error) {
	options := map[string]string{"provider": providerID}
	for k, v := range opts {
		options[k] = v
	}
	return b.supervisor.Start(ctx, bridge.SessionConfig{
		ProjectID:   projectID,
		SessionID:   sessionID,
		RepoPath:    repoPath,
		Options:     options,
		InitialCols: 120,
		InitialRows: 40,
	})
}

func (b *Bridge) Stop(sessionID string, force bool) error    { return b.supervisor.Stop(sessionID, force) }
func (b *Bridge) Get(sessionID string) (*SessionInfo, error) { return b.supervisor.Get(sessionID) }
func (b *Bridge) List(projectID string) []SessionInfo        { return b.supervisor.List(projectID) }
func (b *Bridge) WriteInput(sessionID, clientID string, data []byte) (int, error) {
	return b.supervisor.WriteInput(sessionID, clientID, data)
}
func (b *Bridge) ResizeSession(sessionID, clientID string, cols, rows uint32) error {
	return b.supervisor.Resize(sessionID, clientID, cols, rows)
}
func (b *Bridge) AttachSession(sessionID, clientID string, afterSeq uint64) (*bridge.AttachState, error) {
	return b.supervisor.Attach(sessionID, clientID, afterSeq, bridge.AttachRoleWriter)
}

func (b *Bridge) AttachSessionObserver(sessionID, clientID string, afterSeq uint64) (*bridge.AttachState, error) {
	return b.supervisor.Attach(sessionID, clientID, afterSeq, bridge.AttachRoleObserver)
}
