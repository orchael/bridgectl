package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level bridge daemon configuration.
type Config struct {
	Server       ServerConfig              `yaml:"server"`
	Security     SecurityConfig            `yaml:"security"`
	StepCA       StepCAYAMLConfig          `yaml:"step_ca"`
	TLS          TLSConfig                 `yaml:"tls"`
	Auth         AuthConfig                `yaml:"auth"`
	FeatureFlags FeatureFlagsConfig        `yaml:"feature_flags"`
	Sessions     SessionsConfig            `yaml:"sessions"`
	Input        InputConfig               `yaml:"input"`
	RateLimits   RateLimitsConfig          `yaml:"rate_limits"`
	Persistence  PersistenceConfig         `yaml:"persistence"`
	Runtime      RuntimeConfig             `yaml:"runtime"`
	RepoSetup    RepoSetupConfig           `yaml:"repo_setup"`
	Providers    map[string]ProviderConfig `yaml:"providers"`
	AllowedPaths []string                  `yaml:"allowed_paths"`
	Logging      LoggingConfig             `yaml:"logging"`
}

// SecurityConfig holds the v1.1 security model: explicit transport mode,
// certificate provider, and authorization mode. When absent in the YAML file,
// it is synthesized from legacy fields (server.listen, step_ca, tls) for
// backward compatibility.
type SecurityConfig struct {
	Transport     TransportConfig     `yaml:"transport"`
	Certificates  CertificatesConfig  `yaml:"certificates"`
	Authorization AuthorizationConfig `yaml:"authorization"`
}

// TransportMode constants for the security.transport.mode field.
const (
	TransportModeLocal = "local"
	TransportModeTLS   = "tls"
	TransportModeMTLS  = "mtls"
)

// TransportConfig controls the transport security level.
type TransportConfig struct {
	// Mode is one of "local", "tls", or "mtls".
	Mode string `yaml:"mode"`
}

// CertificatesConfig controls where certificate material comes from.
type CertificatesConfig struct {
	// Provider is the certificate provider name: "filesystem", "auto", or "stepca".
	Provider   string                   `yaml:"provider"`
	Filesystem FilesystemCertConfig     `yaml:"filesystem"`
	StepCA     StepCACertProviderConfig `yaml:"stepca"`
}

// FilesystemCertConfig holds paths for the filesystem certificate provider.
type FilesystemCertConfig struct {
	CA          string `yaml:"ca"`
	Certificate string `yaml:"certificate"`
	PrivateKey  string `yaml:"private_key"`
	ServerCert  string `yaml:"server_certificate"`
	ServerKey   string `yaml:"server_private_key"`
}

// StepCACertProviderConfig holds settings for the Step CA certificate provider.
type StepCACertProviderConfig struct {
	URL                     string `yaml:"url"`
	Root                    string `yaml:"root"`
	Provisioner             string `yaml:"provisioner"`
	ProvisionerPasswordFile string `yaml:"provisioner_password_file"`
}

// AuthorizationConfig controls the authorization mechanism.
type AuthorizationConfig struct {
	// Mode is "jwt" or "none".
	Mode string `yaml:"mode"`
}

// SecurityMode returns the resolved transport mode, falling back to "local"
// if not configured.
func (s *SecurityConfig) SecurityMode() string {
	if s.Transport.Mode != "" {
		return s.Transport.Mode
	}
	return TransportModeLocal
}

// IsSecurityConfigured reports whether the security block was explicitly set
// in the YAML (i.e. has a non-empty transport mode).
func (s *SecurityConfig) IsSecurityConfigured() bool {
	return s.Transport.Mode != ""
}

// RuntimeConfig controls how the bridge locates provider CLIs and the Node.js
// runtime. When ProviderRoot is set, Node version validation reads
// {provider_root}/.nvmrc and relative provider binary/arg paths are resolved
// relative to {provider_root} instead of the daemon working directory. When
// empty, existing CWD-relative behaviour is preserved.
type RuntimeConfig struct {
	ProviderRoot string `yaml:"provider_root"`
}

