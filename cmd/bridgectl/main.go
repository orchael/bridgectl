package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/localserver"
)

var version = "dev"

const providerShortcutGroupID = "providers"

func main() {
	var (
		bareProvider   string
		bareProject    string
		bareTimeout    time.Duration
		bareNoTTY      bool
		bareRemote     string
		bareCert       string
		bareKey        string
		bareJWTKey     string
		bareServerName string
	)

	root := &cobra.Command{
		Use:   "bridgectl",
		Short: "AI Agent Bridge — run AI agents locally",
		Long: `bridgectl starts a local bridge server and spawns AI agent sessions
in your terminal. The server auto-starts on first use and is shared
across terminal windows.

Running bridgectl with no subcommand is shorthand for
'bridgectl session start' with the default provider.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
		Args:          cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return startSession(args, bareProvider, bareProject, bareTimeout, bareNoTTY, bareRemote, bareCert, bareKey, bareJWTKey, bareServerName)
		},
	}
	root.Flags().StringVarP(&bareProvider, "provider", "p", "claude", "AI provider (claude, codex, opencode, gemini, echo)")
	root.Flags().StringVar(&bareProject, "project", "local", "project ID")
	root.Flags().DurationVarP(&bareTimeout, "timeout", "t", 30*time.Minute, "session timeout")
	root.Flags().BoolVar(&bareNoTTY, "no-tty", false, "run without a terminal (for scripting and tests)")
	addRemoteFlags(root, &bareRemote, &bareCert, &bareKey, &bareJWTKey, &bareServerName)

	root.AddCommand(
		newRunCmd(),
		newSessionCmd(),
		newServerCmd(),
		newClientCmd(),
		newEnrollmentCmd(),
		newIdentityCmd(),
		newTelemetryCmd(),
		newBridgeLoginCmd(),
		newBridgeLogoutCmd(),
		newBridgeWhoamiCmd(),
		newDoctorCmd(),
	)

	root.AddGroup(&cobra.Group{ID: providerShortcutGroupID, Title: "Provider shortcuts:"})
	for _, providerID := range providerShortcutIDs() {
		if commandNameReserved(root, providerID) {
			// A provider named e.g. "doctor" or "help" would otherwise
			// create a duplicate/conflicting top-level command. Skip the
			// shortcut; the provider is still reachable via
			// `session start --provider <name>`. Flagged by Copilot
			// review on PR #279.
			continue
		}
		shortcut := newProviderShortcutCmd(providerID)
		shortcut.GroupID = providerShortcutGroupID
		root.AddCommand(shortcut)
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if strings.HasPrefix(err.Error(), "unknown command") {
			fmt.Fprintln(os.Stderr)
			if usageErr := root.Usage(); usageErr != nil {
				fmt.Fprintln(os.Stderr, usageErr)
			}
		}
		os.Exit(1)
	}
}

// commandNameReserved reports whether name collides with a command or
// alias already registered on root, or with one of Cobra's own
// auto-registered commands ("help", "completion") that are not yet present
// in root.Commands() at the point providerShortcutIDs is consulted (Cobra
// adds them lazily during Execute).
func commandNameReserved(root *cobra.Command, name string) bool {
	if name == "help" || name == "completion" {
		return true
	}
	for _, c := range root.Commands() {
		if c.Name() == name {
			return true
		}
		for _, alias := range c.Aliases {
			if alias == name {
				return true
			}
		}
	}
	return false
}

// providerShortcutIDs returns the provider names that get a top-level
// `bridgectl <name>` shortcut (issue #238): every built-in provider
// (internal/localserver.KnownProviderIDs), plus any custom provider
// defined in the locally discoverable bridge.yaml config file.
func providerShortcutIDs() []string {
	ids := localserver.KnownProviderIDs()
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	if configPath := defaultServerConfigPath(localserver.StateDir()); configPath != "" {
		if fileCfg, err := config.Load(configPath); err == nil {
			for name := range fileCfg.Providers {
				if !seen[name] {
					seen[name] = true
					ids = append(ids, name)
				}
			}
		}
	}
	return ids
}
