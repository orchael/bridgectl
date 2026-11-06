package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	lookPathNode = exec.LookPath
	runCommand   = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
)

func parseRequiredNodeMajor(s string) (int, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("empty node version requirement")
	}
	majorPart, _, _ := strings.Cut(trimmed, ".")
	major, err := strconv.Atoi(strings.TrimPrefix(majorPart, "v"))
	if err != nil || major <= 0 {
		return 0, fmt.Errorf("parse required node major from %q", trimmed)
	}
	return major, nil
}

func parseNodeMajorVersion(s string) (int, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if trimmed == "" {
		return 0, fmt.Errorf("empty node version output")
	}
	majorPart, _, _ := strings.Cut(trimmed, ".")
	major, err := strconv.Atoi(majorPart)
	if err != nil || major <= 0 {
		return 0, fmt.Errorf("parse node major from %q", strings.TrimSpace(s))
	}
	return major, nil
}

// nodeVersionMatchesRequirement treats the .nvmrc major as a minimum, matching
// package.json's engines range (">=24"). .nvmrc pins the development version;
// newer Node majors are supported at runtime.
func nodeVersionMatchesRequirement(requiredMajor, actualMajor int) bool {
	return actualMajor >= requiredMajor
}

func RequiredNodeMajor(projectRoot string) (int, error) {
	data, err := os.ReadFile(filepath.Join(projectRoot, ".nvmrc"))
	if err != nil {
		return 0, fmt.Errorf("read .nvmrc: %w", err)
	}
	major, err := parseRequiredNodeMajor(string(data))
	if err != nil {
		return 0, fmt.Errorf(".nvmrc: %w", err)
	}
	return major, nil
}

func ValidateNodeRuntime(projectRoot string) error {
	requiredMajor, err := RequiredNodeMajor(projectRoot)
	if err != nil {
		return err
	}

	nodePath, err := lookPathNode("node")
	if err != nil {
		return fmt.Errorf("node not found on PATH; install Node.js %d or newer and ensure it is available before starting bridge", requiredMajor)
	}

	output, err := runCommand(nodePath, "--version")
	if err != nil {
		return fmt.Errorf("check node version via %q: %w", nodePath, err)
	}

	actualMajor, err := parseNodeMajorVersion(string(output))
	if err != nil {
		return fmt.Errorf("parse node version from %q: %w", strings.TrimSpace(string(output)), err)
	}
	if !nodeVersionMatchesRequirement(requiredMajor, actualMajor) {
		return fmt.Errorf("node on PATH is v%d but bridge requires v%d or newer (from .nvmrc)", actualMajor, requiredMajor)
	}
	return nil
}

// RequiresNodeRuntime reports whether cfg has at least one provider that
// invokes Node.js, detected by the provider binary base name being "node"
// or "nodejs". Providers that use non-Node binaries (e.g. native CLIs,
// /bin/cat) do not require the Node runtime and do not need .nvmrc validation.
func RequiresNodeRuntime(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, p := range cfg.Providers {
		base := filepath.Base(p.Binary)
		if base == "node" || base == "nodejs" {
			return true
		}
	}
	return false
}