// RepoSetupConfig controls repo-local pre-agent setup execution.
type RepoSetupConfig struct {
	Enabled        *bool  `yaml:"enabled"`
	ConfigPath     string `yaml:"config_path"`
	DefaultTimeout string `yaml:"default_timeout"`
	MaxTimeout     string `yaml:"max_timeout"`
}

func (r RepoSetupConfig) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

type ServerConfig struct {
	Listen                   string   `yaml:"listen"`
	SANs                     []string `yaml:"san"`
	CertValidity             string   `yaml:"cert_validity"`
	CertRenewalCheckInterval string   `yaml:"cert_renewal_check_interval"`
}

// StepCAYAMLConfig holds Step CA settings from the YAML config file.
type StepCAYAMLConfig struct {
	URL                     string               `yaml:"url"`
	Root                    string               `yaml:"root"`
	Provisioner             string               `yaml:"provisioner"`
	ProvisionerPasswordFile string               `yaml:"provisioner_password_file"`
	Clients                 []StepCAClientConfig `yaml:"clients"`
}

// StepCAClientConfig declares a Step CA-authenticated client whose bridge JWT
// public key should be loaded at startup when available.
type StepCAClientConfig struct {
	Issuer   string `yaml:"issuer"`
	KeyPath  string `yaml:"key_path"`
	Required bool   `yaml:"required"`
}

type TLSConfig struct {
	CABundle string `yaml:"ca_bundle"`
	Cert     string `yaml:"cert"`
	Key      string `yaml:"key"`
}

type AuthConfig struct {
	JWTPublicKeys []JWTKeyConfig `yaml:"jwt_public_keys"`
	JWTAudience   string         `yaml:"jwt_audience"`
	JWTMaxTTL     string         `yaml:"jwt_max_ttl"`
}

type FeatureFlagsConfig struct {
	ProviderFallbacks bool `yaml:"provider_fallbacks"`
}

type JWTKeyConfig struct {
	Issuer  string `yaml:"issuer"`
	KeyPath string `yaml:"key_path"`
}

type SessionsConfig struct {
	MaxPerProject            int    `yaml:"max_per_project"`
	MaxGlobal                int    `yaml:"max_global"`
	IdleTimeout              string `yaml:"idle_timeout"`
	StopGracePeriod          string `yaml:"stop_grace_period"`
	EventBufferSize          int    `yaml:"event_buffer_size"`
	MaxSubscribersPerSession int    `yaml:"max_subscribers_per_session"`
	SubscriberTTL            string `yaml:"subscriber_ttl"`
}

type InputConfig struct {
	MaxSizeBytes int `yaml:"max_size_bytes"`
}

type RateLimitsConfig struct {
	GlobalRPS                  float64 `yaml:"global_rps"`
	GlobalBurst                int     `yaml:"global_burst"`
	StartSessionPerClientRPS   float64 `yaml:"start_session_per_client_rps"`
	StartSessionPerClientBurst int     `yaml:"start_session_per_client_burst"`
	SendInputPerSessionRPS     float64 `yaml:"send_input_per_session_rps"`
	SendInputPerSessionBurst   int     `yaml:"send_input_per_session_burst"`
}

type ProviderConfig struct {
	Binary          string   `yaml:"binary"`
	Mode            string   `yaml:"mode"` // deprecated: no longer supported; remove from config
	Args            []string `yaml:"args"`
	StartupTimeout  string   `yaml:"startup_timeout"`
	ValidateStartup *bool    `yaml:"validate_startup"`
	StartupProbe    string   `yaml:"startup_probe"`
	RequiredEnv     []string `yaml:"required_env"`
	PTY             *bool    `yaml:"pty"` // deprecated: PTY is the default; remove this field
	StreamJSON      bool     `yaml:"stream_json"`
	StripANSI       bool     `yaml:"strip_ansi"`
	// PromptPattern is a regex matched against PTY output lines. When it
	// matches the first time, AGENT_READY is emitted; on subsequent matches
	// after output, RESPONSE_COMPLETE is emitted.
	PromptPattern string `yaml:"prompt_pattern"`
	// Fallbacks is an ordered list of provider IDs to try when this provider
	// is unavailable at session start time. At most 2 entries are allowed.
	Fallbacks []string `yaml:"fallbacks"`
	// Transport selects the provider transport backend. Supported values are
	// "" (default PTY/stdio), and "opencode_server" (headless OpenCode HTTP/SSE).
	Transport string `yaml:"transport"`
	// Hostname is the address to bind the OpenCode server to. Only used when
	// Transport is "opencode_server". Defaults to "127.0.0.1".
	Hostname string `yaml:"hostname"`
	// PortRange specifies the port range for the OpenCode server in
	// "start-end" format (e.g. "4100-4199"). Only used when Transport is
	// "opencode_server". Defaults to "4100-4199".
	PortRange string `yaml:"port_range"`
}

