// Package localserver provides an embeddable gRPC bridge server for local
// and remote use. In local mode it runs without TLS on a unix-domain socket.
// In secure mode (--listen flag) it binds to a TCP address with mTLS + JWT.
package localserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/auth"
	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/certprovider"
	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/pki"
	"github.com/orchael/bridgectl/internal/provider"
	"github.com/orchael/bridgectl/internal/redact"
	"github.com/orchael/bridgectl/internal/reposetup"
	"github.com/orchael/bridgectl/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// StateDir returns the bridgectl state directory. It respects the
// BRIDGECTL_STATE_DIR environment variable for testing; otherwise
// defaults to ~/.config/bridgectl.
func StateDir() string {
	if dir := os.Getenv("BRIDGECTL_STATE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".config/bridgectl")
}

// SocketPath returns the default unix socket path.
func SocketPath() string {
	return filepath.Join(StateDir(), "server.sock")
}

// PIDPath returns the path to the server PID file.
func PIDPath() string {
	return filepath.Join(StateDir(), "server.pid")
}

// AddrPath returns the path to the server address file (TCP fallback on Windows).
func AddrPath() string {
	return filepath.Join(StateDir(), "server.addr")
}

// ServerNamePath returns the path to the file recording the TLS name local
// clients should use when probing or dialing a secure server.
func ServerNamePath() string {
	return filepath.Join(StateDir(), "server.name")
}

// Server wraps all the components needed for a local bridge server.
type Server struct {
	grpcServer *grpc.Server
	supervisor *bridge.Supervisor
	store      bridge.SessionStore // non-nil when persistence is enabled
	registry   *bridge.Registry
	listener   net.Listener
	logger     *slog.Logger
	stateDir   string
	mu         sync.Mutex
	stopped    bool

	// providerFallbacks is the resolved fallback map passed to the bridge
	// server. Nil when the feature flag is disabled.
	providerFallbacks map[string][]string

	// Certificate renewal (secure mode only).
	renewCancel              context.CancelFunc               // cancels the renewal goroutine
	serverSANs               []string                         // SANs for cert re-issuance
	stepCA                   *StepCAConfig                    // nil for auto-PKI mode (legacy)
	certProvider             certprovider.CertificateProvider // v1.1 provider (may be nil during transition)
	pkiMat                   *PKIMaterial                     // paths to cert/key files
	certValidity             time.Duration                    // server cert validity for auto-PKI renewal
	certRenewalCheckInterval time.Duration                    // how often to check cert expiry
}

// ServerMode represents how the server is running.
type ServerMode string

const (
	// ModeLocal is the default mode: unix socket, no auth.
	ModeLocal ServerMode = "local"
	// ModeTLS uses TCP + server TLS + JWT for remote access on trusted networks.
	ModeTLS ServerMode = "tls"
	// ModeMTLS uses TCP + mutual TLS + JWT for production/zero-trust access.
	ModeMTLS ServerMode = "mtls"
	// ModeSecure is a deprecated alias for ModeMTLS. It is recognized for
	// backward compatibility with existing server.mode files and tests.
	ModeSecure ServerMode = "secure"

	sessionDrainShutdownTimeout = 30 * time.Second
)

// IsSecureMode reports whether the given mode requires TLS (either
// server-only TLS or mutual TLS).
func IsSecureMode(mode ServerMode) bool {
	return mode == ModeSecure || mode == ModeMTLS || mode == ModeTLS
}

// IsMutualTLS reports whether the given mode requires client certificates.
func IsMutualTLS(mode ServerMode) bool {
	return mode == ModeSecure || mode == ModeMTLS
}

// ModePath returns the path to the server mode file.
func ModePath() string {
	return filepath.Join(StateDir(), "server.mode")
}

// DiscoverMode reads the server.mode file to determine how to connect.
// Returns ModeLocal if the file is missing or unreadable. Recognizes
// "mtls" and "tls" as new v1.1 mode values alongside the legacy "secure".
func DiscoverMode(stateDir string) ServerMode {
	if stateDir == "" {
		stateDir = StateDir()
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "server.mode"))
	if err != nil {
		return ModeLocal
	}
	mode := ServerMode(strings.TrimSpace(string(data)))
	switch mode {
	case ModeSecure, ModeMTLS:
		return ModeSecure
	case ModeTLS:
		return ModeTLS
	case ModeLocal:
		return ModeLocal
	default:
		return ModeLocal
	}
}

// DiscoverServerName reads the TLS server name recorded by secure startup.
// Older state directories do not have this file, so auto-PKI's historical
// "server" name remains the fallback.
func DiscoverServerName(stateDir string) string {
	if stateDir == "" {
		stateDir = StateDir()
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "server.name"))
	if err != nil {
		return "server"
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "server"
	}
	return name
}

func serverNameFromCert(certPath string) string {
	cert, err := pki.LoadCert(certPath)
	if err != nil {
		return "server"
	}
	for _, name := range cert.DNSNames {
		if strings.TrimSpace(name) != "" && !strings.HasPrefix(name, "*.") {
			return name
		}
	}
	for _, ip := range cert.IPAddresses {
		if ip != nil {
			return ip.String()
		}
	}
	return "server"
}

