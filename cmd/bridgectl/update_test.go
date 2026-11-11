package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	if got := string(log); got != "update\ninstall -y bridgectl=1.5.0\n" {
		t.Fatalf("apt calls = %q", got)
	}
}
