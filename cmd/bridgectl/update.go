package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/localserver"
)

var updateVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

type updateConfig struct {
	Login  bool   `yaml:"login"`
	Doctor bool   `yaml:"doctor"`
	Bridge string `yaml:"bridge"`
}

func loadUpdateConfig(path string) (updateConfig, error) {
	cfg := updateConfig{Bridge: productionBridgeURL}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	var file struct {
		Version int       `yaml:"version"`
		Update  yaml.Node `yaml:"update"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Version != 1 {
		return cfg, fmt.Errorf("%s: version must be 1", path)
	}
	if file.Update.Kind != 0 {
		updateData, err := yaml.Marshal(&file.Update)
		if err != nil {
			return cfg, err
		}
		decoder := yaml.NewDecoder(strings.NewReader(string(updateData)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse %s update: %w", path, err)
		}
	}
	if cfg.Bridge == "" {
		cfg.Bridge = productionBridgeURL
	}
	if _, err := bridgeURL(cfg.Bridge); err != nil {
		return cfg, fmt.Errorf("update bridge: %w", err)
	}
	return cfg, nil
}

func runUpdateCommand(ctx context.Context, cmd *cobra.Command, name string, args ...string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is required: %w", name, err)
	}
	c := exec.CommandContext(ctx, path, args...)
	c.Stdin = cmd.InOrStdin()
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return path, nil
}

func updateOutput(ctx context.Context, name string, args ...string) (string, error) {
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probe, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func installedPackageManager(ctx context.Context) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	if _, err := exec.LookPath("dpkg-query"); err == nil {
		out, err := updateOutput(ctx, "dpkg-query", "-S", resolved)
		if err == nil && aptOwnsBinary(out) {
			return "apt", nil
		}
	}
	if _, err := exec.LookPath("brew"); err == nil {
		out, err := updateOutput(ctx, "brew", "list", "--cask", "--versions", "bridgectl")
		if err == nil && strings.HasPrefix(out, "bridgectl ") {
			prefix, err := updateOutput(ctx, "brew", "--prefix")
			if err == nil && brewOwnsBinary(prefix, resolved) {
				return "brew", nil
			}
		}
	}
	return "", fmt.Errorf("this bridgectl binary is not managed by apt or Homebrew: %s", resolved)
}

func aptOwnsBinary(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		pkg, _, ok := strings.Cut(line, ": ")
		if ok && (pkg == "bridgectl" || strings.HasPrefix(pkg, "bridgectl:")) {
			return true
		}
	}
	return false
}

func brewOwnsBinary(prefix, binary string) bool {
	return prefix != "" && strings.HasPrefix(binary, filepath.Join(prefix, "Caskroom", "bridgectl")+string(os.PathSeparator))
}

func newUpdateCmd() *cobra.Command {
	var versionFlag, configPath, bridgeFlag string
	var loginFlag, doctorFlag bool
	cmd := &cobra.Command{
		Use: "update", Short: "Update the installed bridgectl package",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadUpdateConfig(configPath)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("login") {
				cfg.Login = loginFlag
			}
			if cmd.Flags().Changed("doctor") {
				cfg.Doctor = doctorFlag
			}
			if cmd.Flags().Changed("bridge") {
				cfg.Bridge = bridgeFlag
			}
			origin, err := bridgeURL(cfg.Bridge)
			if err != nil {
				return err
			}
			cfg.Bridge = origin
			if err := updateSelf(cmd, versionFlag); err != nil {
				return err
			}
			if cfg.Login {
				if enrollment, err := readEnrollmentMetadata(); err == nil && strings.TrimRight(enrollment.BridgeURL, "/") != strings.TrimRight(cfg.Bridge, "/") {
					return fmt.Errorf("already enrolled with %s; use bridgectl login %s --force to change Bridge", enrollment.BridgeURL, cfg.Bridge)
				}
				if _, err := runUpdateCommand(cmd.Context(), cmd, "bridgectl", "login", cfg.Bridge); err != nil {
					return fmt.Errorf("bridge login: %w", err)
				}
			}
			if cfg.Doctor {
				if _, err := runUpdateCommand(cmd.Context(), cmd, "bridgectl", "doctor"); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&versionFlag, "version", "", "package version to install (default: latest)")
	cmd.Flags().StringVar(&configPath, "config", ".bridgectl.yaml", "repository setup configuration file")
	cmd.Flags().BoolVar(&loginFlag, "login", false, "log into Bridge after updating")
	cmd.Flags().BoolVar(&doctorFlag, "doctor", false, "run doctor after updating")
	cmd.Flags().StringVar(&bridgeFlag, "bridge", "", "Bridge HTTPS origin for login")
	cmd.AddCommand(newUpdateAgentsCmd())
	return cmd
}

func updateSelf(cmd *cobra.Command, requested string) error {
	if requested != "" && !updateVersionPattern.MatchString(requested) {
		return fmt.Errorf("invalid version %q", requested)
	}
	manager, err := installedPackageManager(cmd.Context())
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	switch manager {
	case "apt":
		privilege := ""
		if os.Geteuid() != 0 {
			privilege = "sudo"
		}
		run := func(name string, args ...string) error {
			if privilege != "" {
				args = append([]string{name}, args...)
				name = privilege
			}
			_, err := runUpdateCommand(ctx, cmd, name, args...)
			return err
		}
		if err := run("apt-get", "update"); err != nil {
			return err
		}
		candidate := strings.TrimPrefix(requested, "v")
		if candidate == "" {
			out, err := updateOutput(ctx, "apt-cache", "policy", "bridgectl")
			if err != nil {
				return err
			}
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "Candidate:") {
					candidate = strings.TrimSpace(strings.TrimPrefix(line, "Candidate:"))
					break
				}
			}
			if candidate == "" || candidate == "(none)" {
				return errors.New("no bridgectl candidate in configured apt repositories")
			}
		}
		if requested != "" {
			out, err := updateOutput(ctx, "apt-cache", "madison", "bridgectl")
			if err != nil {
				return err
			}
			found := false
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Split(line, "|")
				if len(fields) >= 2 && strings.TrimSpace(fields[1]) == candidate {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("bridgectl version %s is unavailable in configured apt repositories", candidate)
			}
		}
		if err := run("apt-get", "install", "-y", "bridgectl="+candidate); err != nil {
			return err
		}
	case "brew":
		if _, err := runUpdateCommand(ctx, cmd, "brew", "update"); err != nil {
			return err
		}
		if requested != "" {
			if err := installVersionedBrewCask(cmd, strings.TrimPrefix(requested, "v")); err != nil {
				return err
			}
			break
		}
		out, err := updateOutput(ctx, "brew", "outdated", "--cask", "--quiet", "bridgectl")
		if err != nil {
			return err
		}
		if out == "" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "bridgectl is already at the latest Homebrew cask version.")
			return nil
		}
		if _, err := runUpdateCommand(ctx, cmd, "brew", "upgrade", "--cask", "orchael/bridgectl/bridgectl"); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "bridgectl package updated with %s; restart the server after active sessions finish.\n", manager)
	return nil
}

func installVersionedBrewCask(cmd *cobra.Command, v string) error {
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("no Homebrew release archive for %s", arch)
	}
	archive := fmt.Sprintf("bridgectl_%s_darwin_%s.tar.gz", v, arch)
	base := "https://github.com/orchael/bridgectl/releases/download/v" + v + "/"
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, base+"checksums.txt", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release v%s checksums: %s", v, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	sha := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == archive && regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(fields[0]) {
			sha = fields[0]
			break
		}
	}
	if sha == "" {
		return fmt.Errorf("release v%s has no checksum for %s", v, archive)
	}
	dir, err := os.MkdirTemp("", "bridgectl-cask-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "bridgectl.rb")
	content := fmt.Sprintf("cask \"bridgectl\" do\n  version \"%s\"\n  sha256 \"%s\"\n  url \"%s%s\"\n  name \"bridgectl\"\n  homepage \"https://github.com/orchael/bridgectl\"\n  binary \"bridgectl\"\nend\n", v, sha, base, archive)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	_, err = runUpdateCommand(cmd.Context(), cmd, "brew", "install", "--cask", "--force", path)
	return err
}

var agentPackages = map[string]string{"claude": "@anthropic-ai/claude-code", "codex": "@openai/codex", "opencode": "opencode-ai"}

func npmVersion(ctx context.Context, client *http.Client, registry, pkg, version string) (string, error) {
	path := strings.ReplaceAll(pkg, "/", "%2F")
	if version == "" {
		version = "latest"
	} else {
		version = strings.TrimPrefix(version, "v")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/"+path+"/"+version, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry returned %s for %s@%s", resp.Status, pkg, version)
	}
	var result struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if !updateVersionPattern.MatchString(result.Version) {
		return "", fmt.Errorf("npm returned invalid version %q for %s", result.Version, pkg)
	}
	return result.Version, nil
}

func defaultAgentRuntimeDir() string {
	if path := defaultServerConfigPath(localserver.StateDir()); path != "" {
		if cfg, err := config.Load(path); err == nil && cfg.Runtime.ProviderRoot != "" {
			return cfg.Runtime.ProviderRoot
		}
	}
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "bridgectl", "providers")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "bridgectl", "providers")
}

func newUpdateAgentsCmd() *cobra.Command {
	var versionFlag, runtimeDir string
	var claudeVersion, codexVersion, opencodeVersion string
	cmd := &cobra.Command{Use: "agents [all|claude|codex|opencode]", Short: "Install npm agent versions into the provider runtime", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		selection := "all"
		if len(args) > 0 {
			selection = args[0]
		}
		if selection != "all" && agentPackages[selection] == "" {
			return fmt.Errorf("unknown npm agent %q (choose claude, codex, opencode, or all)", selection)
		}
		if versionFlag != "" && !updateVersionPattern.MatchString(versionFlag) {
			return fmt.Errorf("invalid version %q", versionFlag)
		}
		if versionFlag != "" && selection == "all" {
			return errors.New("--version requires a specific agent; use per-agent version flags with all")
		}
		versions := map[string]string{"claude": claudeVersion, "codex": codexVersion, "opencode": opencodeVersion}
		for name, v := range versions {
			if v != "" && !updateVersionPattern.MatchString(v) {
				return fmt.Errorf("invalid %s version %q", name, v)
			}
			if selection != "all" && name != selection && v != "" {
				return fmt.Errorf("--%s-version requires %s or all", name, name)
			}
		}
		if versionFlag != "" && versions[selection] != "" {
			return fmt.Errorf("use either --version or --%s-version", selection)
		}
		if runtimeDir == "" {
			runtimeDir = defaultAgentRuntimeDir()
		}
		if !filepath.IsAbs(runtimeDir) {
			return errors.New("--runtime-dir must be absolute")
		}
		if _, err := os.Stat(filepath.Join(runtimeDir, "package.json")); err != nil {
			return fmt.Errorf("provider runtime is not initialized at %s: %w", runtimeDir, err)
		}
		if _, err := exec.LookPath("corepack"); err != nil {
			return errors.New("corepack is required; install Node.js 24")
		}
		client := &http.Client{Timeout: 15 * time.Second}
		selected := []string{selection}
		if selection == "all" {
			selected = []string{"claude", "codex", "opencode"}
		}
		var packages []string
		for _, name := range selected {
			pkg := agentPackages[name]
			requested := versions[name]
			if versionFlag != "" {
				requested = versionFlag
			}
			ver, err := npmVersion(cmd.Context(), client, "https://registry.npmjs.org", pkg, requested)
			if err != nil {
				return err
			}
			packages = append(packages, pkg+"@"+ver)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Installing %s in %s\n", strings.Join(packages, ", "), runtimeDir)
		installArgs := append([]string{"pnpm", "add", "--save-exact", "--prod"}, packages...)
		c := exec.CommandContext(cmd.Context(), "corepack", installArgs...)
		c.Dir = runtimeDir
		c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
		if err := c.Run(); err != nil {
			return fmt.Errorf("install agents: %w", err)
		}
		return nil
	}}
	cmd.Flags().StringVar(&versionFlag, "version", "", "exact npm version for one agent (default: latest)")
	cmd.Flags().StringVar(&claudeVersion, "claude-version", "", "exact Claude version for an all-agent update")
	cmd.Flags().StringVar(&codexVersion, "codex-version", "", "exact Codex version for an all-agent update")
	cmd.Flags().StringVar(&opencodeVersion, "opencode-version", "", "exact OpenCode version for an all-agent update")
	cmd.Flags().StringVar(&runtimeDir, "runtime-dir", "", "provider runtime directory (default: user data directory)")
	return cmd
}