// Config controls local server behaviour.
type Config struct {
	// StateDir overrides the default ~/.config/bridgectl directory.
	StateDir string
	// Logger overrides the default logger. Nil uses a default logger at
	// Warn level; set Verbose to lower it to Info.
	Logger *slog.Logger
	// Verbose enables Info-level logging (session lifecycle events).
	// Ignored when Logger is explicitly provided.
	Verbose bool
	// AllowedPaths restricts which repo paths sessions may use.
	// Empty means allow all.
	AllowedPaths []string

	// SecurityMode overrides the security mode determined from ListenAddr.
	// When set to ModeTLS, the server uses server-only TLS + JWT.
	// When set to ModeMTLS or ModeSecure, the server uses mTLS + JWT.
	// When empty, the mode is determined from ListenAddr: non-empty
	// ListenAddr → ModeSecure (mTLS), empty → ModeLocal.
	SecurityMode ServerMode

	// ListenAddr, when set, enables secure mode: the server binds to this
	// TCP address with mTLS + JWT instead of a unix socket. Example:
	// "10.0.0.1:9445" or "0.0.0.0:9445".
	ListenAddr string
	// ServerSANs are additional DNS names or IP addresses for the server
	// certificate. The host from ListenAddr is added automatically.
	ServerSANs []string

	// ConfigPath is an optional path to a YAML config file (same schema as
	// the former daemon bridge.yaml). Values from the file are merged into
	// Config; explicit fields in Config take precedence over file values.
	ConfigPath string

	// DBPath enables BoltDB session persistence. When set, the supervisor
	// writes session metadata and PTY chunks to the file and rehydrates
	// them on startup via LoadHistory.
	DBPath string

	// ProviderFallbacks maps each provider ID to an ordered list of
	// fallback provider IDs to try when the primary is unavailable.
	// Fallbacks are only used when ProviderFallbacksEnabled is true or
	// when the config file sets feature_flags.provider_fallbacks: true.
	ProviderFallbacks map[string][]string

	// ProviderFallbacksEnabled gates whether provider fallback logic is
	// active. When false (the default), fallback lists are ignored even if
	// configured. Set by feature_flags.provider_fallbacks in the config
	// file or programmatically for tests.
	ProviderFallbacksEnabled bool

	// RedactPatterns are compiled into a Redactor that scrubs sensitive
	// values from log output.
	RedactPatterns []string

	// RepoSetup controls repo-local pre-agent setup files. Nil means enabled
	// with built-in defaults.
	RepoSetupEnabled        *bool
	RepoSetupConfigPath     string
	RepoSetupDefaultTimeout time.Duration
	RepoSetupMaxTimeout     time.Duration

	// RateLimits overrides the default rate-limit config. Zero values keep
	// the built-in defaults.
	RateLimits server.RateLimitConfig

	// EventBufferSize overrides the per-session output ring-buffer size in
	// bytes. Zero uses the default (8 MiB).
	EventBufferSize int

	// IdleTimeout overrides the session idle-timeout. Zero uses the
	// default (30 minutes).
	IdleTimeout time.Duration

	// Explicit TLS cert paths. When set, these override auto-PKI generation
	// so pre-issued certificates (e.g. from a CI/CD pipeline) can be used.
	// All three (CABundlePath, TLSCertPath, TLSKeyPath) must be provided
	// together; mixed partial configuration is not supported.
	CABundlePath string
	TLSCertPath  string
	TLSKeyPath   string

	// JWTPublicKeys maps issuer name to public key file path for JWT
	// verification in explicit-cert mode. Populated from auth.jwt_public_keys
	// in the config file.
	JWTPublicKeys map[string]string
	// StepCAClients declares Step CA-authenticated clients whose JWT public
	// keys should be loaded at startup if present. Missing optional clients are
	// logged and skipped; missing required clients fail secure startup.
	StepCAClients []ConfiguredJWTClient

	// StepCAURL enables Step CA integration. When set, auto-PKI generation is
	// skipped and server certificates are obtained from the Step CA instance
	// instead. The `step` CLI must be on PATH.
	StepCAURL string
	// StepCARootPath is the path to the Step CA root certificate. Required
	// when StepCAURL is set.
	StepCARootPath string
	// StepCAOIDCProvider is the OIDC issuer URL configured as a Step CA
	// provisioner. Used by `bridgectl server issue-client --oidc-provider`.
	StepCAOIDCProvider string
	// StepCAProvisioner is the name of the Step CA provisioner to use
	// (e.g. "acme", "bridge-jwk"). When empty, the step CLI selects the
	// default provisioner.
	StepCAProvisioner string
	// StepCAProvisionerPasswordFile is the path to a file containing the
	// JWK provisioner password. When set, `step ca certificate` runs
	// non-interactively (required in Docker/headless environments).
	StepCAProvisionerPasswordFile string

	// securityConfig is the resolved v1.1 SecurityConfig. It is set
	// internally during config merging when the YAML file contains a
	// security: block. It is unexported because callers use the
	// individual fields above; this field exists to construct a
	// CertificateProvider when the new config model is active.
	securityConfig *config.SecurityConfig

	// CertValidity overrides the server certificate validity duration.
	// Zero uses the default (90 days). Useful for testing renewal flows.
	CertValidity time.Duration
	// CertRenewalCheckInterval overrides how often the renewal loop checks
	// certificate expiry. Zero uses the default (1 hour).
	CertRenewalCheckInterval time.Duration
}

