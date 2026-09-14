package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "bridgectl",
		Short: "AI Agent Bridge — run AI agents locally",
		Long: `bridgectl starts a local bridge server and spawns AI agent sessions
in your terminal. The server auto-starts on first use and is shared
across terminal windows.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}

	root.AddCommand(
		newRunCmd(),
		newSessionCmd(),
		newServerCmd(),
		newClientCmd(),
		newEnrollmentCmd(),
		newIdentityCmd(),
		newTelemetryCmd(),
	)

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
