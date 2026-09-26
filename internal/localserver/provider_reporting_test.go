package localserver

import (
	"testing"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/provider"
)

func TestDefaultProvidersReportInteraction(t *testing.T) {
	for _, id := range []string{"claude", "codex", "codex-app-server"} {
		t.Run(id, func(t *testing.T) {
			p := sessionProvider(provider.StdioConfig{ProviderID: id, Binary: id}, "")
			if _, ok := p.(bridge.InteractionWatcher); !ok {
				t.Fatalf("%s has no structured watcher", id)
			}
			if !p.(bridge.InteractionCapableProvider).InteractionCapabilities().InteractionStateSupported {
				t.Fatal("missing capability")
			}
			if p.ID() != id {
				t.Fatalf("provider identity changed: %s", p.ID())
			}
			plain := sessionProvider(provider.StdioConfig{ProviderID: id, Binary: id}, "stdio")
			if plain.(bridge.InteractionCapableProvider).InteractionCapabilities().InteractionStateSupported {
				t.Fatal("plain stdio falsely claims support")
			}
		})
	}
}