// Start launches a local bridge gRPC server. In local mode (default) it
// listens on a unix socket (or TCP localhost on Windows) without auth.
// In secure mode (ListenAddr set) it binds to TCP with mTLS + JWT.
func Start(cfg Config) (*Server, error) {
	// Merge YAML config file values into cfg. Explicit fields in cfg take
	// precedence: we only apply file values when the cfg field is still at
	// its zero value.
	var configProviderDefs map[string]config.ProviderConfig
	var providerRoot string
	providerFallbacksEnabled := cfg.ProviderFallbacksEnabled
	repoSetupEnabled := true
	repoSetupConfigPath := ".bridgectl.yaml"
	repoSetupDefaultTimeout := 2 * time.Minute
	repoSetupMaxTimeout := 15 * time.Minute
	configHasServerListen := false
	if cfg.ConfigPath != "" {
		var err error
		configHasServerListen, err = config.HasExplicitServerListen(cfg.ConfigPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect config %q: %w", cfg.ConfigPath, err)
		}
		fileCfg, err := config.Load(cfg.ConfigPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load config %q: %w", cfg.ConfigPath, err)
		}
		if fileCfg != nil {
			if len(fileCfg.Providers) > 0 {
				configProviderDefs = fileCfg.Providers
			}
			providerRoot = fileCfg.Runtime.ProviderRoot
			providerFallbacksEnabled = providerFallbacksEnabled || fileCfg.FeatureFlags.ProviderFallbacks
			repoSetupEnabled = fileCfg.RepoSetup.IsEnabled()
			repoSetupConfigPath = fileCfg.RepoSetup.ConfigPath
			repoSetupDefaultTimeout = config.ParseDuration(fileCfg.RepoSetup.DefaultTimeout, repoSetupDefaultTimeout)
			repoSetupMaxTimeout = config.ParseDuration(fileCfg.RepoSetup.MaxTimeout, repoSetupMaxTimeout)
			if cfg.DBPath == "" && fileCfg.Persistence.DBPath != "" {
				cfg.DBPath = fileCfg.Persistence.DBPath
			}
			if cfg.RedactPatterns == nil && len(fileCfg.Logging.RedactPatterns) > 0 {
				cfg.RedactPatterns = fileCfg.Logging.RedactPatterns
			}
			if cfg.RateLimits.GlobalRPS == 0 && fileCfg.RateLimits.GlobalRPS > 0 {
				cfg.RateLimits.GlobalRPS = fileCfg.RateLimits.GlobalRPS
			}
			if cfg.RateLimits.GlobalBurst == 0 && fileCfg.RateLimits.GlobalBurst > 0 {
				cfg.RateLimits.GlobalBurst = fileCfg.RateLimits.GlobalBurst
			}
			if cfg.RateLimits.StartSessionPerClientRPS == 0 && fileCfg.RateLimits.StartSessionPerClientRPS > 0 {
				cfg.RateLimits.StartSessionPerClientRPS = fileCfg.RateLimits.StartSessionPerClientRPS
			}
			if cfg.RateLimits.StartSessionPerClientBurst == 0 && fileCfg.RateLimits.StartSessionPerClientBurst > 0 {
				cfg.RateLimits.StartSessionPerClientBurst = fileCfg.RateLimits.StartSessionPerClientBurst
			}
			if cfg.RateLimits.SendInputPerSessionRPS == 0 && fileCfg.RateLimits.SendInputPerSessionRPS > 0 {
				cfg.RateLimits.SendInputPerSessionRPS = fileCfg.RateLimits.SendInputPerSessionRPS
			}
			if cfg.RateLimits.SendInputPerSessionBurst == 0 && fileCfg.RateLimits.SendInputPerSessionBurst > 0 {
				cfg.RateLimits.SendInputPerSessionBurst = fileCfg.RateLimits.SendInputPerSessionBurst
			}
			if cfg.EventBufferSize == 0 && fileCfg.Sessions.EventBufferSize > 0 {
				cfg.EventBufferSize = fileCfg.Sessions.EventBufferSize
			}
			if cfg.IdleTimeout == 0 && fileCfg.Sessions.IdleTimeout != "" {
				cfg.IdleTimeout = config.ParseDuration(fileCfg.Sessions.IdleTimeout, 0)
			}
			if cfg.AllowedPaths == nil && len(fileCfg.AllowedPaths) > 0 {
				cfg.AllowedPaths = fileCfg.AllowedPaths
			}
			if cfg.ListenAddr == "" && configHasServerListen && fileCfg.Server.Listen != "" {
				cfg.ListenAddr = fileCfg.Server.Listen
			}
			if cfg.CABundlePath == "" && fileCfg.TLS.CABundle != "" {
				cfg.CABundlePath = fileCfg.TLS.CABundle
				cfg.TLSCertPath = fileCfg.TLS.Cert
				cfg.TLSKeyPath = fileCfg.TLS.Key
			}
			if cfg.JWTPublicKeys == nil && len(fileCfg.Auth.JWTPublicKeys) > 0 {
				cfg.JWTPublicKeys = make(map[string]string, len(fileCfg.Auth.JWTPublicKeys))
				for _, k := range fileCfg.Auth.JWTPublicKeys {
					cfg.JWTPublicKeys[k.Issuer] = k.KeyPath
				}
			}
			if cfg.StepCAClients == nil && len(fileCfg.StepCA.Clients) > 0 {
				cfg.StepCAClients = make([]ConfiguredJWTClient, 0, len(fileCfg.StepCA.Clients))
				for _, c := range fileCfg.StepCA.Clients {
					cfg.StepCAClients = append(cfg.StepCAClients, ConfiguredJWTClient{
						Issuer:   c.Issuer,
						KeyPath:  c.KeyPath,
						Required: c.Required,
					})
				}
			}
			if len(cfg.ServerSANs) == 0 && len(fileCfg.Server.SANs) > 0 {
				cfg.ServerSANs = fileCfg.Server.SANs
			}
			if cfg.CertValidity == 0 && fileCfg.Server.CertValidity != "" {
				cfg.CertValidity = config.ParseDuration(fileCfg.Server.CertValidity, 0)
			}
			if cfg.CertRenewalCheckInterval == 0 && fileCfg.Server.CertRenewalCheckInterval != "" {
				cfg.CertRenewalCheckInterval = config.ParseDuration(fileCfg.Server.CertRenewalCheckInterval, 0)
			}
			if cfg.StepCAURL == "" && fileCfg.StepCA.URL != "" {
				cfg.StepCAURL = strings.TrimRight(fileCfg.StepCA.URL, "/")
			}
			if cfg.StepCARootPath == "" && fileCfg.StepCA.Root != "" {
				cfg.StepCARootPath = fileCfg.StepCA.Root
			}
			if cfg.StepCAProvisioner == "" && fileCfg.StepCA.Provisioner != "" {
				cfg.StepCAProvisioner = fileCfg.StepCA.Provisioner
			}
			if cfg.StepCAProvisionerPasswordFile == "" && fileCfg.StepCA.ProvisionerPasswordFile != "" {
				cfg.StepCAProvisionerPasswordFile = fileCfg.StepCA.ProvisionerPasswordFile
			}
			// Capture the resolved security config for provider construction.
			if fileCfg.Security.IsSecurityConfigured() {
				cfg.securityConfig = &fileCfg.Security
			}
		}
	}

	// Apply built-in defaults for any fields still at zero.
	if cfg.RateLimits.GlobalRPS == 0 {
		cfg.RateLimits.GlobalRPS = 100
	}
	if cfg.RateLimits.GlobalBurst == 0 {
		cfg.RateLimits.GlobalBurst = 200
	}
	if cfg.RateLimits.StartSessionPerClientRPS == 0 {
		cfg.RateLimits.StartSessionPerClientRPS = 5
	}
	if cfg.RateLimits.StartSessionPerClientBurst == 0 {
		cfg.RateLimits.StartSessionPerClientBurst = 10
	}
	if cfg.RateLimits.SendInputPerSessionRPS == 0 {
		cfg.RateLimits.SendInputPerSessionRPS = 50
	}
	if cfg.RateLimits.SendInputPerSessionBurst == 0 {
		cfg.RateLimits.SendInputPerSessionBurst = 100
	}
	if cfg.EventBufferSize <= 0 {
		cfg.EventBufferSize = 8 << 20
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if cfg.RepoSetupEnabled != nil {
		repoSetupEnabled = *cfg.RepoSetupEnabled
	}
	if cfg.RepoSetupConfigPath != "" {
		repoSetupConfigPath = cfg.RepoSetupConfigPath
	}
	if cfg.RepoSetupDefaultTimeout > 0 {
		repoSetupDefaultTimeout = cfg.RepoSetupDefaultTimeout
	}
	if cfg.RepoSetupMaxTimeout > 0 {
		repoSetupMaxTimeout = cfg.RepoSetupMaxTimeout
	}

	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = StateDir()
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %q: %w", stateDir, err)
	}

	logger := cfg.Logger
	if logger == nil {
		level := slog.LevelWarn
		if cfg.Verbose {
			level = slog.LevelInfo
		}
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}

	var logRedactor *redact.Redactor
	// Apply log redaction when patterns are configured.
	if len(cfg.RedactPatterns) > 0 {
		redactor, err := redact.New(cfg.RedactPatterns)
		if err != nil {
			return nil, fmt.Errorf("compile redact patterns: %w", err)
		}
		logRedactor = redactor
		logger = slog.New(&redactingHandler{inner: logger.Handler(), redactor: redactor})
	}

	// Install as the default so internal packages that call slog.Warn etc.
	// (e.g. supervisor's slow-observer warning) use the same configured logger.
	slog.SetDefault(logger)

	// Build provider registry. Config-file providers take precedence; the
	// auto-detect path fills in any providers not explicitly configured.
	registry := bridge.NewRegistry()
	var redactFunc func(string) string
	if logRedactor != nil {
		redactFunc = logRedactor.Redact
	}
	repoSetupRunner := reposetup.NewCoordinator(reposetup.Options{
		Enabled:        repoSetupEnabled,
		ConfigPath:     repoSetupConfigPath,
		DefaultTimeout: repoSetupDefaultTimeout,
		MaxTimeout:     repoSetupMaxTimeout,
		Logger:         logger,
		Redact:         redactFunc,
		BaseEnv: func() []string {
			return provider.FilterEnv(os.Environ())
		},
	})

	// Register providers explicitly declared in the config file.
	for id, pc := range configProviderDefs {
		timeout := config.ParseDuration(pc.StartupTimeout, 60*time.Second)
		sc := provider.StdioConfig{
			ProviderID:     id,
			Binary:         pc.Binary,
			DefaultArgs:    pc.Args,
			StartupTimeout: timeout,
			StopGrace:      10 * time.Second,
			StartupProbe:   pc.StartupProbe,
			PromptPattern:  pc.PromptPattern,
			RequiredEnv:    pc.RequiredEnv,
			StreamJSON:     pc.StreamJSON,
			StripANSI:      pc.StripANSI,
			ProviderRoot:   providerRoot,
		}
		var p bridge.Provider
		switch {
		case pc.Transport == "opencode_server":
			osCfg := provider.OpenCodeServerConfig{
				ProviderID:     id,
				Binary:         pc.Binary,
				DefaultArgs:    pc.Args,
				StartupTimeout: timeout,
				StopGrace:      10 * time.Second,
				RequiredEnv:    pc.RequiredEnv,
				Hostname:       pc.Hostname,
				ProviderRoot:   providerRoot,
			}
			if pc.PortRange != "" {
				start, end, parseErr := config.ParsePortRange(pc.PortRange)
				if parseErr != nil {
					logger.Warn("skip config provider: invalid port_range", "provider", id, "error", parseErr)
					continue
				}
				osCfg.PortRangeStart = start
				osCfg.PortRangeEnd = end
			}
			p = provider.NewOpenCodeServerProvider(osCfg)
		case id == "codex":
			p = provider.NewCodexProvider(sc)
		default:
			p = provider.NewStdioProvider(sc)
		}
		if err := registry.Register(p); err != nil {
			logger.Warn("skip config provider", "provider", id, "error", err)
			continue
		}
		logger.Info("registered config provider", "provider", id, "binary", pc.Binary, "transport", pc.Transport)
	}

	// Build fallbacks map from config providers (merged with any set on cfg)
	// only when the provider_fallbacks feature flag is enabled.
	if providerFallbacksEnabled {
		logger.Info("provider fallbacks enabled")
		if cfg.ProviderFallbacks == nil && len(configProviderDefs) > 0 {
			cfg.ProviderFallbacks = make(map[string][]string)
		}
		for id, pc := range configProviderDefs {
			if len(pc.Fallbacks) > 0 {
				if _, already := cfg.ProviderFallbacks[id]; !already {
					cfg.ProviderFallbacks[id] = pc.Fallbacks
					logger.Info("registered provider fallbacks", "provider", id, "fallbacks", pc.Fallbacks)
				}
			}
		}
	} else {
		// Feature flag disabled: clear any fallbacks so the server does not
		// attempt provider failover.
		cfg.ProviderFallbacks = nil
	}

	// Auto-detect additional providers not already registered via config.
	for _, pd := range detectProviders() {
		if _, err := registry.Get(pd.ID); err == nil {
			continue // already registered from config
		}
		sc := provider.StdioConfig{
			ProviderID:     pd.ID,
			Binary:         pd.Binary,
			DefaultArgs:    pd.Args,
			StartupTimeout: pd.StartupTimeout,
			StopGrace:      10 * time.Second,
			StartupProbe:   pd.StartupProbe,
			PromptPattern:  pd.PromptPattern,
			RequiredEnv:    pd.RequiredEnv,
			StreamJSON:     pd.StreamJSON,
		}
		var p bridge.Provider
		if pd.ID == "codex" {
			p = provider.NewCodexProvider(sc)
		} else {
			p = provider.NewStdioProvider(sc)
		}
		if err := registry.Register(p); err != nil {
			logger.Warn("skip provider", "provider", pd.ID, "error", err)
			continue
		}
		logger.Info("registered provider", "provider", pd.ID, "binary", pd.Binary)
	}

	// Always register the echo provider for testing.
	echoProv := provider.NewStdioProvider(provider.StdioConfig{
		ProviderID:     "echo",
		Binary:         "cat",
		StartupTimeout: 5 * time.Second,
		StopGrace:      2 * time.Second,
		StartupProbe:   "none",
	})
	if err := registry.Register(echoProv); err != nil {
		logger.Debug("echo provider already registered", "error", err)
	}

	// Policy
	policy := bridge.Policy{
		MaxPerProject: 10,
		MaxGlobal:     20,
		MaxInputBytes: 65536,
		AllowedPaths:  cfg.AllowedPaths,
	}

	// Supervisor options: persistence store when DBPath is set.
	var supOpts []bridge.SupervisorOption
	supOpts = append(supOpts, bridge.WithRepoSetupRunner(repoSetupRunner))
	var store bridge.SessionStore
	if cfg.DBPath != "" {
		var err error
		store, err = bridge.NewBoltSessionStore(cfg.DBPath)
		if err != nil {
			return nil, fmt.Errorf("open session store %q: %w", cfg.DBPath, err)
		}
		supOpts = append(supOpts, bridge.WithStore(store))
	}

	sup := bridge.NewSupervisor(registry, policy, cfg.EventBufferSize, cfg.IdleTimeout, supOpts...)
	if store != nil {
		if err := sup.LoadHistory(); err != nil {
			logger.Warn("failed to load session history", "error", err)
		}
	}

	// Server instance ID
	instanceID := generateInstanceID()

	// Determine server mode and build gRPC options accordingly.
	mode := ModeLocal
	var grpcOpts []grpc.ServerOption
	var jwtVerifier *auth.JWTVerifier

	// Renewal-related state; populated when secure mode is using managed PKI.
	var serverSANs []string
	var stepCAConfig *StepCAConfig
	var pkiMat *PKIMaterial
	explicitCerts := false
	renewExplicitCerts := false
	tlsServerName := "server"

	// Determine the effective security mode. SecurityMode takes
	// precedence; otherwise fall back to the legacy ListenAddr heuristic.
	switch cfg.SecurityMode {
	case ModeSecure, ModeMTLS, ModeTLS:
		mode = cfg.SecurityMode
	case ModeLocal:
		mode = ModeLocal
		// Explicit local mode: do not promote to secure even if ListenAddr is set.
	case "":
		// Legacy: determine from ListenAddr.
		if cfg.ListenAddr != "" {
			mode = ModeSecure
		}
	default:
		sup.Close()
		if store != nil {
			_ = store.Close()
		}
		return nil, fmt.Errorf("unknown security mode %q (valid: local, tls, mtls)", cfg.SecurityMode)
	}

	if IsSecureMode(mode) {
		// Secure mode: TCP + TLS or mTLS + JWT.
		// TODO(windows): Secure mode (mTLS+JWT) is not yet supported on Windows.
		// Windows support requires named-pipe ACLs or equivalent transport security.
		if runtime.GOOS == "windows" {
			sup.Close()
			if store != nil {
				_ = store.Close()
			}
			return nil, fmt.Errorf("secure mode (--listen) is not yet supported on Windows")
		}

		var mat *PKIMaterial

		serverSANs = BuildServerSANs(cfg.ListenAddr, cfg.ServerSANs)
		if cfg.StepCAURL != "" {
			stepCAConfig = &StepCAConfig{
				URL:                     cfg.StepCAURL,
				RootPath:                cfg.StepCARootPath,
				OIDCProviderURL:         cfg.StepCAOIDCProvider,
				Provisioner:             cfg.StepCAProvisioner,
				ProvisionerPasswordFile: cfg.StepCAProvisionerPasswordFile,
			}
		}

		if cfg.CABundlePath != "" {
			// Use pre-issued certificates from Config (e.g. provided via config file).
			explicitCerts = true
			if cfg.TLSCertPath == "" || cfg.TLSKeyPath == "" {
				sup.Close()
				if store != nil {
					_ = store.Close()
				}
				return nil, fmt.Errorf("CABundlePath requires TLSCertPath and TLSKeyPath to also be set")
			}
			var pkiErr error
			mat, pkiErr = EnsureLocalManagementPKI(stateDir, cfg.CABundlePath, logger)
			if pkiErr != nil {
				sup.Close()
				if store != nil {
					_ = store.Close()
				}
				return nil, fmt.Errorf("ensure local management PKI: %w", pkiErr)
			}
			mat.ServerCertPath = cfg.TLSCertPath
			mat.ServerKeyPath = cfg.TLSKeyPath
			if stepCAConfig != nil {
				renewExplicitCerts = true
			}
			if err := ensureExplicitServerCertFresh(mat, serverSANs, logger, stepCAConfig); err != nil {
				sup.Close()
				if store != nil {
					_ = store.Close()
				}
				return nil, err
			}
			tlsServerName = serverNameFromCert(cfg.TLSCertPath)
		} else {
			// Auto-generate PKI material if not present, or delegate to Step CA.
			var pkiErr error
			mat, pkiErr = EnsurePKI(stateDir, serverSANs, logger, stepCAConfig, cfg.CertValidity)
			if pkiErr != nil {
				sup.Close()
				if store != nil {
					_ = store.Close()
				}
				return nil, fmt.Errorf("ensure PKI: %w", pkiErr)
			}
			// Derive the TLS server name from the actual certificate SANs.
			// Step CA renewal or ACME provisioners may drop bare-hostname
			// SANs like "server", so we must use whatever the cert contains.
			tlsServerName = serverNameFromCert(mat.ServerCertPath)
		}
		pkiMat = mat

		var secureOpts []grpc.ServerOption
		var verifier *auth.JWTVerifier
		var buildErr error
		if mode == ModeTLS {
			secureOpts, verifier, buildErr = buildTLSOnlyGRPCOpts(mat, stateDir, logger, cfg.JWTPublicKeys, cfg.StepCAClients)
		} else {
			secureOpts, verifier, buildErr = buildSecureGRPCOpts(mat, stateDir, logger, cfg.JWTPublicKeys, cfg.StepCAClients)
		}
		if buildErr != nil {
			sup.Close()
			if store != nil {
				_ = store.Close()
			}
			return nil, fmt.Errorf("build gRPC options (%s): %w", mode, buildErr)
		}
		grpcOpts = secureOpts
		jwtVerifier = verifier
	} else {
		// Local mode: unix socket, anonymous passthrough auth.
		grpcOpts = []grpc.ServerOption{
			grpc.ChainUnaryInterceptor(auth.UnaryPassthroughInterceptor()),
			grpc.ChainStreamInterceptor(auth.StreamPassthroughInterceptor()),
		}
	}

	grpcServer := grpc.NewServer(grpcOpts...)

	providerFallbacks := cfg.ProviderFallbacks

	bridgeServer := server.New(sup, registry, logger, cfg.RateLimits, instanceID, providerFallbacks, jwtVerifier, CertsDir(stateDir))
	bridgev1.RegisterBridgeServiceServer(grpcServer, bridgeServer)

	// Listen: TCP for secure mode, unix socket for local mode.
	var ln net.Listener
	var listenAddr string
	var err error
	if IsSecureMode(mode) {
		ln, err = net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			sup.Close()
			return nil, fmt.Errorf("listen tcp %s: %w", cfg.ListenAddr, err)
		}
		listenAddr = localDialAddr(ln.Addr().String())
	} else {
		ln, listenAddr, err = listen(stateDir)
		if err != nil {
			sup.Close()
			return nil, fmt.Errorf("listen: %w", err)
		}
	}

	// Write PID file.
	pidFile := filepath.Join(stateDir, "server.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		_ = ln.Close()
		sup.Close()
		return nil, fmt.Errorf("write pid file: %w", err)
	}

	// Write address file.
	addrFile := filepath.Join(stateDir, "server.addr")
	if err := os.WriteFile(addrFile, []byte(listenAddr), 0o644); err != nil {
		_ = ln.Close()
		sup.Close()
		return nil, fmt.Errorf("write addr file: %w", err)
	}

	// Write mode file so discovery knows how to connect.
	modeFile := filepath.Join(stateDir, "server.mode")
	if err := os.WriteFile(modeFile, []byte(string(mode)), 0o644); err != nil {
		_ = ln.Close()
		sup.Close()
		return nil, fmt.Errorf("write mode file: %w", err)
	}
	if IsSecureMode(mode) {
		nameFile := filepath.Join(stateDir, "server.name")
		if err := os.WriteFile(nameFile, []byte(tlsServerName), 0o644); err != nil {
			_ = ln.Close()
			sup.Close()
			return nil, fmt.Errorf("write server name file: %w", err)
		}
	}

	logger.Info("server starting", "mode", mode, "addr", listenAddr, "pid", os.Getpid())

	s := &Server{
		grpcServer:        grpcServer,
		supervisor:        sup,
		store:             store,
		registry:          registry,
		listener:          ln,
		logger:            logger,
		stateDir:          stateDir,
		providerFallbacks: providerFallbacks,
	}

	// Construct a CertificateProvider from the security config when the
	// new config model is explicitly used. This is stored for future use
	// by the enrollment system and identity commands.
	if cfg.securityConfig != nil && cfg.securityConfig.IsSecurityConfigured() {
		cp, cpErr := CertProviderFromConfig(*cfg.securityConfig, stateDir, logger)
		if cpErr != nil {
			logger.Warn("failed to construct certificate provider from security config", "error", cpErr)
		} else {
			s.certProvider = cp
		}
	}

	// Start background certificate renewal for managed PKI. Explicit certs are
	// normally externally managed, but when Step CA settings are also present
	// we can safely renew the configured cert/key paths in-place.
	if IsSecureMode(mode) && (!explicitCerts || renewExplicitCerts) {
		s.serverSANs = serverSANs
		s.stepCA = stepCAConfig
		s.pkiMat = pkiMat
		s.certValidity = cfg.CertValidity
		s.certRenewalCheckInterval = cfg.CertRenewalCheckInterval
		renewCtx, renewCancel := context.WithCancel(context.Background())
		s.renewCancel = renewCancel
		go s.certRenewalLoop(renewCtx)
	}

	go func() {
		if err := grpcServer.Serve(ln); err != nil {
			logger.Error("grpc serve", "error", err)
		}
	}()

	return s, nil
}

