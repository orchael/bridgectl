package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/localserver"
)

// sdNotify sends a notification to the systemd service manager via
// $NOTIFY_SOCKET. It is a no-op when the socket is not set (i.e. when not
// running under systemd).
//
// systemd uses abstract Unix domain sockets whose path begins with '@'; the
// kernel API requires the leading '@' to be replaced with a NUL byte.
func sdNotify(state string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	// Translate abstract socket notation: '@' prefix → '\0' prefix.
	if len(socket) > 0 && socket[0] == '@' {
		socket = "\x00" + socket[1:]
	}
	addr := &net.UnixAddr{Name: socket, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte(state))
}

// sdWatchdog sends periodic WATCHDOG=1 pings until done is closed.
// WatchdogSec must be set in the systemd unit for these to be effective.
func sdWatchdog(interval time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sdNotify("WATCHDOG=1")
		case <-done:
			return
		}
	}
}

func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Manage the local bridge server",
	}

	cmd.AddCommand(
		newServerInitCmd(),
		newServerStartCmd(),
		newServerStatusCmd(),
		newServerStopCmd(),
		newServerIssueClientCmd(),
		newServerRenewCertCmd(),
	)

	return cmd
}

func newServerStartCmd() *cobra.Command {
	var (
		listenAddr                    string
		securityMode                  string
		serverSANs                    []string
		configPath                    string
		dbPath                        string
		globalRPS                     float64
		logLevel                      string
		logFormat                     string
		stepCAURL                     string
		stepCARootPath                string
		stepCAProvisioner             string
		stepCAProvisionerPasswordFile string
		certValidity                  time.Duration
		certRenewalCheckInterval      time.Duration
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the bridge server in the foreground",
		Long: `Start the bridge server. By default it runs in local mode on a unix
socket with no authentication. Use --listen to bind to a TCP address
with mTLS + JWT for remote access (e.g. over a WireGuard VPN).

Tier 1 (default): PKI material (CA, server cert, JWT keypair) is
auto-generated on first start and stored in ~/.config/bridgectl/certs/.

Tier 2 (optional): Pass --step-ca-url and --step-ca-root to delegate
certificate issuance to a Step CA instance instead of auto-generating.
The 'step' CLI must be on PATH. Suitable for teams with existing OIDC
infrastructure (Google, GitHub, Okta, etc.) managed through Step CA.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if localserver.IsServerRunning("") {
				return fmt.Errorf("server already running")
			}

			// Default config path to the first known per-user config when not set.
			if configPath == "" {
				configPath = defaultServerConfigPath(localserver.StateDir())
			}

			// Build logger from --log-level and --log-format.
			level := slog.LevelWarn
			switch strings.ToLower(logLevel) {
			case "debug":
				level = slog.LevelDebug
			case "info":
				level = slog.LevelInfo
			case "warn", "warning":
				level = slog.LevelWarn
			case "error":
				level = slog.LevelError
			}
			// Secure mode and explicit log-level both default to info-level output.
			if listenAddr != "" && logLevel == "" {
				level = slog.LevelInfo
			}
			var logger *slog.Logger
			if logFormat == "json" {
				logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
			} else {
				logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
			}

			cfg := localserver.Config{
				SecurityMode:                  localserver.ServerMode(securityMode),
				ListenAddr:                    listenAddr,
				ServerSANs:                    serverSANs,
				ConfigPath:                    configPath,
				DBPath:                        dbPath,
				Logger:                        logger,
				StepCAURL:                     strings.TrimRight(stepCAURL, "/"),
				StepCARootPath:                stepCARootPath,
				StepCAProvisioner:             stepCAProvisioner,
				StepCAProvisionerPasswordFile: stepCAProvisionerPasswordFile,
				CertValidity:                  certValidity,
				CertRenewalCheckInterval:      certRenewalCheckInterval,
			}
			if globalRPS > 0 {
				cfg.RateLimits.GlobalRPS = globalRPS
			}

			srv, err := localserver.Start(cfg)
			if err != nil {
				return err
			}

			discoveredMode := localserver.DiscoverMode(localserver.StateDir())
			var modeDesc string
			switch {
			case localserver.IsMutualTLS(discoveredMode):
				modeDesc = fmt.Sprintf("secure (mTLS+JWT on %s)", srv.Addr())
			case discoveredMode == localserver.ModeTLS:
				modeDesc = fmt.Sprintf("secure (TLS+JWT on %s)", srv.Addr())
			default:
				if runtime.GOOS == "windows" {
					modeDesc = "local (TCP localhost, no auth)"
				} else {
					modeDesc = "local (unix socket, no auth)"
				}
			}
			fmt.Fprintf(os.Stderr, "bridgectl server listening — %s (pid %d)\n", modeDesc, os.Getpid())

			// Notify systemd that the server is ready and start the watchdog
			// heartbeat. Both are no-ops when not running under systemd.
			sdNotify("READY=1\nSTATUS=listening")
			watchdogDone := make(chan struct{})
			go sdWatchdog(15*time.Second, watchdogDone)

			// Block until signal.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			sig := <-sigCh
			fmt.Fprintf(os.Stderr, "\nReceived %s, shutting down...\n", sig)
			close(watchdogDone)
			srv.Stop()
			return nil
		},
	}

	cmd.Flags().StringVar(&securityMode, "mode", "", "security mode: local, tls, or mtls (default: auto-detect from --listen)")
	cmd.Flags().StringVar(&listenAddr, "listen", "", "TCP address for secure mode (e.g. 10.0.0.1:9445 or 0.0.0.0:9445)")
	cmd.Flags().StringSliceVar(&serverSANs, "san", nil, "additional server cert SANs (DNS names or IPs)")
	cmd.Flags().StringVar(&configPath, "config", "", "path to YAML config file (merged with flag values; flags take precedence)")
	cmd.Flags().StringVar(&dbPath, "db-path", "", "path to BoltDB session store for persistence across restarts")
	cmd.Flags().Float64Var(&globalRPS, "rate-limit-global-rps", 0, "override global RPS rate limit (default 100)")
	cmd.Flags().StringVar(&logLevel, "log-level", "", "log level: debug, info, warn, error (default warn; info when --listen is set)")
	cmd.Flags().StringVar(&logFormat, "log-format", "text", "log format: text or json")
	cmd.Flags().StringVar(&stepCAURL, "step-ca-url", "", "Step CA URL for Tier-2 PKI (e.g. https://step-ca.internal:443); requires --step-ca-root")
	cmd.Flags().StringVar(&stepCARootPath, "step-ca-root", "", "path to the Step CA root certificate (required with --step-ca-url)")
	cmd.Flags().StringVar(&stepCAProvisioner, "step-ca-provisioner", "", "Step CA provisioner name (e.g. acme, bridge-jwk); defaults to CA's default provisioner")
	cmd.Flags().StringVar(&stepCAProvisionerPasswordFile, "step-ca-provisioner-password-file", "", "path to provisioner password file for non-interactive Step CA cert requests")
	cmd.Flags().DurationVar(&certValidity, "cert-validity", 0, "server certificate validity duration (e.g. 30m, 24h, 2160h); default 90 days")
	cmd.Flags().DurationVar(&certRenewalCheckInterval, "cert-renewal-check-interval", 0, "how often to check certificate expiry (e.g. 10m, 1h); default 1 hour")

	return cmd
}

func defaultServerConfigPath(stateDir string) string {
	for _, path := range defaultServerConfigCandidates(stateDir) {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func defaultServerConfigCandidates(stateDir string) []string {
	candidates := []string{filepath.Join(stateDir, "bridge.yaml")}
	if xdgConfigHome := os.Getenv("XDG_CONFIG_HOME"); xdgConfigHome != "" {
		candidates = append(candidates, filepath.Join(xdgConfigHome, "bridgectl", "config.yaml"))
	}
	if configDir, err := os.UserConfigDir(); err == nil && configDir != "" {
		path := filepath.Join(configDir, "bridgectl", "config.yaml")
		if !stringSliceContains(candidates, path) {
			candidates = append(candidates, path)
		}
	}
	return candidates
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func newServerStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check server status",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, mode := localserver.DiscoverTarget("")
			if target == "" {
				fmt.Println("Server: not running")
				return nil
			}

			// Read PID file.
			pidData, _ := os.ReadFile(localserver.PIDPath())
			pid := strings.TrimSpace(string(pidData))

			client, err := connectClient("", 3*time.Second)
			if err != nil {
				fmt.Printf("Server: stale (cannot connect to %s)\n", target)
				return nil
			}
			defer func() { _ = client.Close() }()

			resp, err := client.Health(cmd.Context())
			if err != nil {
				fmt.Printf("Server: reachable but unhealthy (%v)\n", err)
				return nil
			}

			fmt.Printf("Server: running\n")
			fmt.Printf("  Mode:        %s\n", mode)
			fmt.Printf("  PID:         %s\n", pid)
			fmt.Printf("  Address:     %s\n", target)
			fmt.Printf("  Instance:    %s\n", resp.ServerInstanceId)
			fmt.Printf("  Providers:   %d\n", len(resp.Providers))
			for _, p := range resp.Providers {
				avail := "available"
				if !p.Available {
					avail = "unavailable"
				}
				fmt.Printf("    %-12s %s\n", p.Provider, avail)
			}
			return nil
		},
	}
	return cmd
}

func newServerStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the bridge server",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidData, err := os.ReadFile(localserver.PIDPath())
			if err != nil {
				return fmt.Errorf("no server PID file found (is the server running?)")
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if err != nil {
				return fmt.Errorf("invalid PID file: %w", err)
			}

			proc, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("find process %d: %w", pid, err)
			}
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				// SIGTERM is not supported on Windows; fall back to Kill.
				if killErr := proc.Kill(); killErr != nil {
					return fmt.Errorf("kill process %d: %w (SIGTERM failed: %v)", pid, killErr, err)
				}
				fmt.Printf("Killed server (pid %d)\n", pid)
			} else {
				fmt.Printf("Sent SIGTERM to server (pid %d)\n", pid)
			}

			// Wait until the server is actually down before cleaning up
			// state files.
			for i := 0; i < 30; i++ {
				time.Sleep(200 * time.Millisecond)
				if !localserver.IsServerRunning("") {
					break
				}
			}

			// Only remove state files if the server is no longer responding.
			if !localserver.IsServerRunning("") {
				_ = os.Remove(localserver.PIDPath())
				_ = os.Remove(localserver.SocketPath())
				_ = os.Remove(localserver.AddrPath())
				stateDir := localserver.StateDir()
				_ = os.Remove(filepath.Join(stateDir, "server.mode"))
			} else {
				fmt.Fprintf(os.Stderr, "Warning: server still responding after SIGTERM; state files not removed\n")
			}

			return nil
		},
	}
	return cmd
}

func newServerIssueClientCmd() *cobra.Command {
	var (
		clientName     string
		oidcProvider   string
		stepCAURL      string
		stepCARootPath string
		bundleCreds    bool
		deployTarget   string
	)

	cmd := &cobra.Command{
		Use:   "issue-client",
		Short: "Issue a client certificate for a remote machine",
		Long: `Generate a client certificate and JWT keypair for a remote machine.
Each client gets its own signing key so credentials can be rotated or
revoked independently. The remote machine needs these files:

  1. CA bundle       (ca-bundle.crt)   — to verify the server
  2. Client cert     (<name>.crt)      — to authenticate to the server
  3. Client key      (<name>.key)      — private key for the cert
  4. JWT signing key (jwt-signing.key) — per-client key to mint tokens

Use --bundle to create a .tar.gz of these files for easy transfer.
Use --deploy <host:path> to scp the bundle to a remote machine directly.

Tier 1 (default): The certificate is signed by the local auto-generated CA.
Run 'bridgectl server start --listen' first to generate PKI.

Tier 2 (Step CA + OIDC): Pass --oidc-provider, --step-ca-url, and
--step-ca-root to enrol via an OIDC login flow. A browser window will open
for interactive authentication. The 'step' CLI must be on PATH.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if clientName == "" {
				return fmt.Errorf("--name is required")
			}

			stateDir := localserver.StateDir()
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			var certPath, keyPath string
			var issueErr error

			if oidcProvider != "" {
				// Tier 2: obtain cert from Step CA via OIDC.
				stepCfg := &localserver.StepCAConfig{
					URL:             strings.TrimRight(stepCAURL, "/"),
					RootPath:        stepCARootPath,
					OIDCProviderURL: oidcProvider,
				}
				certPath, keyPath, issueErr = localserver.IssueClientCertViaOIDC(stateDir, clientName, stepCfg, logger)
			} else {
				// Tier 1: sign with local auto-generated CA.
				certPath, keyPath, issueErr = localserver.IssueClientCert(stateDir, clientName, logger)
			}
			if issueErr != nil {
				return issueErr
			}

			mat := localserver.LoadPKIMaterial(stateDir)
			clientDir := filepath.Join(localserver.CertsDir(stateDir), "clients", clientName)
			clientJWTKey := filepath.Join(clientDir, "jwt-signing.key")

			fmt.Println("Client credentials issued successfully.")
			fmt.Println()
			fmt.Println("  CA bundle:       " + mat.CABundlePath)
			fmt.Println("  Client cert:     " + certPath)
			fmt.Println("  Client key:      " + keyPath)
			fmt.Println("  JWT signing key: " + clientJWTKey)

			// --deploy implies --bundle.
			if deployTarget != "" {
				bundleCreds = true
			}

			if bundleCreds {
				bundlePath := filepath.Join(clientDir, clientName+"-creds.tar.gz")
				if err := localserver.BundleClientCreds(bundlePath, stateDir, clientName); err != nil {
					return fmt.Errorf("bundle credentials: %w", err)
				}
				fmt.Println()
				fmt.Println("  Credential bundle: " + bundlePath)

				if deployTarget != "" {
					fmt.Println()
					fmt.Printf("Deploying to %s...\n", deployTarget)
					if err := localserver.DeployBundle(bundlePath, deployTarget, logger); err != nil {
						return fmt.Errorf("deploy credentials: %w", err)
					}
					fmt.Printf("Credentials deployed to %s\n", deployTarget)
				}
			}

			fmt.Println()
			fmt.Println("The server will accept tokens from this client on next restart.")
			fmt.Println("If the server is already running, restart it to load the new key.")
			fmt.Println()
			fmt.Println("Example Go SDK usage:")
			fmt.Println()
			serverName := localserver.DiscoverServerName(stateDir)
			fmt.Printf("  client, err := bridgeclient.New(\n")
			fmt.Printf("    bridgeclient.WithTarget(\"<server-addr>:9445\"),\n")
			fmt.Printf("    bridgeclient.WithMTLS(bridgeclient.MTLSConfig{\n")
			fmt.Printf("      CABundlePath: \"ca-bundle.crt\",\n")
			fmt.Printf("      CertPath:     \"%s.crt\",\n", clientName)
			fmt.Printf("      KeyPath:      \"%s.key\",\n", clientName)
			fmt.Printf("      ServerName:   \"%s\",\n", serverName)
			fmt.Printf("    }),\n")
			fmt.Printf("    bridgeclient.WithJWT(bridgeclient.JWTConfig{\n")
			fmt.Printf("      PrivateKeyPath: \"jwt-signing.key\",\n")
			fmt.Printf("      Issuer:         \"%s\",\n", clientName)
			fmt.Printf("      Audience:       \"bridge\",\n")
			fmt.Printf("    }),\n")
			fmt.Printf("  )\n")

			return nil
		},
	}

	cmd.Flags().StringVar(&clientName, "name", "", "client name (used as cert CN and filenames)")
	_ = cmd.MarkFlagRequired("name")
	cmd.Flags().StringVar(&oidcProvider, "oidc-provider", "", "OIDC issuer URL for Step CA enrollment (e.g. https://accounts.google.com); enables Tier-2 flow")
	cmd.Flags().StringVar(&stepCAURL, "step-ca-url", "", "Step CA server URL (required with --oidc-provider)")
	cmd.Flags().StringVar(&stepCARootPath, "step-ca-root", "", "path to Step CA root certificate (required with --oidc-provider)")
	cmd.Flags().BoolVar(&bundleCreds, "bundle", false, "create a .tar.gz bundle of client credentials for easy transfer")
	cmd.Flags().StringVar(&deployTarget, "deploy", "", "scp credential bundle to a remote host (e.g. do-dev2:~/); implies --bundle. Run 'bridgectl client setup --bundle <file>' on the remote to extract.")

	return cmd
}

