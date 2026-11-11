package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

func TestUpdateAgentsAllUsesRequestedAndLatestVersions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command fixture")
	}
	wantPaths := map[string]string{
		"/@anthropic-ai%2Fclaude-code/2.0.0": "2.0.0",
		"/@openai%2Fcodex/latest":            "1.4.0",
		"/opencode-ai/latest":                "3.1.0",
	}
	seen := map[string]bool{}
	var seenMu sync.Mutex
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version, ok := wantPaths[r.URL.EscapedPath()]
		if !ok {
			t.Errorf("unexpected registry request %s", r.URL.EscapedPath())
			http.NotFound(w, r)
			return
		}
		seenMu.Lock()
		seen[r.URL.EscapedPath()] = true
		seenMu.Unlock()
		_, _ = fmt.Fprintf(w, `{"version":%q}`, version)
	}))
	defer registry.Close()
	previousRegistry := npmRegistryURL
	npmRegistryURL = registry.URL
	t.Cleanup(func() { npmRegistryURL = previousRegistry })

	root := t.TempDir()
	live, bin := filepath.Join(root, "runtime"), filepath.Join(root, "bin")
	for _, dir := range []string{live, bin} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(live, "package.json"), []byte(`{"private":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(root, "pnpm-args")
	script := "#!/bin/sh\nprintf '%s\n' \"$@\" > \"$AGENT_ARGS_PATH\"\n/bin/mkdir -p node_modules/.bin\nfor agent in claude codex opencode; do printf ready > node_modules/.bin/$agent; done\nprintf lock > pnpm-lock.yaml\n"
	if err := os.WriteFile(filepath.Join(bin, "corepack"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("AGENT_ARGS_PATH", argsPath)
	cmd := newUpdateAgentsCmd()
	cmd.SetArgs([]string{"all", "--claude-version", "v2.0.0", "--runtime-dir", live})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != len(wantPaths) {
		t.Fatalf("registry requests = %v", seen)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@anthropic-ai/claude-code@2.0.0", "@openai/codex@1.4.0", "opencode-ai@3.1.0"} {
		if !strings.Contains(string(args), want+"\n") {
			t.Errorf("missing %s in pnpm args %q", want, args)
		}
	}
	for _, name := range []string{"claude", "codex", "opencode"} {
		if _, err := os.Stat(filepath.Join(live, "node_modules", ".bin", name)); err != nil {
			t.Errorf("missing installed %s: %v", name, err)
		}
	}
}

func TestUpdateAgentsRejectsInvalidOptionsBeforeInstall(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"},
		{"codex", "--version", "latest"},
		{"all", "--version", "1.0.0"},
		{"all", "--claude-version", "invalid"},
		{"codex", "--claude-version", "1.0.0"},
		{"codex", "--version", "1.0.0", "--codex-version", "1.0.0"},
		{"codex", "--runtime-dir", "relative"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			cmd := newUpdateAgentsCmd()
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected invalid options to fail")
			}
		})
	}
}

func TestUpdateAgentsRecoversInterruptedRuntimeBeforeRegistryFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable fixture")
	}
	root := t.TempDir()
	live, backup, bin := filepath.Join(root, "runtime"), filepath.Join(root, ".bridgectl-agents-backup-test"), filepath.Join(root, "bin")
	for _, dir := range []string{live, backup, bin} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(backup, "package.json"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(backup, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "node_modules", "original"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := agentUpdateMarker(live)
	if err := atomicJSON(marker, agentUpdateTransaction{Backup: backup, State: "in_progress", Had: map[string]bool{"package.json": true, "node_modules": true}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "corepack"), []byte(""), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer registry.Close()
	previousRegistry := npmRegistryURL
	npmRegistryURL = registry.URL
	t.Cleanup(func() { npmRegistryURL = previousRegistry })
	cmd := newUpdateAgentsCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"codex", "--runtime-dir", live})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected registry failure, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(live, "package.json")); err != nil || string(got) != "original" {
		t.Fatalf("manifest was not restored: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(live, "node_modules", "original")); err != nil || string(got) != "ready" {
		t.Fatalf("modules were not restored: %q, %v", got, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction marker remains: %v", err)
	}
}

func TestUpdateAgentsInitializesMissingRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command fixture")
	}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/@openai%2Fcodex/latest" {
			t.Errorf("unexpected registry path %s", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"version":"1.4.0"}`))
	}))
	defer registry.Close()
	previousRegistry := npmRegistryURL
	npmRegistryURL = registry.URL
	t.Cleanup(func() { npmRegistryURL = previousRegistry })

	root := t.TempDir()
	live, bin := filepath.Join(root, "missing", "bridgectl", "providers"), filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ -f package.json ] || exit 9\n/bin/mkdir -p node_modules/.bin\nprintf ready > node_modules/.bin/codex\nprintf lock > pnpm-lock.yaml\n"
	if err := os.WriteFile(filepath.Join(bin, "corepack"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cmd := newUpdateAgentsCmd()
	cmd.SetArgs([]string{"codex", "--runtime-dir", live})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(live, "package.json"))
	if err != nil || !strings.Contains(string(manifest), `"name": "bridgectl-provider-runtime"`) {
		t.Fatalf("initialized manifest = %q, %v", manifest, err)
	}
	if _, err := os.Stat(filepath.Join(live, "node_modules", ".bin", "codex")); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateAgentsDoesNotOverwriteIncompleteRuntime(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "providers")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	modules := filepath.Join(live, "node_modules")
	if err := os.Mkdir(modules, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := installAgentPackages(cmd, live, []string{"codex"}, []string{"@openai/codex@1.4.0"}); err == nil || !strings.Contains(err.Error(), "has files but no package.json") {
		t.Fatalf("expected incomplete-runtime error, got %v", err)
	}
	if _, err := os.Stat(modules); err != nil {
		t.Fatalf("existing modules were removed: %v", err)
	}
}

func TestRecoverAgentRuntimeRejectsInvalidMarkerWithoutDeletingData(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		marker func(live, backup string) []byte
	}{
		{"invalid JSON", func(_, _ string) []byte { return []byte("{") }},
		{"other directory", func(_, _ string) []byte { return []byte(`{"backup":"/tmp/other","state":"in_progress","had":{}}`) }},
		{"invalid state", func(_, backup string) []byte {
			return []byte(fmt.Sprintf(`{"backup":%q,"state":"unknown","had":{}}`, backup))
		}},
		{"missing backup", func(_, backup string) []byte {
			return []byte(fmt.Sprintf(`{"backup":%q,"state":"in_progress","had":{}}`, backup))
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			live := filepath.Join(root, "runtime")
			backup := filepath.Join(root, ".bridgectl-agents-backup-missing")
			if err := os.Mkdir(live, 0o700); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(live, "package.json")
			if err := os.WriteFile(manifest, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			marker := agentUpdateMarker(live)
			if err := os.WriteFile(marker, scenario.marker(live, backup), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := recoverAgentRuntime(live); err == nil {
				t.Fatal("expected recovery to reject invalid transaction")
			}
			if got, err := os.ReadFile(manifest); err != nil || string(got) != "original" {
				t.Fatalf("live manifest changed: %q, %v", got, err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("transaction marker lost: %v", err)
			}
		})
	}
}

func TestRecoverAgentRuntimeRejectsMissingOriginal(t *testing.T) {
	root := t.TempDir()
	live, backup := filepath.Join(root, "runtime"), filepath.Join(root, ".bridgectl-agents-backup-test")
	for _, dir := range []string{live, backup} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := atomicJSON(agentUpdateMarker(live), agentUpdateTransaction{Backup: backup, State: "in_progress", Had: map[string]bool{"package.json": true}}); err != nil {
		t.Fatal(err)
	}
	if err := recoverAgentRuntime(live); err == nil || !strings.Contains(err.Error(), "both live and backup are missing") {
		t.Fatalf("expected missing original error, got %v", err)
	}
}

func TestCopyRuntimeFileRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture")
	}
	root := t.TempDir()
	source, link, dest := filepath.Join(root, "source"), filepath.Join(root, "link"), filepath.Join(root, "dest")
	if err := os.WriteFile(source, []byte("manifest"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := copyRuntimeFile(source, dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "manifest" {
		t.Fatalf("copy = %q, %v", got, err)
	}
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := copyRuntimeFile(link, filepath.Join(root, "rejected")); err == nil {
		t.Fatal("expected symlink to be rejected")
	}
	if err := copyRuntimeFile(filepath.Join(root, "missing"), filepath.Join(root, "missing-copy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing source error, got %v", err)
	}
}

func TestInstallVersionedBrewCaskUsesVerifiedRelease(t *testing.T) {
	if runtime.GOOS == "windows" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("shell fixture requires a supported Homebrew architecture")
	}
	const version = "1.2.3"
	archive := fmt.Sprintf("bridgectl_%s_darwin_%s.tar.gz", version, runtime.GOARCH)
	checksum := strings.Repeat("a", 64)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.2.3/checksums.txt" {
			t.Errorf("unexpected release path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "%s  %s\n", checksum, archive)
	}))
	defer registry.Close()
	previousBase := brewReleaseBaseURL
	brewReleaseBaseURL = registry.URL
	t.Cleanup(func() { brewReleaseBaseURL = previousBase })

	root := t.TempDir()
	copyPath := filepath.Join(root, "installed-cask.rb")
	script := "#!/bin/sh\n[ \"$1\" = install ] && [ \"$2\" = --cask ] && [ \"$3\" = --force ] || exit 9\n/bin/cp \"$4\" \"$BREW_CASK_COPY\"\n"
	if err := os.WriteFile(filepath.Join(root, "brew"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	t.Setenv("BREW_CASK_COPY", copyPath)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := installVersionedBrewCask(cmd, version); err != nil {
		t.Fatal(err)
	}
	cask, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`version "1.2.3"`, `sha256 "` + checksum + `"`, registry.URL + "/v1.2.3/" + archive} {
		if !strings.Contains(string(cask), want) {
			t.Errorf("cask is missing %q: %s", want, cask)
		}
	}
}

func TestInstallVersionedBrewCaskRejectsMissingChecksum(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("unsupported Homebrew architecture")
	}
	for _, scenario := range []struct {
		name, body string
		status     int
	}{
		{"missing archive", strings.Repeat("a", 64) + "  other.tar.gz\n", http.StatusOK},
		{"missing release", "", http.StatusNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(scenario.status)
				_, _ = w.Write([]byte(scenario.body))
			}))
			defer registry.Close()
			previousBase := brewReleaseBaseURL
			brewReleaseBaseURL = registry.URL
			t.Cleanup(func() { brewReleaseBaseURL = previousBase })
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			if err := installVersionedBrewCask(cmd, "1.2.3"); err == nil {
				t.Fatal("expected unverified release to be rejected")
			}
		})
	}
}