// buildSecureGRPCOpts returns gRPC server options for mTLS + JWT mode.
// extraKeys maps issuer name to public key file path for JWT verification
// when using pre-issued certificates instead of auto-PKI.
func buildSecureGRPCOpts(mat *PKIMaterial, stateDir string, logger *slog.Logger, extraKeys map[string]string, stepCAClients []ConfiguredJWTClient) ([]grpc.ServerOption, *auth.JWTVerifier, error) {
	// TLS credentials with client cert verification.
	tlsCfg, err := auth.ServerTLSConfig(auth.TLSConfig{
		CABundlePath: mat.CABundlePath,
		CertPath:     mat.ServerCertPath,
		KeyPath:      mat.ServerKeyPath,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("server TLS config: %w", err)
	}

	// JWT verifier: load the local key plus any per-client keys.
	keys := make(map[string]ed25519.PublicKey)

	if len(extraKeys) > 0 {
		// Load explicit issuer→key mappings from config (explicit cert mode).
		for issuer, keyPath := range extraKeys {
			pub, keyErr := pki.LoadEd25519PublicKey(keyPath)
			if keyErr != nil {
				return nil, nil, fmt.Errorf("load JWT public key for issuer %q: %w", issuer, keyErr)
			}
			keys[issuer] = pub
		}
	} else if mat.JWTSigningPub != "" {
		// Auto-PKI mode: load the locally generated key as the "local" verifier.
		localPub, keyErr := pki.LoadEd25519PublicKey(mat.JWTSigningPub)
		if keyErr != nil {
			return nil, nil, fmt.Errorf("load JWT public key: %w", keyErr)
		}
		keys["local"] = localPub
	}

	// Load per-client JWT public keys from certs/jwt-clients/*.pub.
	clientKeysDir := filepath.Join(CertsDir(stateDir), "jwt-clients")
	entries, _ := os.ReadDir(clientKeysDir)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".pub" {
			continue
		}
		issuer := strings.TrimSuffix(e.Name(), ".pub")
		pub, err := pki.LoadEd25519PublicKey(filepath.Join(clientKeysDir, e.Name()))
		if err != nil {
			logger.Warn("skip client JWT key", "file", e.Name(), "error", err)
			continue
		}
		keys[issuer] = pub
		logger.Info("loaded client JWT key", "issuer", issuer)
	}

	verifier := &auth.JWTVerifier{
		Keys:     keys,
		Audience: "bridge",
		MaxTTL:   10 * time.Minute,
	}
	if err := loadConfiguredJWTClients(verifier, stateDir, logger, stepCAClients); err != nil {
		return nil, nil, err
	}

	return []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(
			auth.UnaryJWTInterceptor(verifier, logger),
			auth.UnaryAuditInterceptor(logger),
		),
		grpc.ChainStreamInterceptor(
			auth.StreamJWTInterceptor(verifier, logger),
			auth.StreamAuditInterceptor(logger),
		),
	}, verifier, nil
}

