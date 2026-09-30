package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// The launch agent shipped before this change hardcoded /usr/local/bin and gave
// launchd no PATH, so it failed outright under Homebrew on Apple Silicon and
// could not resolve node for provider sessions. These tests pin both fixes.

// fakeBinary creates an executable file inside a temp directory and returns its
// path. Tests must not reference real filesystem locations such as
// /opt/homebrew/bin/bridgectl: newLaunchAgentSpec resolves symlinks, so on a
// machine that actually has bridgectl installed the result would differ.
func fakeBinary(t *testing.T, elem ...string) string {
	t.Helper()

	dir := filepath.Join(append([]string{t.TempDir()}, elem...)...)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	bin := filepath.Join(dir, "bridgectl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	resolved, err := filepath.EvalSymlinks(bin)
	require.NoError(t, err)

	return resolved
}

func TestRenderLaunchAgentPlistIsWellFormedXML(t *testing.T) {
	t.Parallel()

	spec, err := newLaunchAgentSpec(fakeBinary(t, "bin"), "/Users/example", "/usr/bin:/bin")
	require.NoError(t, err)

	out, err := renderLaunchAgentPlist(spec)
	require.NoError(t, err)

	decoder := xml.NewDecoder(strings.NewReader(string(out)))
	decoder.Strict = true
	for {
		_, err := decoder.Token()
		if err != nil {
			require.ErrorIs(t, err, io.EOF, "plist must parse as XML: %v", err)
			break
		}
	}
}

func TestNewLaunchAgentSpecUsesTheRunningBinaryPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		layout []string
	}{
		{name: "apple silicon homebrew", layout: []string{"opt", "homebrew", "bin"}},
		{name: "intel homebrew", layout: []string{"usr", "local", "bin"}},
		{name: "manual install", layout: []string{"home", "go", "bin"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			exe := fakeBinary(t, tc.layout...)

			spec, err := newLaunchAgentSpec(exe, "/Users/example", "/usr/bin:/bin")
			require.NoError(t, err)
			require.Equal(t, exe, spec.Program)
			require.Equal(t, []string{"server", "start"}, spec.Args)

			out, err := renderLaunchAgentPlist(spec)
			require.NoError(t, err)
			require.Contains(t, string(out), "<string>"+exe+"</string>")
		})
	}
}

func TestNewLaunchAgentSpecPutsBinaryDirectoryOnPath(t *testing.T) {
	t.Parallel()

	exe := fakeBinary(t, "opt", "homebrew", "bin")

	spec, err := newLaunchAgentSpec(exe, "/Users/example", "/usr/bin:/bin")
	require.NoError(t, err)

	// Without this, launchd's default PATH omits the Homebrew prefix and
	// internal/config/node.go fails with "node not found on PATH".
	require.Equal(t, filepath.Dir(exe)+":/usr/bin:/bin", spec.Path)
}

func TestNewLaunchAgentSpecResolvesSymlinks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	realDir := filepath.Join(dir, "Caskroom", "bridgectl", "1.4.1")
	require.NoError(t, os.MkdirAll(realDir, 0o755))
	realBin := filepath.Join(realDir, "bridgectl")
	require.NoError(t, os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o755))

	linkDir := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(linkDir, 0o755))
	link := filepath.Join(linkDir, "bridgectl")
	require.NoError(t, os.Symlink(realBin, link))

	spec, err := newLaunchAgentSpec(link, "/Users/example", "/usr/bin")
	require.NoError(t, err)

	// Homebrew symlinks the cask binary into its bin directory; launchd should
	// reference the real file so an upgrade that replaces the link is visible.
	resolvedReal, err := filepath.EvalSymlinks(realBin)
	require.NoError(t, err)
	require.Equal(t, resolvedReal, spec.Program)
}

func TestNewLaunchAgentSpecUsesPerUserLogPaths(t *testing.T) {
	t.Parallel()

	spec, err := newLaunchAgentSpec(fakeBinary(t, "bin"), "/Users/example", "/usr/bin")
	require.NoError(t, err)

	// /tmp is world-readable and shared between users; session output can carry
	// repository contents, so logs must not land there.
	require.Equal(t, "/Users/example/Library/Logs/bridgectl/bridgectl.log", spec.StdoutPath)
	require.Equal(t, "/Users/example/Library/Logs/bridgectl/bridgectl.err", spec.StderrPath)
	require.NotContains(t, spec.StdoutPath, "/tmp/")
	require.NotContains(t, spec.StderrPath, "/tmp/")
}

