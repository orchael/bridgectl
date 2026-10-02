package localserver

import (
	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/provider"
)

// Shared by configured and auto-detected providers so an explicit binary or
// argument list cannot accidentally disable structured session reporting.
func sessionProvider(sc provider.StdioConfig, transport string) bridge.Provider {
	if transport != "stdio" && !sc.StreamJSON {
		switch sc.ProviderID {
		case "claude":
			return provider.NewClaudeHooksProvider(sc)
		case "codex", "codex-app-server":
			return provider.NewCodexAppServerProvider(sc)
		}
	}
	if sc.ProviderID == "codex" || sc.ProviderID == "codex-app-server" {
		return provider.NewCodexProvider(sc)
	}
	return provider.NewStdioProvider(sc)
}