// buildTLSOnlyGRPCOpts creates gRPC server options for TLS-only mode
// (server certificate presented, no client certificate required).
// JWT is still used for authorization.
func buildTLSOnlyGRPCOpts(mat *PKIMaterial, stateDir string, logger *slog.Logger, extraKeys map[string]string, stepCAClients []ConfiguredJWTClient) ([]grpc.ServerOption, *auth.JWTVerifier, error) {
	// TLS credentials without client cert verification.
	tlsCfg, err := auth.ServerTLSOnlyConfig(auth.TLSConfig{
		CABundlePath: mat.CABundlePath,
		CertPath:     mat.ServerCertPath,
		KeyPath:      mat.ServerKeyPath,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("server TLS-only config: %w", err)
	}

	// JWT verifier: same key loading logic as mTLS mode.
	keys := make(map[string]ed25519.PublicKey)

	if len(extraKeys) > 0 {
		for issuer, keyPath := range extraKeys {
			pub, keyErr := pki.LoadEd25519PublicKey(keyPath)
			if keyErr != nil {
				return nil, nil, fmt.Errorf("load JWT public key for issuer %q: %w", issuer, keyErr)
			}
			keys[issuer] = pub
		}
	} else if mat.JWTSigningPub != "" {
		localPub, keyErr := pki.LoadEd25519PublicKey(mat.JWTSigningPub)
		if keyErr != nil {
			return nil, nil, fmt.Errorf("load JWT public key: %w", keyErr)
		}
		keys["local"] = localPub
	}

	clientKeysDir := filepath.Join(CertsDir(stateDir), "jwt-clients")
	entries, _ := os.ReadDir(clientKeysDir)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".pub" {
			continue
		}
		issuer := strings.TrimSuffix(e.Name(), ".pub")
		pub, loadErr := pki.LoadEd25519PublicKey(filepath.Join(clientKeysDir, e.Name()))
		if loadErr != nil {
			logger.Warn("skip client JWT key", "file", e.Name(), "error", loadErr)
			continue
		}
		keys[issuer] = pub
		logger.Info("loaded client JWT key", "issuer", issuer)
	}

	verifier := &auth.JWTVerifier{
		Keys:     keys,
		Audience: "bridge",
		MaxTTL:   10 * time.Minute,
	}
	if err := loadConfiguredJWTClients(verifier, stateDir, logger, stepCAClients); err != nil {
		return nil, nil, err
	}

	return []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(
			auth.UnaryJWTInterceptor(verifier, logger),
			auth.UnaryAuditInterceptor(logger),
		),
		grpc.ChainStreamInterceptor(
			auth.StreamJWTInterceptor(verifier, logger),
			auth.StreamAuditInterceptor(logger),
		),
	}, verifier, nil
}