func TestRenderLaunchAgentPlistKeepAliveRespectsCleanExit(t *testing.T) {
	t.Parallel()

	spec, err := newLaunchAgentSpec(fakeBinary(t, "bin"), "/Users/example", "/usr/bin")
	require.NoError(t, err)

	out, err := renderLaunchAgentPlist(spec)
	require.NoError(t, err)

	// A bare <key>KeepAlive</key><true/> respawns the server after a clean
	// `bridgectl server stop`, making it impossible to stop the daemon.
	require.Contains(t, string(out), "<key>KeepAlive</key>")
	require.Contains(t, string(out), "<key>SuccessfulExit</key>")
	require.NotRegexp(t, `(?s)<key>KeepAlive</key>\s*<true/>`, string(out))
}

func TestRenderLaunchAgentPlistEscapesXMLMetacharacters(t *testing.T) {
	t.Parallel()

	spec, err := newLaunchAgentSpec(fakeBinary(t, "bin"), "/Users/a&b<c>", "/usr/bin")
	require.NoError(t, err)

	out, err := renderLaunchAgentPlist(spec)
	require.NoError(t, err)

	require.NotContains(t, string(out), "<string>/Users/a&b<c></string>")
	require.Contains(t, string(out), "&amp;")
}

func TestPrependToPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		dir  string
		want string
	}{
		{name: "prepends", path: "/usr/bin:/bin", dir: "/opt/homebrew/bin", want: "/opt/homebrew/bin:/usr/bin:/bin"},
		{name: "deduplicates", path: "/usr/bin:/opt/homebrew/bin:/bin", dir: "/opt/homebrew/bin", want: "/opt/homebrew/bin:/usr/bin:/bin"},
		{name: "drops empty entries", path: "/usr/bin::/bin", dir: "/opt/homebrew/bin", want: "/opt/homebrew/bin:/usr/bin:/bin"},
		{name: "empty dir is a no-op", path: "/usr/bin:/bin", dir: "", want: "/usr/bin:/bin"},
		{name: "empty path yields dir only", path: "", dir: "", want: ""},
		{name: "dir only", path: "", dir: "/opt/homebrew/bin", want: "/opt/homebrew/bin"},
		{name: "deduplicates unrelated repeats", path: "/usr/bin:/bin:/usr/bin", dir: "", want: "/usr/bin:/bin"},
		{name: "preserves order of survivors", path: "/a:/b:/a:/c", dir: "/b", want: "/b:/a:/c"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, prependToPath(tc.path, tc.dir))
		})
	}
}

func TestLaunchAgentPaths(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		"/Users/example/Library/LaunchAgents/com.orchael.bridgectl.plist",
		launchAgentPlistPath("/Users/example"),
	)
	require.Equal(t, "/Users/example/Library/Logs/bridgectl", launchAgentLogDir("/Users/example"))
}

// withDarwin makes the command bodies reachable on a Linux CI runner and
// replaces the launchctl calls, which must never run during tests.
func withDarwin(t *testing.T) (bootstrapped *[]string, bootedOut *int) {
	t.Helper()

	origGOOS, origBootstrap, origBootout := currentGOOS, bootstrapLaunchAgentFn, bootoutLaunchAgentFn
	t.Cleanup(func() {
		currentGOOS = origGOOS
		bootstrapLaunchAgentFn = origBootstrap
		bootoutLaunchAgentFn = origBootout
	})

	calls := []string{}
	boots := 0
	currentGOOS = "darwin"
	bootstrapLaunchAgentFn = func(plistPath string) error {
		calls = append(calls, plistPath)
		return nil
	}
	bootoutLaunchAgentFn = func() error {
		boots++
		return nil
	}

	return &calls, &boots
}

func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()

	return out.String(), err
}

func TestInstallAgentRefusesOffMacOS(t *testing.T) {
	origGOOS := currentGOOS
	t.Cleanup(func() { currentGOOS = origGOOS })
	currentGOOS = "linux"

	_, err := runCmd(t, newServerInstallAgentCmd())
	require.Error(t, err)
	require.Contains(t, err.Error(), "macOS-only")
	require.Contains(t, err.Error(), "packaging/bridge.user.service")

	_, err = runCmd(t, newServerUninstallAgentCmd())
	require.Error(t, err)
	require.Contains(t, err.Error(), "macOS-only")
}

