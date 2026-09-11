package provider

import (
	"context"
	"fmt"
)

func (p *StdioProvider) validateStartupPrompt(_ context.Context, _ []string) error {
	return fmt.Errorf("provider %q: PTY startup probes are not supported on Windows", p.cfg.ProviderID)
}

func (p *StdioProvider) validateStartupOutput(_ context.Context, _ []string) error {
	return fmt.Errorf("provider %q: PTY startup probes are not supported on Windows", p.cfg.ProviderID)
}