// BuildServerSANs extracts the host from listenAddr and merges it with
// any additional SANs. Deduplicates entries.
func BuildServerSANs(listenAddr string, extra []string) []string {
	seen := make(map[string]bool)
	var sans []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			sans = append(sans, s)
		}
	}

	// Extract host from listenAddr (e.g. "10.0.0.1:9445" → "10.0.0.1").
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// Might be just a host without port.
		host = listenAddr
	}
	// Don't add wildcard addresses as SANs.
	if host != "" && host != "0.0.0.0" && host != "::" {
		add(host)
	}
	// Always include localhost and the CN "server" so TLS ServerName
	// verification works (Go ignores CN when SANs are present).
	add("server")
	add("127.0.0.1")
	add("localhost")

	for _, s := range extra {
		add(s)
	}
	return sans
}

func localDialAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "0.0.0.0", "":
		return net.JoinHostPort("127.0.0.1", port)
	case "::":
		return net.JoinHostPort("::1", port)
	default:
		return addr
	}
}

// Addr returns the listener address (unix socket path or TCP address).
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Target returns the gRPC dial target for this server.
func (s *Server) Target() string {
	addr := s.listener.Addr()
	if addr.Network() == "unix" {
		return "unix://" + addr.String()
	}
	return addr.String()
}