func TestInstallAgentWritesPlistAndLogDirectory(t *testing.T) {
	bootstrapped, _ := withDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := runCmd(t, newServerInstallAgentCmd())
	require.NoError(t, err)

	plistPath := launchAgentPlistPath(home)
	require.FileExists(t, plistPath)
	require.DirExists(t, launchAgentLogDir(home))
	require.Contains(t, out, plistPath)

	// Without --start the agent is written but never loaded.
	require.Empty(t, *bootstrapped)
	require.Contains(t, out, "launchctl bootstrap")

	info, err := os.Stat(plistPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	body, err := os.ReadFile(plistPath)
	require.NoError(t, err)
	require.Contains(t, string(body), "<key>Label</key>")
	require.Contains(t, string(body), launchAgentLabel)
}

func TestInstallAgentStartLoadsTheAgent(t *testing.T) {
	bootstrapped, _ := withDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := runCmd(t, newServerInstallAgentCmd(), "--start")
	require.NoError(t, err)
	require.Equal(t, []string{launchAgentPlistPath(home)}, *bootstrapped)
	require.Contains(t, out, "Agent loaded")
}

func TestInstallAgentSurfacesBootstrapFailure(t *testing.T) {
	withDarwin(t)
	bootstrapLaunchAgentFn = func(string) error { return errors.New("Load failed: 5: Input/output error") }
	t.Setenv("HOME", t.TempDir())

	_, err := runCmd(t, newServerInstallAgentCmd(), "--start")
	require.Error(t, err)
	require.Contains(t, err.Error(), "Load failed")
}

func TestUninstallAgentWhenNothingInstalled(t *testing.T) {
	_, bootedOut := withDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := runCmd(t, newServerUninstallAgentCmd())
	require.NoError(t, err)
	require.Contains(t, out, "No launch agent installed")

	// Nothing to boot out, so launchctl must not be invoked at all.
	require.Zero(t, *bootedOut)
}

func TestUninstallAgentRemovesPlistAndKeepsLogs(t *testing.T) {
	_, bootedOut := withDarwin(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := runCmd(t, newServerInstallAgentCmd())
	require.NoError(t, err)

	logMarker := filepath.Join(launchAgentLogDir(home), "bridgectl.log")
	require.NoError(t, os.WriteFile(logMarker, []byte("prior output\n"), 0o600))

	out, err := runCmd(t, newServerUninstallAgentCmd())
	require.NoError(t, err)
	require.NoFileExists(t, launchAgentPlistPath(home))
	require.Equal(t, 1, *bootedOut)

	// Logs are deliberately preserved; brew --zap is what deletes them.
	require.FileExists(t, logMarker)
	require.Contains(t, out, "left in place")
}

func TestNewLaunchAgentSpecFallsBackWhenPathCannotBeResolved(t *testing.T) {
	t.Parallel()

	// A path that does not exist still yields a usable spec: EvalSymlinks fails
	// and the caller's value is kept rather than aborting the install.
	missing := filepath.Join(t.TempDir(), "not-installed", "bridgectl")

	spec, err := newLaunchAgentSpec(missing, "/Users/example", "/usr/bin")
	require.NoError(t, err)
	require.Equal(t, missing, spec.Program)
}

func TestNewLaunchAgentSpecMakesRelativePathsAbsolute(t *testing.T) {
	// Not parallel: Chdir mutates process state.
	dir := t.TempDir()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(resolvedDir, "bridgectl"), []byte("#!/bin/sh\n"), 0o755))
	t.Chdir(resolvedDir)

	spec, err := newLaunchAgentSpec("bridgectl", "/Users/example", "/usr/bin")
	require.NoError(t, err)

	// launchd rejects a relative ProgramArguments entry.
	require.True(t, filepath.IsAbs(spec.Program), "program path must be absolute, got %q", spec.Program)
	require.Equal(t, filepath.Join(resolvedDir, "bridgectl"), spec.Program)
}

func TestLaunchctlDomainTargetsTheCurrentUser(t *testing.T) {
	t.Parallel()

	require.Equal(t, "gui/"+strconv.Itoa(os.Getuid()), launchctlDomain())
}
