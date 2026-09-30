package main

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