// CertProvider returns the v1.1 CertificateProvider, if one was constructed
// during startup. Returns nil when the server is running with legacy config
// or in local mode.
func (s *Server) CertProvider() certprovider.CertificateProvider {
	return s.certProvider
}

// Stop gracefully shuts down the server and cleans up state files.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true

	s.logger.Info("stopping local server")

	// Cancel the background cert renewal goroutine if running.
	if s.renewCancel != nil {
		s.renewCancel()
	}

	// Stop accepting new RPCs, then gracefully drain provider sessions so
	// long-lived AttachSession streams can finish before the gRPC server exits.
	done := make(chan struct{})
	go func() {
		s.grpcServer.GracefulStop()
		close(done)
	}()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), sessionDrainShutdownTimeout)
	if err := s.supervisor.Shutdown(shutdownCtx); err != nil {
		s.logger.Warn("graceful session shutdown timed out, forced remaining sessions", "error", err)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.logger.Warn("grpc graceful shutdown timed out, forcing stop")
		s.grpcServer.Stop()
	}

	s.supervisor.Close()
	_ = s.listener.Close()
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			s.logger.Warn("close session store", "error", err)
		}
	}

	// Clean up state files.
	_ = os.Remove(filepath.Join(s.stateDir, "server.pid"))
	_ = os.Remove(filepath.Join(s.stateDir, "server.addr"))
	_ = os.Remove(filepath.Join(s.stateDir, "server.mode"))
	_ = os.Remove(filepath.Join(s.stateDir, "server.name"))
	_ = os.Remove(filepath.Join(s.stateDir, "server.sock"))
	_ = os.Remove(filepath.Join(s.stateDir, "server.lock"))
}

func ensureExplicitServerCertFresh(mat *PKIMaterial, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig) error {
	notBefore, notAfter, err := ServerCertExpiry(mat.ServerCertPath)
	if err != nil {
		if stepCA == nil || stepCA.URL == "" {
			return fmt.Errorf("read explicit TLS certificate %q: %w", mat.ServerCertPath, err)
		}
		logger.Warn("explicit TLS certificate unreadable, requesting replacement from Step CA",
			"cert", mat.ServerCertPath,
			"error", err,
		)
		if err := RenewServerCertMaterial(mat, serverSANs, logger, stepCA, 0); err != nil {
			return fmt.Errorf("renew explicit TLS certificate %q: %w", mat.ServerCertPath, err)
		}
		return nil
	}

	lifetime := notAfter.Sub(notBefore)
	remaining := time.Until(notAfter)
	renewThreshold := lifetime / 3

	if remaining > renewThreshold {
		return nil
	}

	if stepCA == nil || stepCA.URL == "" {
		if remaining <= 0 {
			return fmt.Errorf("explicit TLS certificate %q expired at %s; configure step_ca.url and step_ca.provisioner_password_file or replace the certificate",
				mat.ServerCertPath,
				notAfter.Format(time.RFC3339),
			)
		}
		logger.Warn("explicit TLS certificate is approaching expiry but no Step CA renewal config is available",
			"cert", mat.ServerCertPath,
			"expires", notAfter.Format(time.RFC3339),
			"remaining", remaining.Round(time.Second),
		)
		return nil
	}

	if remaining <= 0 {
		logger.Warn("explicit TLS certificate has expired, renewing before startup",
			"cert", mat.ServerCertPath,
			"expired", notAfter.Format(time.RFC3339),
		)
		if err := RenewServerCertMaterial(mat, serverSANs, logger, stepCA, 0); err != nil {
			return fmt.Errorf("renew expired explicit TLS certificate %q: %w", mat.ServerCertPath, err)
		}
		return nil
	}

	logger.Info("explicit TLS certificate approaching expiry, renewing before startup",
		"cert", mat.ServerCertPath,
		"expires", notAfter.Format(time.RFC3339),
		"remaining", remaining.Round(time.Second),
	)
	if err := RenewServerCertMaterial(mat, serverSANs, logger, stepCA, 0); err != nil {
		logger.Warn("explicit TLS certificate renewal failed; continuing with current valid certificate",
			"cert", mat.ServerCertPath,
			"error", err,
		)
	}
	return nil
}

// defaultCertRenewalCheckInterval is how often the renewal loop checks cert expiry.
const defaultCertRenewalCheckInterval = 1 * time.Hour

// certRenewalLoop periodically checks the server certificate's expiry and
// renews it when less than 1/3 of the certificate's total lifetime remains.
// Renewed certs are written to the same file paths; the CertReloader in the
// TLS config picks them up on the next handshake. Existing gRPC connections
// are unaffected — only new TLS handshakes use the renewed cert.
func (s *Server) certRenewalLoop(ctx context.Context) {
	// Check once at startup to handle already-expired certs.
	s.checkAndRenew()

	interval := s.certRenewalCheckInterval
	if interval <= 0 {
		interval = defaultCertRenewalCheckInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkAndRenew()
		}
	}
}

func (s *Server) checkAndRenew() {
	notBefore, notAfter, err := ServerCertExpiry(s.pkiMat.ServerCertPath)
	if err != nil {
		s.logger.Warn("cert renewal: failed to read server cert", "error", err)
		return
	}

	lifetime := notAfter.Sub(notBefore)
	remaining := time.Until(notAfter)
	renewThreshold := lifetime / 3

	if remaining > renewThreshold {
		s.logger.Debug("cert renewal: certificate still valid",
			"expires", notAfter.Format(time.RFC3339),
			"remaining", remaining.Round(time.Minute),
			"renew_at", time.Now().Add(remaining-renewThreshold).Format(time.RFC3339),
		)
		return
	}

	if remaining <= 0 {
		s.logger.Warn("cert renewal: server certificate has EXPIRED, renewing now",
			"expired", notAfter.Format(time.RFC3339),
		)
	} else {
		s.logger.Info("cert renewal: server certificate approaching expiry, renewing",
			"expires", notAfter.Format(time.RFC3339),
			"remaining", remaining.Round(time.Minute),
		)
	}

	if err := RenewServerCertMaterial(s.pkiMat, s.serverSANs, s.logger, s.stepCA, s.certValidity); err != nil {
		s.logger.Error("cert renewal: failed to renew server certificate", "error", err)
		return
	}

	s.logger.Info("cert renewal: server certificate renewed successfully — new connections will use the new cert")
}

