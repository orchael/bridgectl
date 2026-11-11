package main

import (
  "fmt"
  "os"
  "path/filepath"
  "strings"

  "gopkg.in/yaml.v3"
)

type launchConfig struct {
  Project struct { ID string `yaml:"id"` } `yaml:"project"`
  Agent struct { Provider string `yaml:"provider"` } `yaml:"agent"`
}

type projectRegistry struct {
  Projects map[string]struct { Path string `yaml:"path"` } `yaml:"projects"`
}

func expandProjectPath(path string) string {
  if path == "~" || strings.HasPrefix(path, "~/") {
    home, err := os.UserHomeDir()
    if err == nil { return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")) }
  }
  return path
}

func resolveLaunchDirectory(project, dir string) (string, error) {
  if dir != "." || project == "" || project == "local" { return dir, nil }
  home, err := os.UserHomeDir()
  if err != nil { return dir, nil }
  path := filepath.Join(home, ".config", "bridgectl", "projects.yaml")
  data, err := os.ReadFile(path)
  if os.IsNotExist(err) { return dir, nil }
  if err != nil { return "", fmt.Errorf("read project registry: %w", err) }
  var registry projectRegistry
  if err := yaml.Unmarshal(data, &registry); err != nil { return "", fmt.Errorf("parse project registry: %w", err) }
  entry, ok := registry.Projects[project]
  if !ok { return dir, nil } // Preserve legacy project IDs.
  if entry.Path == "" { return "", fmt.Errorf("project %q has an empty path", project) }
  return expandProjectPath(entry.Path), nil
}

func loadLaunchConfig(dir string) (*launchConfig, error) {
  current := dir
  for {
    candidate := filepath.Join(current, ".bridgectl.yaml")
    data, err := os.ReadFile(candidate)
    if err == nil {
      var cfg launchConfig
      if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
        return nil, fmt.Errorf("parse %s: %w", candidate, err)
      }
      return &cfg, nil
    }
    if !os.IsNotExist(err) { return nil, fmt.Errorf("read %s: %w", candidate, err) }
    parent := filepath.Dir(current)
    if parent == current { return nil, nil }
    current = parent
  }
}
