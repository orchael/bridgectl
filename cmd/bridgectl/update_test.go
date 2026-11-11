package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestLoadUpdateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bridgectl.yaml")
	cfg, err := loadUpdateConfig(path)
	if err != nil || cfg.Bridge != productionBridgeURL || cfg.Login || cfg.Doctor {
		t.Fatalf("default config = %+v, %v", cfg, err)
	}
	if err := os.WriteFile(path, []byte("version: 1\nsetup:\n  - echo ready\nupdate:\n  login: true\n  doctor: true\n  bridge: https://bridge.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadUpdateConfig(path)
	if err != nil || !cfg.Login || !cfg.Doctor || cfg.Bridge != "https://bridge.example" {
		t.Fatalf("loaded config = %+v, %v", cfg, err)
	}
}

func TestLoadUpdateConfigRejectsUnknownAndInsecureFields(t *testing.T) {
	for _, content := range []string{"version: 1\nupdate:\n  logni: true\n", "version: 1\nupdate:\n  bridge: http://bridge.example\n", "version: 2\nupdate:\n  doctor: true\n"} {
		path := filepath.Join(t.TempDir(), "update.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadUpdateConfig(path); err == nil {
			t.Fatalf("expected error for %q", content)
		}
	}
}

func TestNpmVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/@openai%2Fcodex/latest" {
			t.Errorf("registry path = %q", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"version":"0.162.1"}`))
	}))
	defer server.Close()
	got, err := npmVersion(context.Background(), server.Client(), server.URL, "@openai/codex", "")
	if err != nil || got != "0.162.1" {
		t.Fatalf("npm version = %q, %v", got, err)
	}
}

func TestNpmVersionRejectsMissingVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, err := npmVersion(context.Background(), server.Client(), server.URL, "opencode-ai", "99.0.0")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected missing version error, got %v", err)
	}
}

func TestPackageOwnership(t *testing.T) {
	if !aptOwnsBinary("bridgectl: /usr/bin/bridgectl") {
		t.Fatal("expected apt ownership")
	}
	if !aptOwnsBinary("bridgectl:amd64: /usr/bin/bridgectl") {
		t.Fatal("expected multiarch apt ownership")
	}
	if aptOwnsBinary("other: /usr/bin/bridgectl") {
		t.Fatal("unrelated apt package must not count")
	}
	if !brewOwnsBinary("/opt/homebrew", "/opt/homebrew/Caskroom/bridgectl/1.5.0/bridgectl") {
		t.Fatal("expected Homebrew cask ownership")
	}
	if brewOwnsBinary("/opt/homebrew", "/usr/local/bin/bridgectl") {
		t.Fatal("manual binary must not count as Homebrew")
	}
}

func TestUpdateSelfAptInstallsRequestedVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command fixture")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "apt.log")
	scripts := map[string]string{
		"dpkg-query": "#!/bin/sh\necho 'bridgectl: /usr/bin/bridgectl'\n",
		"apt-cache":  "#!/bin/sh\necho 'bridgectl | 1.5.0 | repository'\n",
		"apt-get":    "#!/bin/sh\necho \"$*\" >> '" + logPath + "'\n",
		"sudo":       "#!/bin/sh\nexec \"$@\"\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := updateSelf(cmd, "v1.5.0"); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(log); got != "update\ninstall -y --allow-downgrades bridgectl=1.5.0\n" {
		t.Fatalf("apt calls = %q", got)
	}
}

func TestAgentRuntimeLockSerializesConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
	firstDone, secondEntered, secondDone := make(chan error, 1), make(chan struct{}), make(chan error, 1)
	go func() {
		firstDone <- withAgentRuntimeLock(ctx, dir, func(string) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		secondDone <- withAgentRuntimeLock(ctx, dir, func(string) error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second update entered before first released the runtime")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestUpdateSelfAptInstallsLatestCandidate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command fixture")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "apt.log")
	scripts := map[string]string{
		"dpkg-query": "#!/bin/sh\necho 'bridgectl: /usr/bin/bridgectl'\n",
		"apt-cache":  "#!/bin/sh\necho 'bridgectl:'\necho '  Candidate: 1.6.0'\n",
		"apt-get":    "#!/bin/sh\necho \"$*\" >> '" + logPath + "'\n",
		"sudo":       "#!/bin/sh\nexec \"$@\"\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := updateSelf(cmd, ""); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(log); got != "update\ninstall -y bridgectl=1.6.0\n" {
		t.Fatalf("apt calls = %q", got)
	}
}

func TestReplaceLaunchAgentProgramPreservesOtherSettings(t *testing.T) {
	original := []byte("<plist><dict><key>ProgramArguments</key><array><string>/old/bridgectl</string><string>server</string></array><key>EnvironmentVariables</key><dict><key>PATH</key><string>/custom/bin</string></dict></dict></plist>")
	updated, err := replaceLaunchAgentProgram(original, "/new/with&space/bridgectl")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated, []byte("/new/with&amp;space/bridgectl")) || !bytes.Contains(updated, []byte("<string>/custom/bin</string>")) || bytes.Contains(updated, []byte("/old/bridgectl")) {
		t.Fatalf("updated plist = %s", updated)
	}
}

func TestRefreshLaunchAgentAfterBrewUsesNewCaskPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink and shell fixture")
	}
	root := t.TempDir()
	home, prefix, bin := filepath.Join(root, "home"), filepath.Join(root, "homebrew"), filepath.Join(root, "bin")
	newBinary := filepath.Join(prefix, "Caskroom", "bridgectl", "2.0.0", "bridgectl")
	for _, dir := range []string{home, bin, filepath.Dir(newBinary)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(newBinary, []byte("new binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newBinary, filepath.Join(bin, "bridgectl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "brew"), []byte("#!/bin/sh\necho '"+prefix+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	plistPath := launchAgentPlistPath(home)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("<plist><dict><key>ProgramArguments</key><array><string>/old/Caskroom/bridgectl</string><string>server</string></array><key>EnvironmentVariables</key><dict><key>PATH</key><string>/custom/bin</string></dict></dict></plist>")
	if err := os.WriteFile(plistPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := refreshLaunchAgentAfterBrew(cmd); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated, []byte(newBinary)) || bytes.Contains(updated, []byte("/old/Caskroom")) || !bytes.Contains(updated, []byte("/custom/bin")) {
		t.Fatalf("updated plist = %s", updated)
	}
}

func TestSwapAgentRuntimeRollsBackOnMissingStagedModules(t *testing.T) {
	root := t.TempDir()
	live, stage := filepath.Join(root, "live"), filepath.Join(root, "stage")
	for _, dir := range []string{live, stage} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(live, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "node_modules", "old"), []byte("working"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ dir, name, contents string }{
		{live, "package.json", "old manifest"}, {live, "pnpm-lock.yaml", "old lock"},
		{stage, "package.json", "new manifest"}, {stage, "pnpm-lock.yaml", "new lock"},
	} {
		if err := os.WriteFile(filepath.Join(item.dir, item.name), []byte(item.contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := swapAgentRuntime(live, stage); err == nil {
		t.Fatal("expected missing staged modules to fail")
	}
	for name, want := range map[string]string{"package.json": "old manifest", "pnpm-lock.yaml": "old lock"} {
		got, err := os.ReadFile(filepath.Join(live, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(live, "node_modules", "old")); err != nil || string(got) != "working" {
		t.Fatalf("old modules = %q, %v", got, err)
	}
}

func TestRecoverAgentRuntimeAtSwapBoundaries(t *testing.T) {
	for _, moved := range []int{0, 1, 2, 3, 4, 5, 6} {
		t.Run(fmt.Sprint(moved), func(t *testing.T) {
			root := t.TempDir()
			live, backup := filepath.Join(root, "live"), filepath.Join(root, ".bridgectl-agents-backup-test")
			for _, dir := range []string{live, backup} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range agentRuntimeItems {
				if name == "node_modules" {
					if err := os.Mkdir(filepath.Join(live, name), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(live, name, "old"), []byte("old"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(live, name), []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			txn := agentUpdateTransaction{Backup: backup, State: "in_progress", Had: map[string]bool{"package.json": true, "pnpm-lock.yaml": true, "node_modules": true}}
			if err := atomicJSON(agentUpdateMarker(live), txn); err != nil {
				t.Fatal(err)
			}
			for step := 0; step < moved; step++ {
				name := agentRuntimeItems[step/2]
				path := filepath.Join(live, name)
				if step%2 == 0 {
					if err := os.Rename(path, filepath.Join(backup, name)); err != nil {
						t.Fatal(err)
					}
				} else if name == "node_modules" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := recoverAgentRuntime(live); err != nil {
				t.Fatal(err)
			}
			for _, name := range agentRuntimeItems[:2] {
				got, err := os.ReadFile(filepath.Join(live, name))
				if err != nil || string(got) != "old" {
					t.Fatalf("%s = %q, %v", name, got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(live, "node_modules", "old")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(agentUpdateMarker(live)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("marker remains: %v", err)
			}
		})
	}
}

func TestRecoverCommittedAgentRuntimeKeepsNewFiles(t *testing.T) {
	root := t.TempDir()
	live, backup := filepath.Join(root, "live"), filepath.Join(root, ".bridgectl-agents-backup-test")
	for _, dir := range []string{live, backup} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(live, "package.json"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(agentUpdateMarker(live), agentUpdateTransaction{Backup: backup, State: "committed", Had: map[string]bool{"package.json": true}}); err != nil {
		t.Fatal(err)
	}
	if err := recoverAgentRuntime(live); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(live, "package.json"))
	if err != nil || string(got) != "new" {
		t.Fatalf("manifest = %q, %v", got, err)
	}
}

func TestInstallAgentPackagesStagesBeforeReplacing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command fixture")
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			root := t.TempDir()
			live, bin := filepath.Join(root, "runtime"), filepath.Join(root, "bin")
			if err := os.MkdirAll(filepath.Join(live, "node_modules", ".bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, contents := range map[string]string{"package.json": "old manifest", "pnpm-lock.yaml": "old lock", "pnpm-workspace.yaml": "packages: []", "node_modules/.bin/codex": "old binary"} {
				if err := os.WriteFile(filepath.Join(live, name), []byte(contents), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			script := "#!/bin/sh\ncase \" $* \" in *\" --workspace-root \"*) ;; *) exit 8;; esac\n/bin/mkdir -p node_modules/.bin\nprintf 'new manifest' > package.json\nprintf 'new lock' > pnpm-lock.yaml\nprintf 'new binary' > node_modules/.bin/codex\n"
			if fail {
				script += "exit 7\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "corepack"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			err := installAgentPackages(cmd, live, []string{"codex"}, []string{"@openai/codex@0.2.0"})
			if fail && err == nil {
				t.Fatal("expected staged install failure")
			}
			if !fail && err != nil {
				t.Fatal(err)
			}
			want := "new manifest"
			if fail {
				want = "old manifest"
			}
			got, readErr := os.ReadFile(filepath.Join(live, "package.json"))
			if readErr != nil || string(got) != want {
				t.Fatalf("manifest = %q, %v", got, readErr)
			}
			want = "new binary"
			if fail {
				want = "old binary"
			}
			got, readErr = os.ReadFile(filepath.Join(live, "node_modules", ".bin", "codex"))
			if readErr != nil || string(got) != want {
				t.Fatalf("binary = %q, %v", got, readErr)
			}
		})
	}
}

func TestUpdateAgentsCommandResolvesAndInstallsExactVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command fixture")
	}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/@openai%2Fcodex/0.2.0" {
			t.Errorf("registry path = %q", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"version":"0.2.0"}`))
	}))
	defer registry.Close()
	previousRegistry := npmRegistryURL
	npmRegistryURL = registry.URL
	t.Cleanup(func() { npmRegistryURL = previousRegistry })
	root := t.TempDir()
	live, bin := filepath.Join(root, "runtime"), filepath.Join(root, "bin")
	for _, dir := range []string{live, bin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(live, "package.json"), []byte("old manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n/bin/mkdir -p node_modules/.bin\nprintf 'new manifest' > package.json\nprintf 'new lock' > pnpm-lock.yaml\nprintf 'new binary' > node_modules/.bin/codex\n"
	if err := os.WriteFile(filepath.Join(bin, "corepack"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cmd := newUpdateAgentsCmd()
	cmd.SetArgs([]string{"codex", "--version", "0.2.0", "--runtime-dir", live})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(live, "node_modules", ".bin", "codex"))
	if err != nil || string(got) != "new binary" {
		t.Fatalf("installed binary = %q, %v", got, err)
	}
}