func newServerRenewCertCmd() *cobra.Command {
	var (
		configPath                    string
		serverSANs                    []string
		stepCAURL                     string
		stepCARootPath                string
		stepCAProvisioner             string
		stepCAProvisionerPasswordFile string
	)

	cmd := &cobra.Command{
		Use:   "renew-cert",
		Short: "Renew the server TLS certificate without restarting",
		Long: `Re-issue the server TLS certificate using the existing CA (auto-PKI) or
by requesting a new one from Step CA. The new certificate is written to
the same file paths so the running server picks it up on the next TLS
handshake — no restart required.

Existing client connections are unaffected; only new connections use the
renewed certificate. Client trust bundles do not need to change because
the same CA signed both the old and new certificates.

The running server also performs automatic background renewal when less
than 1/3 of the certificate's lifetime remains. Use this command for
immediate renewal (e.g. when the cert has already expired).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir := localserver.StateDir()
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			// Load config file for SANs and Step CA settings.
			if configPath == "" {
				defaultCfg := filepath.Join(stateDir, "bridge.yaml")
				if _, err := os.Stat(defaultCfg); err == nil {
					configPath = defaultCfg
				}
			}
			var certValidity time.Duration
			mat := localserver.LoadPKIMaterial(stateDir)
			if configPath != "" {
				fileCfg, err := config.Load(configPath)
				if err != nil {
					logger.Warn("could not load config file", "path", configPath, "error", err)
				} else {
					if (fileCfg.TLS.Cert == "") != (fileCfg.TLS.Key == "") {
						return fmt.Errorf("config %q must set both tls.cert and tls.key or neither", configPath)
					}
					if fileCfg.TLS.Cert != "" {
						mat.ServerCertPath = fileCfg.TLS.Cert
						mat.ServerKeyPath = fileCfg.TLS.Key
					}
					if len(serverSANs) == 0 && len(fileCfg.Server.SANs) > 0 {
						serverSANs = fileCfg.Server.SANs
					}
					if stepCAURL == "" && fileCfg.StepCA.URL != "" {
						stepCAURL = fileCfg.StepCA.URL
					}
					if stepCARootPath == "" && fileCfg.StepCA.Root != "" {
						stepCARootPath = fileCfg.StepCA.Root
					}
					if stepCAProvisioner == "" && fileCfg.StepCA.Provisioner != "" {
						stepCAProvisioner = fileCfg.StepCA.Provisioner
					}
					if stepCAProvisionerPasswordFile == "" && fileCfg.StepCA.ProvisionerPasswordFile != "" {
						stepCAProvisionerPasswordFile = fileCfg.StepCA.ProvisionerPasswordFile
					}
					if fileCfg.Server.Listen != "" {
						serverSANs = localserver.BuildServerSANs(fileCfg.Server.Listen, serverSANs)
					}
					if fileCfg.Server.CertValidity != "" {
						certValidity = config.ParseDuration(fileCfg.Server.CertValidity, 0)
					}
				}
			}

			// Ensure we have at least the default SANs.
			if len(serverSANs) == 0 {
				serverSANs = []string{"server", "127.0.0.1", "localhost"}
			}

			// Show current cert status.
			_, notAfter, err := localserver.ServerCertExpiry(mat.ServerCertPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not read current cert: %v\n", err)
			} else {
				remaining := time.Until(notAfter)
				if remaining <= 0 {
					fmt.Fprintf(os.Stderr, "Current certificate EXPIRED %s ago.\n", (-remaining).Round(time.Second))
				} else {
					fmt.Fprintf(os.Stderr, "Current certificate expires in %s (%s).\n",
						remaining.Round(time.Second), notAfter.Format(time.RFC3339))
				}
			}

			var stepCA *localserver.StepCAConfig
			if stepCAURL != "" {
				stepCA = &localserver.StepCAConfig{
					URL:                     strings.TrimRight(stepCAURL, "/"),
					RootPath:                stepCARootPath,
					Provisioner:             stepCAProvisioner,
					ProvisionerPasswordFile: stepCAProvisionerPasswordFile,
				}
			}

			if err := localserver.RenewServerCertMaterial(mat, serverSANs, logger, stepCA, certValidity); err != nil {
				return fmt.Errorf("renew cert: %w", err)
			}

			// Show new cert status.
			_, notAfter, err = localserver.ServerCertExpiry(mat.ServerCertPath)
			if err != nil {
				return fmt.Errorf("verify renewed cert: %w", err)
			}

			fmt.Fprintf(os.Stderr, "Certificate renewed. New expiry: %s (%s from now).\n",
				notAfter.Format(time.RFC3339), time.Until(notAfter).Round(time.Second))
			fmt.Fprintln(os.Stderr, "The running server will use the new certificate for all new TLS connections.")
			fmt.Fprintln(os.Stderr, "Existing connections are unaffected.")

			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to YAML config file (default: ~/.config/bridgectl/bridge.yaml)")
	cmd.Flags().StringSliceVar(&serverSANs, "san", nil, "server cert SANs (overrides config file)")
	cmd.Flags().StringVar(&stepCAURL, "step-ca-url", "", "Step CA URL (overrides config file)")
	cmd.Flags().StringVar(&stepCARootPath, "step-ca-root", "", "Step CA root cert path (overrides config file)")
	cmd.Flags().StringVar(&stepCAProvisioner, "step-ca-provisioner", "", "Step CA provisioner name (overrides config file)")
	cmd.Flags().StringVar(&stepCAProvisionerPasswordFile, "step-ca-provisioner-password-file", "", "provisioner password file (overrides config file)")

	return cmd
}
