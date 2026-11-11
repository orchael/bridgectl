package main

import (
  "os"
  "path/filepath"
  "testing"
)

func TestLaunchConfigAncestorDiscovery(t *testing.T) {
  root := t.TempDir()
  nested := filepath.Join(root, "src", "nested")
  if err := os.MkdirAll(nested, 0700); err != nil { t.Fatal(err) }
  if err := os.WriteFile(filepath.Join(root, ".bridgectl.yaml"), []byte("project:\n  id: orchael/test\nagent:\n  provider: codex\n"), 0600); err != nil { t.Fatal(err) }
  cfg, err := loadLaunchConfig(nested)
  if err != nil { t.Fatal(err) }
  if cfg == nil || cfg.Project.ID != "orchael/test" || cfg.Agent.Provider != "codex" { t.Fatalf("unexpected config: %+v", cfg) }
}

func TestProjectRegistryResolution(t *testing.T) {
  home := t.TempDir()
  t.Setenv("HOME", home)
  configDir := filepath.Join(home, ".config", "bridgectl")
  if err := os.MkdirAll(configDir, 0700); err != nil { t.Fatal(err) }
  if err := os.WriteFile(filepath.Join(configDir, "projects.yaml"), []byte("projects:\n  crew:\n    path: ~/code/orchael/crew\n"), 0600); err != nil { t.Fatal(err) }
  got, err := resolveLaunchDirectory("crew", ".")
  if err != nil { t.Fatal(err) }
  want := filepath.Join(home, "code", "orchael", "crew")
  if got != want { t.Fatalf("got %q want %q", got, want) }
  got, err = resolveLaunchDirectory("legacy-id", ".")
  if err != nil || got != "." { t.Fatalf("legacy project behavior changed: %q %v", got, err) }
}
