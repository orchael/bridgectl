package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/claudehooks"
)

// ClaudeHooksProvider keeps the native terminal and observes session-local
// hooks. Observation works without a telemetry collector or remote service.
type ClaudeHooksProvider struct {
	*StdioProvider
	mu       sync.Mutex
	sessions map[string]*claudehooks.Observer
}

func NewClaudeHooksProvider(cfg StdioConfig) *ClaudeHooksProvider {
	cfg.InteractionCapabilities = claudehooks.Capabilities()
	return &ClaudeHooksProvider{StdioProvider: NewStdioProvider(cfg), sessions: make(map[string]*claudehooks.Observer)}
}

func (p *ClaudeHooksProvider) BuildCommand(ctx context.Context, cfg bridge.SessionConfig) (*exec.Cmd, error) {
	cmd, err := p.StdioProvider.BuildCommand(ctx, cfg)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	observerCtx, cancel := context.WithCancel(ctx)
	o, err := claudehooks.New(observerCtx, executable)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude session hooks: %w", err)
	}
	args, err := mergeClaudeSettings(cmd.Args, o.SettingsPath, cfg.RepoPath)
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Args = args
	p.mu.Lock()
	if _, exists := p.sessions[cfg.SessionID]; exists {
		p.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("claude observer already exists for session")
	}
	p.sessions[cfg.SessionID] = o
	p.mu.Unlock()
	go func() {
		<-observerCtx.Done()
		cancel()
		p.mu.Lock()
		delete(p.sessions, cfg.SessionID)
		p.mu.Unlock()
	}()
	return cmd, nil
}

func (p *ClaudeHooksProvider) WatchInteraction(_ context.Context, id string, _ bridge.SessionConfig) (<-chan bridge.Interaction, error) {
	p.mu.Lock()
	o := p.sessions[id]
	p.mu.Unlock()
	if o == nil {
		return nil, fmt.Errorf("claude: no hook observer for session %q", id)
	}
	return o.Updates, nil
}

// Preserve user CLI settings and hooks without modifying their source files.
func mergeClaudeSettings(args []string, generated, repo string) ([]string, error) {
	data, err := os.ReadFile(generated)
	if err != nil {
		return nil, err
	}
	var base map[string]json.RawMessage
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, err
	}
	var hooks map[string][]json.RawMessage
	if err := json.Unmarshal(base["hooks"], &hooks); err != nil {
		return nil, err
	}
	merged := map[string]json.RawMessage{}
	var out []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = args[i:]
			break
		}
		value := ""
		if arg == "--settings" {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("claude --settings requires a value")
			}
			value = args[i]
		} else if strings.HasPrefix(arg, "--settings=") {
			value = strings.TrimPrefix(arg, "--settings=")
		} else {
			out = append(out, arg)
			continue
		}
		data := []byte(value)
		if !strings.HasPrefix(strings.TrimSpace(value), "{") {
			path := value
			if !filepath.IsAbs(path) {
				path = filepath.Join(repo, path)
			}
			data, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read Claude settings: %w", err)
			}
		}
		var user map[string]json.RawMessage
		if err := json.Unmarshal(data, &user); err != nil || user == nil {
			return nil, fmt.Errorf("invalid Claude settings object")
		}
		for key, val := range user {
			if key == "hooks" {
				var additional map[string][]json.RawMessage
				if err := json.Unmarshal(val, &additional); err != nil {
					return nil, fmt.Errorf("invalid Claude hooks")
				}
				for event, entries := range additional {
					hooks[event] = append(entries, hooks[event]...)
				}
			} else {
				merged[key] = val
			}
		}
	}
	merged["hooks"], err = json.Marshal(hooks)
	if err != nil {
		return nil, err
	}
	data, err = json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(generated, data, 0o600); err != nil {
		return nil, err
	}
	return append(append(out, "--settings", generated), positional...), nil
}
