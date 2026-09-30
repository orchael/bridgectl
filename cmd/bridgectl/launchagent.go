package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// launchAgentLabel is the launchd job label. It doubles as the plist filename
// and as the argument to `launchctl bootout`, and the Homebrew cask references
// it in its uninstall stanza, so changing it orphans agents on existing installs.
const launchAgentLabel = "com.orchael.bridgectl"

// launchAgentSpec is the fully resolved input to the plist template. Every path
// is absolute: launchd does not expand ~, $HOME, or any other variable inside
// ProgramArguments or the log paths.
type launchAgentSpec struct {
	Label      string
	Program    string
	Args       []string
	Path       string
	Home       string
	StdoutPath string
	StderrPath string
}

// launchAgentPlistTemplate renders a launchd user agent.
//
// KeepAlive is a dict rather than <true/> so that a clean exit — `bridgectl
// server stop`, or `launchctl stop` — is respected. A bare <true/> respawns the
// daemon immediately and makes the server impossible to stop without unloading
// the agent.
//
// EnvironmentVariables carries PATH because launchd gives agents a minimal
// default (/usr/bin:/bin:/usr/sbin:/sbin). bridgectl resolves node through
// exec.LookPath (internal/config/node.go), so without an inherited PATH the
// server starts but every provider session fails with "node not found on PATH".
const launchAgentPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{ xml .Label }}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{ xml .Program }}</string>
{{- range .Args }}
    <string>{{ xml . }}</string>
{{- end }}
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>{{ xml .Path }}</string>
    <key>HOME</key>
    <string>{{ xml .Home }}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>WorkingDirectory</key>
  <string>{{ xml .Home }}</string>
  <key>StandardOutPath</key>
  <string>{{ xml .StdoutPath }}</string>
  <key>StandardErrorPath</key>
  <string>{{ xml .StderrPath }}</string>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`

// launchAgentPlistPath returns the per-user LaunchAgents path for the agent.
func launchAgentPlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

// launchAgentLogDir returns the directory holding the agent's stdout and stderr.
// The previous plist wrote both to /tmp, which is world-readable and shared
// between users; session output can carry repository contents and provider
// prompts, so the logs belong under the user's own Library.
func launchAgentLogDir(home string) string {
	return filepath.Join(home, "Library", "Logs", "bridgectl")
}

// prependToPath puts dir at the front of a PATH list and drops empty and
// duplicate entries, preserving the order of what remains. An empty dir only
// deduplicates. Interactive shells accumulate a lot of repetition through
// profile chaining, and that noise is baked into the plist verbatim otherwise.
func prependToPath(path, dir string) string {
	entries := filepath.SplitList(path)
	if dir != "" {
		entries = append([]string{dir}, entries...)
	}

	seen := make(map[string]struct{}, len(entries))
	kept := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		kept = append(kept, entry)
	}

	return strings.Join(kept, string(os.PathListSeparator))
}

// renderLaunchAgentPlist renders spec into a launchd property list.
func renderLaunchAgentPlist(spec launchAgentSpec) ([]byte, error) {
	tmpl, err := template.New("launchagent").Funcs(template.FuncMap{
		"xml": func(s string) (string, error) {
			var buf bytes.Buffer
			if err := xml.EscapeText(&buf, []byte(s)); err != nil {
				return "", err
			}
			return buf.String(), nil
		},
	}).Parse(launchAgentPlistTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse launch agent template: %w", err)
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, spec); err != nil {
		return nil, fmt.Errorf("render launch agent plist: %w", err)
	}

	return out.Bytes(), nil
}

// newLaunchAgentSpec resolves the agent definition for the running binary.
// exePath is resolved by the caller so tests can supply one; symlinks are
// followed because Homebrew links the cask binary into its bin directory and
// launchd should point at the real file in the Caskroom.
func newLaunchAgentSpec(exePath, home, currentPath string) (launchAgentSpec, error) {
	resolved, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		// A broken symlink is worth reporting, but a missing EvalSymlinks
		// target on an otherwise valid path should not block installation.
		resolved = exePath
	}
	if !filepath.IsAbs(resolved) {
		abs, absErr := filepath.Abs(resolved)
		if absErr != nil {
			return launchAgentSpec{}, fmt.Errorf("resolve absolute path for %q: %w", resolved, absErr)
		}
		resolved = abs
	}

	logDir := launchAgentLogDir(home)

	return launchAgentSpec{
		Label:      launchAgentLabel,
		Program:    resolved,
		Args:       []string{"server", "start"},
		Path:       prependToPath(currentPath, filepath.Dir(resolved)),
		Home:       home,
		StdoutPath: filepath.Join(logDir, "bridgectl.log"),
		StderrPath: filepath.Join(logDir, "bridgectl.err"),
	}, nil
}

// Injection points for tests. The command bodies are gated on macOS and shell
// out to launchctl, neither of which is reachable on a Linux CI runner, so they
// are indirected the same way internal/config/node.go indirects exec.LookPath.
var (
	currentGOOS            = runtime.GOOS
	bootstrapLaunchAgentFn = bootstrapLaunchAgent
	bootoutLaunchAgentFn   = bootoutLaunchAgent
)

// errLaunchAgentUnsupported explains why the command is a no-op off macOS.
func errLaunchAgentUnsupported() error {
	return fmt.Errorf(
		"launch agents are macOS-only (this is %s); on Linux install the systemd user unit from packaging/bridge.user.service",
		currentGOOS,
	)
}

func newServerInstallAgentCmd() *cobra.Command {
	var start bool

	cmd := &cobra.Command{
		Use:   "install-agent",
		Short: "Install a macOS launchd agent that runs the bridge server at login",
		Long: `Write a launchd user agent that starts the bridge server at login.