// listen creates the appropriate listener for the platform.
// On unix, it acquires an exclusive lockfile before replacing the socket
// to prevent a concurrent start from unlinking an active listener.
func listen(stateDir string) (net.Listener, string, error) {
	if runtime.GOOS == "windows" {
		// Windows: use TCP on localhost with a random port.
		// NOTE: local mode disables TLS/JWT, so any local process that
		// discovers the port can call RPCs. This is a known limitation;
		// consider a named pipe with ACLs for hardened Windows support.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", err
		}
		return ln, ln.Addr().String(), nil
	}

	// Unix socket for macOS and Linux.
	sockPath := filepath.Join(stateDir, "server.sock")
	lockPath := filepath.Join(stateDir, "server.lock")

	// Acquire an exclusive lockfile so concurrent starts don't race on
	// socket removal. The lock is released when the file is closed (on
	// process exit or when Stop removes the socket).
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", fmt.Errorf("open lockfile: %w", err)
	}
	if err := acquireLock(lockFile); err != nil {
		_ = lockFile.Close()
		return nil, "", fmt.Errorf("acquire lock (another server starting?): %w", err)
	}

	// Safe to remove a stale socket now that we hold the lock.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = lockFile.Close()
		return nil, "", err
	}
	// Wrap the listener so it holds a reference to the lockfile. The flock
	// is released when Close() is called (via Server.Stop or process exit).
	return &lockedListener{Listener: ln, lockFile: lockFile}, sockPath, nil
}

// lockedListener wraps a net.Listener and holds a reference to a lockfile.
// Closing the listener also closes the lockfile, releasing the flock.
type lockedListener struct {
	net.Listener
	lockFile *os.File
}

func (l *lockedListener) Close() error {
	listenerErr := l.Listener.Close()
	lockErr := l.lockFile.Close()
	if listenerErr != nil {
		return listenerErr
	}
	return lockErr
}

// IsServerRunning checks if a local server is already running by probing
// the socket/address file.
func IsServerRunning(stateDir string) bool {
	if stateDir == "" {
		stateDir = StateDir()
	}
	target := discoverTarget(stateDir)
	if target == "" {
		return false
	}
	mode := DiscoverMode(stateDir)
	return probeHealth(target, mode, stateDir)
}

// DiscoverTarget returns the gRPC dial target and mode for an existing
// local server. Returns empty target if none is found/reachable.
func DiscoverTarget(stateDir string) (target string, mode ServerMode) {
	if stateDir == "" {
		stateDir = StateDir()
	}
	target = discoverTarget(stateDir)
	if target == "" {
		return "", ModeLocal
	}
	mode = DiscoverMode(stateDir)
	if !probeHealth(target, mode, stateDir) {
		return "", ModeLocal
	}
	return target, mode
}

func discoverTarget(stateDir string) string {
	// Check the mode file first to avoid a stale unix socket from a
	// previous crashed local-mode server masking a running secure server.
	mode := DiscoverMode(stateDir)

	if IsSecureMode(mode) {
		// Secure mode (TLS or mTLS) uses TCP — read the addr file directly.
		addrData, err := os.ReadFile(filepath.Join(stateDir, "server.addr"))
		if err != nil {
			return ""
		}
		addr := strings.TrimSpace(string(addrData))
		if addr != "" {
			return addr
		}
	}

	// Local mode: try unix socket first, but only if the server is healthy.
	sockPath := filepath.Join(stateDir, "server.sock")
	if _, err := os.Stat(sockPath); err == nil {
		target := "unix://" + sockPath
		if probeHealth(target, mode, stateDir) {
			return target
		}
	}
	// Fall back to TCP address file (Windows local mode), probing for health.
	addrData, err := os.ReadFile(filepath.Join(stateDir, "server.addr"))
	if err == nil {
		if addr := strings.TrimSpace(string(addrData)); addr != "" {
			if strings.HasPrefix(addr, "/") {
				addr = "unix://" + addr
			}
			if probeHealth(addr, mode, stateDir) {
				return addr
			}
		}
	}

	return ""
}

func probeHealth(target string, mode ServerMode, stateDir string) bool {
	var dialOpts []grpc.DialOption

	switch {
	case IsMutualTLS(mode):
		mat := LoadPKIMaterial(stateDir)
		tlsCfg, err := auth.ClientTLSConfig(auth.TLSConfig{
			CABundlePath: mat.CABundlePath,
			CertPath:     mat.LocalClientCert,
			KeyPath:      mat.LocalClientKey,
			ServerName:   DiscoverServerName(stateDir),
		})
		if err != nil {
			return false
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	case mode == ModeTLS:
		mat := LoadPKIMaterial(stateDir)
		tlsCfg, err := auth.ClientTLSOnlyConfig(auth.TLSConfig{
			CABundlePath: mat.CABundlePath,
			ServerName:   DiscoverServerName(stateDir),
		})
		if err != nil {
			return false
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	default:
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := bridgev1.NewBridgeServiceClient(conn)
	_, err = client.Health(ctx, &bridgev1.HealthRequest{})
	return err == nil
}

// providerDef describes a provider that can be auto-detected.
type providerDef struct {
	ID             string
	Binary         string
	Args           []string
	StartupTimeout time.Duration
	StartupProbe   string
	PromptPattern  string
	RequiredEnv    []string
	StreamJSON     bool
}

func detectProviders() []providerDef {
	var found []providerDef
	for _, pd := range knownProviders() {
		if _, err := exec.LookPath(pd.Binary); err != nil {
			continue
		}
		found = append(found, pd)
	}
	return found
}

func knownProviders() []providerDef {
	return []providerDef{
		{
			ID:             "claude",
			Binary:         "claude",
			Args:           []string{"--verbose"},
			StartupTimeout: 60 * time.Second,
			StartupProbe:   "prompt",
			PromptPattern:  `(?m)(❯|>\s*$)`,
			// No RequiredEnv: local-server mode relies on native CLI auth
			// (e.g. claude auth login). Env vars are still forwarded to the
			// subprocess if present in the environment.
		},
		{
			ID:             "codex",
			Binary:         "codex",
			Args:           nil,
			StartupTimeout: 60 * time.Second,
			StartupProbe:   "prompt",
			PromptPattern:  `(?m)(>\s*$|›)`,
		},
		{
			ID:             "opencode",
			Binary:         "opencode",
			Args:           nil,
			StartupTimeout: 60 * time.Second,
			StartupProbe:   "output",
			PromptPattern:  `❯`,
		},
		{
			ID:             "gemini",
			Binary:         "agy",
			Args:           nil,
			StartupTimeout: 60 * time.Second,
			StartupProbe:   "prompt",
			PromptPattern:  `^\s*>\s*$`,
		},
	}
}

func generateInstanceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// redactingHandler wraps an existing slog.Handler and redacts string values
// in log messages and attributes. It preserves the wrapped handler's output
// format and configured log level.
type redactingHandler struct {
	inner    slog.Handler
	redactor *redact.Redactor
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	r2 := slog.NewRecord(r.Time, r.Level, h.redactor.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		r2.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, r2)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = h.redactAttr(a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(redacted), redactor: h.redactor}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name), redactor: h.redactor}
}

func (h *redactingHandler) redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindString {
		a.Value = slog.StringValue(h.redactor.Redact(a.Value.String()))
	}
	return a
}
