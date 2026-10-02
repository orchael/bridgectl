package provider

import (
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

// NewClaudeChatProvider creates a stream-JSON provider that runs
// `claude --output-format stream-json --verbose` without a PTY.
// stdout is newline-delimited JSON; the supervisor's readLoopStreamJSON
// parser extracts text and thinking deltas as typed OutputChunks, and
// (see Supervisor.observeStreamJSONInteraction) derives an authoritative
// Working/Idle interaction state from the protocol's own message_start/
// message_stop turn-boundary events.
//
// This provider does not declare ApprovalStateSupported: bridgectl does not
// currently wire a response path for Claude's permission/control-request
// protocol, so it must never claim WaitingForApproval.
func NewClaudeChatProvider() *StdioProvider {
	return NewStdioProvider(StdioConfig{
		ProviderID:     "claude-chat",
		Binary:         "claude",
		DefaultArgs:    []string{"--output-format", "stream-json", "--verbose"},
		StartupTimeout: 60 * time.Second,
		StopGrace:      10 * time.Second,
		StartupProbe:   "none",
		RequiredEnv:    []string{"CLAUDE_CODE_OAUTH_TOKEN"},
		StreamJSON:     true,
		InteractionCapabilities: bridge.InteractionCapabilities{
			InteractionStateSupported: true,
		},
	})
}