The agent points at the absolute path of the running bridgectl binary, so it is
correct for Homebrew on Apple Silicon (/opt/homebrew), Homebrew on Intel
(/usr/local), and any manual install. It inherits the PATH of the shell you run
this from, which is how the server finds node and the provider CLIs.

Logs are written to ~/Library/Logs/bridgectl/.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if currentGOOS != "darwin" {
				return errLaunchAgentUnsupported()
			}

			exePath, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate the running bridgectl binary: %w", err)
			}

			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("locate home directory: %w", err)
			}

			spec, err := newLaunchAgentSpec(exePath, home, os.Getenv("PATH"))
			if err != nil {
				return err
			}

			plist, err := renderLaunchAgentPlist(spec)
			if err != nil {
				return err
			}

			if err := os.MkdirAll(launchAgentLogDir(home), 0o700); err != nil {
				return fmt.Errorf("create log directory: %w", err)
			}

			plistPath := launchAgentPlistPath(home)
			if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
				return fmt.Errorf("create LaunchAgents directory: %w", err)
			}
			if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", plistPath, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", plistPath)
			fmt.Fprintf(cmd.OutOrStdout(), "  program: %s\n", spec.Program)
			fmt.Fprintf(cmd.OutOrStdout(), "  logs:    %s\n", launchAgentLogDir(home))

			if !start {
				fmt.Fprintf(cmd.OutOrStdout(), "\nStart it with:\n  launchctl bootstrap gui/$(id -u) %s\n", plistPath)
				return nil
			}

			if err := bootstrapLaunchAgentFn(plistPath); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "\nAgent loaded. Check it with:\n  launchctl print gui/$(id -u)/%s\n", launchAgentLabel)
			return nil
		},
	}

	cmd.Flags().BoolVar(&start, "start", false, "Load the agent immediately after writing it")

	return cmd
}

func newServerUninstallAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall-agent",
		Short: "Remove the macOS launchd agent for the bridge server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if currentGOOS != "darwin" {
				return errLaunchAgentUnsupported()
			}

			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("locate home directory: %w", err)
			}

			plistPath := launchAgentPlistPath(home)
			if _, err := os.Stat(plistPath); os.IsNotExist(err) {
				fmt.Fprintf(cmd.OutOrStdout(), "No launch agent installed at %s\n", plistPath)
				return nil
			}

			// Booting out a job that is not loaded is not an error worth
			// failing on; the plist removal below is what matters.
			_ = bootoutLaunchAgentFn()

			if err := os.Remove(plistPath); err != nil {
				return fmt.Errorf("remove %s: %w", plistPath, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", plistPath)
			fmt.Fprintf(cmd.OutOrStdout(), "Logs under %s were left in place.\n", launchAgentLogDir(home))
			return nil
		},
	}

	return cmd
}

// launchctlDomain returns the gui domain target for the current user.
func launchctlDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func bootstrapLaunchAgent(plistPath string) error {
	out, err := exec.Command("launchctl", "bootstrap", launchctlDomain(), plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func bootoutLaunchAgent() error {
	out, err := exec.Command("launchctl", "bootout", launchctlDomain()+"/"+launchAgentLabel).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
