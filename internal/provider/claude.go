package provider

import (
	"context"
	"os"

	"github.com/orchael/bridgectl/internal/bridge"
)

// ClaudeProvider wraps StdioProvider with Claude-specific auth handling.
// It accepts either CLAUDE_CODE_OAUTH_TOKEN or the credentials the `claude`
// CLI stores for itself (see resolveClaudeAuth) as valid authentication,
// mirroring CodexProvider's treatment of a desktop-local auth.json.
//
// Unlike Codex, no credential is ever seeded or rewritten: the CLI owns its
// store and reads it directly, so BuildCommand needs no override.
type ClaudeProvider struct {
	*StdioProvider
	// resolveAuth is injectable so tests cover every credential branch
	// deterministically. The default probe shells out to `security`, whose
	// login-keychain path derives from HOME, which would otherwise make
	// provider-level results depend on the host's real keychain.
	resolveAuth func(env []string) (claudeAuthSource, error)
}

// NewClaudeProvider creates the interactive PTY-backed Claude provider.
// RequiredEnv is cleared from the underlying StdioConfig because
// ClaudeProvider validates auth itself — a configured
// required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] would otherwise reject a host
// that is authenticated through the CLI's own credential store.
func NewClaudeProvider(cfg StdioConfig) *ClaudeProvider {
	cfg.RequiredEnv = nil // handled by ClaudeProvider
	return &ClaudeProvider{
		StdioProvider: NewStdioProvider(cfg),
		resolveAuth:   resolveClaudeAuth,
	}
}

// ValidateStartup resolves auth explicitly before probing. CodexProvider gets
// this for free because its BuildCommand seeds CODEX_HOME and fails when no
// credential resolves; Claude seeds nothing, so without this check a host with
// no credentials at all would pass validation and fail later at launch.
func (p *ClaudeProvider) ValidateStartup(ctx context.Context) error {
	cmd, err := p.BuildCommand(ctx, bridge.SessionConfig{})
	if err != nil {
		return err
	}
	if _, err := p.resolveAuth(cmd.Env); err != nil {
		return err
	}
	return p.validateStartupWithEnv(ctx, cmd.Env)
}

func (p *ClaudeProvider) Health(ctx context.Context) error {
	return p.HealthWithEnv(ctx, FilterEnv(os.Environ()))
}

func (p *ClaudeProvider) HealthWithEnv(ctx context.Context, env []string) error {
	if _, err := p.resolveAuth(env); err != nil {
		return err
	}
	return p.StdioProvider.HealthWithEnv(ctx, env)
}