func (p ProviderConfig) ShouldValidateStartup() bool {
	return p.ValidateStartup == nil || *p.ValidateStartup
}

type PersistenceConfig struct {
	// DBPath is the path to the bbolt database file used to persist session
	// metadata and PTY output chunks across daemon restarts. An empty string
	// disables persistence.
	DBPath string `yaml:"db_path"`
	// ChunkStorageBytes is the soft upper bound on total chunk bytes stored per
	// session in the database. 0 means unlimited (the default). Enforcement is
	// planned for a future release; this field is reserved for configuration
	// compatibility.
	ChunkStorageBytes int `yaml:"chunk_storage_bytes"`
}

type LoggingConfig struct {
	Level          string   `yaml:"level"`
	Format         string   `yaml:"format"`
	RedactPatterns []string `yaml:"redact_patterns"`
}

// Load reads and parses a YAML configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(cfg)
	synthesizeSecurity(cfg)
	if err := expandRuntimeConfig(cfg); err != nil {
		return nil, err
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// HasExplicitServerListen reports whether a YAML config file explicitly sets
// server.listen, before defaults are applied by Load.
func HasExplicitServerListen(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}
	var raw struct {
		Server struct {
			Listen *string `yaml:"listen"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return false, fmt.Errorf("parse config: %w", err)
	}
	return raw.Server.Listen != nil && strings.TrimSpace(*raw.Server.Listen) != "", nil
}

// ParseDuration is a helper that parses a duration string with a fallback.
func ParseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// synthesizeSecurity fills in cfg.Security from legacy fields when the
// security block is not explicitly configured in the YAML file. This
// provides backward compatibility so existing configs continue to work
// after the v1.1 security model is introduced.
func synthesizeSecurity(cfg *Config) {
	if cfg.Security.IsSecurityConfigured() {
		// The new security block is explicitly set — nothing to synthesize.
		return
	}

	// Legacy detection: determine mode from existing fields.
	hasListen := cfg.Server.Listen != "" && cfg.Server.Listen != "0.0.0.0:9445"
	hasStepCA := cfg.StepCA.URL != ""
	hasExplicitTLS := cfg.TLS.CABundle != ""

	if !hasListen && !hasStepCA && !hasExplicitTLS {
		// No remote/secure config at all — default to local.
		cfg.Security.Transport.Mode = TransportModeLocal
		cfg.Security.Authorization.Mode = "none"
		cfg.Security.Certificates.Provider = "auto"
		return
	}

	// Remote mode — determine provider and transport.
	cfg.Security.Transport.Mode = TransportModeMTLS

	switch {
	case hasStepCA:
		cfg.Security.Certificates.Provider = "stepca"
		cfg.Security.Certificates.StepCA = StepCACertProviderConfig{
			URL:                     cfg.StepCA.URL,
			Root:                    cfg.StepCA.Root,
			Provisioner:             cfg.StepCA.Provisioner,
			ProvisionerPasswordFile: cfg.StepCA.ProvisionerPasswordFile,
		}
	case hasExplicitTLS:
		cfg.Security.Certificates.Provider = "filesystem"
		cfg.Security.Certificates.Filesystem = FilesystemCertConfig{
			CA:         cfg.TLS.CABundle,
			ServerCert: cfg.TLS.Cert,
			ServerKey:  cfg.TLS.Key,
		}
	default:
		cfg.Security.Certificates.Provider = "auto"
	}

	cfg.Security.Authorization.Mode = "jwt"
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = "0.0.0.0:9445"
	}
	if cfg.Auth.JWTAudience == "" {
		cfg.Auth.JWTAudience = "bridge"
	}
	if cfg.Auth.JWTMaxTTL == "" {
		cfg.Auth.JWTMaxTTL = "5m"
	}
	if cfg.Sessions.MaxPerProject == 0 {
		cfg.Sessions.MaxPerProject = 5
	}
	if cfg.Sessions.MaxGlobal == 0 {
		cfg.Sessions.MaxGlobal = 20
	}
	if cfg.Sessions.EventBufferSize == 0 {
		cfg.Sessions.EventBufferSize = 10000
	}
	if cfg.Sessions.StopGracePeriod == "" {
		cfg.Sessions.StopGracePeriod = "10s"
	}
	if cfg.Sessions.IdleTimeout == "" {
		cfg.Sessions.IdleTimeout = "30m"
	}
	if cfg.Sessions.MaxSubscribersPerSession == 0 {
		cfg.Sessions.MaxSubscribersPerSession = 10
	}
	if cfg.Sessions.SubscriberTTL == "" {
		cfg.Sessions.SubscriberTTL = "30m"
	}
	if cfg.Input.MaxSizeBytes == 0 {
		cfg.Input.MaxSizeBytes = 65536
	}
	if cfg.RateLimits.GlobalRPS == 0 {
		cfg.RateLimits.GlobalRPS = 50
	}
	if cfg.RateLimits.GlobalBurst == 0 {
		cfg.RateLimits.GlobalBurst = 100
	}
	if cfg.RateLimits.StartSessionPerClientRPS == 0 {
		cfg.RateLimits.StartSessionPerClientRPS = 1
	}
	if cfg.RateLimits.StartSessionPerClientBurst == 0 {
		cfg.RateLimits.StartSessionPerClientBurst = 3
	}
	if cfg.RateLimits.SendInputPerSessionRPS == 0 {
		cfg.RateLimits.SendInputPerSessionRPS = 20
	}
	if cfg.RateLimits.SendInputPerSessionBurst == 0 {
		cfg.RateLimits.SendInputPerSessionBurst = 50
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
	if cfg.RepoSetup.ConfigPath == "" {
		cfg.RepoSetup.ConfigPath = ".bridgectl.yaml"
	}
	if cfg.RepoSetup.DefaultTimeout == "" {
		cfg.RepoSetup.DefaultTimeout = "2m"
	}
	if cfg.RepoSetup.MaxTimeout == "" {
		cfg.RepoSetup.MaxTimeout = "15m"
	}
}

func expandRuntimeConfig(cfg *Config) error {
	if cfg.Runtime.ProviderRoot == "" {
		return nil
	}
	expanded, err := expandProviderRoot(cfg.Runtime.ProviderRoot)
	if err != nil {
		return err
	}
	cfg.Runtime.ProviderRoot = expanded
	return nil
}

func expandProviderRoot(root string) (string, error) {
	var missing []string
	expanded := os.Expand(root, func(name string) string {
		switch name {
		case "HOME", "XDG_DATA_HOME":
			value := os.Getenv(name)
			if value == "" {
				missing = append(missing, name)
			}
			return value
		default:
			missing = append(missing, name)
			return ""
		}
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("config: runtime.provider_root references unset or unsupported environment variable %q", missing[0])
	}
	return expanded, nil
}

func validate(cfg *Config) error {
	if err := validateSecurity(cfg); err != nil {
		return err
	}
	if cfg.Server.Listen == "" {
		return fmt.Errorf("config: server.listen is required")
	}
	if cfg.Input.MaxSizeBytes <= 0 {
		return fmt.Errorf("config: input.max_size_bytes must be > 0")
	}
	if cfg.Sessions.MaxPerProject < 0 || cfg.Sessions.MaxGlobal < 0 {
		return fmt.Errorf("config: session limits must be >= 0")
	}
	if cfg.Sessions.EventBufferSize <= 0 {
		return fmt.Errorf("config: sessions.event_buffer_size must be > 0")
	}
	if cfg.Sessions.MaxSubscribersPerSession <= 0 {
		return fmt.Errorf("config: sessions.max_subscribers_per_session must be > 0")
	}
	if cfg.RateLimits.GlobalRPS <= 0 || cfg.RateLimits.GlobalBurst <= 0 {
		return fmt.Errorf("config: rate_limits.global_rps/global_burst must be > 0")
	}
	if cfg.RateLimits.StartSessionPerClientRPS <= 0 || cfg.RateLimits.StartSessionPerClientBurst <= 0 {
		return fmt.Errorf("config: rate_limits.start_session_per_client_rps/start_session_per_client_burst must be > 0")
	}
	if cfg.RateLimits.SendInputPerSessionRPS <= 0 || cfg.RateLimits.SendInputPerSessionBurst <= 0 {
		return fmt.Errorf("config: rate_limits.send_input_per_session_rps/send_input_per_session_burst must be > 0")
	}
	if cfg.Runtime.ProviderRoot != "" && !filepath.IsAbs(cfg.Runtime.ProviderRoot) {
		return fmt.Errorf("config: runtime.provider_root must be an absolute path, got %q", cfg.Runtime.ProviderRoot)
	}
	if cfg.RepoSetup.ConfigPath == "" {
		return fmt.Errorf("config: repo_setup.config_path is required")
	}
	if filepath.IsAbs(cfg.RepoSetup.ConfigPath) {
		return fmt.Errorf("config: repo_setup.config_path must be relative, got %q", cfg.RepoSetup.ConfigPath)
	}
	if hasParentPathComponent(cfg.RepoSetup.ConfigPath) {
		return fmt.Errorf("config: repo_setup.config_path must not contain '..', got %q", cfg.RepoSetup.ConfigPath)
	}
	defaultSetupTimeout, err := time.ParseDuration(cfg.RepoSetup.DefaultTimeout)
	if err != nil {
		return fmt.Errorf("config: repo_setup.default_timeout: %w", err)
	}
	if defaultSetupTimeout <= 0 {
		return fmt.Errorf("config: repo_setup.default_timeout must be > 0")
	}
	maxSetupTimeout, err := time.ParseDuration(cfg.RepoSetup.MaxTimeout)
	if err != nil {
		return fmt.Errorf("config: repo_setup.max_timeout: %w", err)
	}
	if maxSetupTimeout <= 0 {
		return fmt.Errorf("config: repo_setup.max_timeout must be > 0")
	}
	if defaultSetupTimeout > maxSetupTimeout {
		return fmt.Errorf("config: repo_setup.default_timeout must not exceed repo_setup.max_timeout")
	}
	if _, err := time.ParseDuration(cfg.Auth.JWTMaxTTL); err != nil {
		return fmt.Errorf("config: auth.jwt_max_ttl: %w", err)
	}
	if cfg.Server.CertValidity != "" {
		d, err := time.ParseDuration(cfg.Server.CertValidity)
		if err != nil {
			return fmt.Errorf("config: server.cert_validity: %w", err)
		}
		if d < 0 {
			return fmt.Errorf("config: server.cert_validity must not be negative")
		}
	}
	if cfg.Server.CertRenewalCheckInterval != "" {
		d, err := time.ParseDuration(cfg.Server.CertRenewalCheckInterval)
		if err != nil {
			return fmt.Errorf("config: server.cert_renewal_check_interval: %w", err)
		}
		if d < 0 {
			return fmt.Errorf("config: server.cert_renewal_check_interval must not be negative")
		}
	}
	for i, client := range cfg.StepCA.Clients {
		if !safeIssuerName(client.Issuer) {
			return fmt.Errorf("config: step_ca.clients[%d].issuer must start with an alphanumeric character and contain only alphanumerics, hyphens, underscores, or dots", i)
		}
	}
	if _, err := time.ParseDuration(cfg.Sessions.IdleTimeout); err != nil {
		return fmt.Errorf("config: sessions.idle_timeout: %w", err)
	}
	if _, err := time.ParseDuration(cfg.Sessions.StopGracePeriod); err != nil {
		return fmt.Errorf("config: sessions.stop_grace_period: %w", err)
	}
	if _, err := time.ParseDuration(cfg.Sessions.SubscriberTTL); err != nil {
		return fmt.Errorf("config: sessions.subscriber_ttl: %w", err)
	}
	for name, provider := range cfg.Providers {
		if provider.Binary == "" {
			return fmt.Errorf("config: providers.%s.binary is required", name)
		}
		if provider.Mode != "" {
			return fmt.Errorf("config: providers.%s.mode is no longer supported; remove the field and use stream_json: true only for JSONL providers", name)
		}
		if provider.Transport != "" {
			switch provider.Transport {
			case "opencode_server":
				// valid
			default:
				return fmt.Errorf("config: providers.%s.transport must be one of: opencode_server; got %q", name, provider.Transport)
			}
		}
		if provider.PortRange != "" {
			if _, _, err := ParsePortRange(provider.PortRange); err != nil {
				return fmt.Errorf("config: providers.%s.port_range: %w", name, err)
			}
		}
		if provider.PTY != nil {
			return fmt.Errorf("config: providers.%s.pty is no longer supported; PTY is the default and stream_json: true opts out of PTY allocation", name)
		}
		if provider.StartupProbe != "" {
			switch provider.StartupProbe {
			case "prompt", "output", "none":
			default:
				return fmt.Errorf("config: providers.%s.startup_probe must be one of prompt, output, none", name)
			}
		}
		if provider.StartupTimeout != "" {
			if _, err := time.ParseDuration(provider.StartupTimeout); err != nil {
				return fmt.Errorf("config: providers.%s.startup_timeout: %w", name, err)
			}
		}
		for i, envName := range provider.RequiredEnv {
			if strings.TrimSpace(envName) == "" {
				return fmt.Errorf("config: providers.%s.required_env[%d] must not be empty", name, i)
			}
		}
		if len(provider.Fallbacks) > 2 {
			return fmt.Errorf("config: providers.%s.fallbacks must have at most 2 entries", name)
		}
		for i, fb := range provider.Fallbacks {
			if fb == name {
				return fmt.Errorf("config: providers.%s.fallbacks[%d]: provider cannot be its own fallback", name, i)
			}
			if _, ok := cfg.Providers[fb]; !ok {
				return fmt.Errorf("config: providers.%s.fallbacks[%d]: unknown provider %q", name, i, fb)
			}
		}
	}
	return nil
}

func validateSecurity(cfg *Config) error {
	mode := cfg.Security.SecurityMode()
	switch mode {
	case TransportModeLocal, TransportModeTLS, TransportModeMTLS:
		// valid
	default:
		return fmt.Errorf("config: security.transport.mode must be one of local, tls, mtls; got %q", mode)
	}

	provider := cfg.Security.Certificates.Provider
	switch provider {
	case "filesystem", "auto", "stepca", "":
		// valid (empty means not configured, will default to "auto")
	default:
		return fmt.Errorf("config: security.certificates.provider must be one of filesystem, auto, stepca; got %q", provider)
	}

	authMode := cfg.Security.Authorization.Mode
	switch authMode {
	case "jwt", "none", "":
		// valid (empty means not configured)
	default:
		return fmt.Errorf("config: security.authorization.mode must be one of jwt, none; got %q", authMode)
	}

	return nil
}

func hasParentPathComponent(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(os.PathSeparator)) {
		if part == ".." {
			return true
		}
	}
	return false
}

func safeIssuerName(issuer string) bool {
	if issuer == "" {
		return false
	}
	for i, r := range issuer {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
		if i == 0 && (r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// ParsePortRange parses a "start-end" port range string and returns the start
// (inclusive) and end (exclusive) port numbers. For example, "4100-4199"
// returns (4100, 4200, nil).
func ParsePortRange(s string) (start, end int, err error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("port_range must be in 'start-end' format, got %q", s)
	}
	start, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port range start: %w", err)
	}
	endInclusive, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port range end: %w", err)
	}
	end = endInclusive + 1 // make end exclusive
	if start <= 0 || endInclusive <= 0 || start > endInclusive {
		return 0, 0, fmt.Errorf("port_range start must be <= end and both > 0, got %q", s)
	}
	if start > 65535 || endInclusive > 65535 {
		return 0, 0, fmt.Errorf("port_range values must be <= 65535, got %q", s)
	}
	return start, end, nil
}
