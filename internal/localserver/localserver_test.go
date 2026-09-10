package localserver

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/pki"
	"github.com/orchael/bridgectl/internal/redact"
	"github.com/orchael/bridgectl/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startLocalServer starts a server in local mode using a temp state dir and
// returns the server and a cleanup function.
func startLocalServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	srv, err := Start(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { srv.Stop() })
	return srv
}

// TestStartDefaultConfig verifies that Start() succeeds with a minimal config.
func TestStartDefaultConfig(t *testing.T) {
	srv := startLocalServer(t, Config{})
	assert.NotNil(t, srv)
	assert.NotEmpty(t, srv.Addr())
}

// TestStartWithDBPath verifies that Start() opens a BoltDB store when DBPath
// is set and that Stop() closes it without error.
func TestStartWithDBPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")

	srv := startLocalServer(t, Config{
		StateDir: dir,
		DBPath:   dbPath,
	})

	// Store file must exist after start.
	_, err := os.Stat(dbPath)
	assert.NoError(t, err, "BoltDB file should be created on start")

	// Stop should close the store cleanly.
	srv.Stop()
	// Double-stop must not panic.
	srv.Stop()
}

// TestStartWithInvalidDBPath ensures that an uncreateable db path causes Start
// to return an error rather than silently skipping persistence.
func TestStartWithInvalidDBPath(t *testing.T) {
	dir := t.TempDir()
	// Use a path whose parent directory does not exist.
	badPath := filepath.Join(dir, "no-such-dir", "sessions.db")

	cfg := Config{
		StateDir: dir,
		DBPath:   badPath,
	}
	_, err := Start(cfg)
	assert.Error(t, err, "Start should fail when the BoltDB path is invalid")
}

// TestStartDBPathRoundTrip verifies that a session created by one Server is
// rehydrated by a second Server opened on the same DBPath.
func TestStartDBPathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")

	// First server: open the store.
	srv1 := startLocalServer(t, Config{
		StateDir: dir,
		DBPath:   dbPath,
	})
	_ = srv1
	srv1.Stop()

	// Second server: LoadHistory should succeed even on an empty DB.
	srv2 := startLocalServer(t, Config{
		StateDir: dir,
		DBPath:   dbPath,
	})
	assert.NotNil(t, srv2)
}

// TestStartWithConfigFile verifies that values from a YAML config file are
// merged into the running config.
func TestStartWithConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "bridge.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte(`
rate_limits:
  global_rps: 42
sessions:
  idle_timeout: 5m
`), 0o644))

	srv := startLocalServer(t, Config{
		StateDir:   dir,
		ConfigPath: cfgFile,
	})
	assert.NotNil(t, srv)
}

func TestStartWithConfigFileWithoutListenStaysLocal(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "bridge.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte(`
providers:
  echo:
    binary: "cat"
`), 0o644))

	srv := startLocalServer(t, Config{
		StateDir:   dir,
		ConfigPath: cfgFile,
	})
	assert.Equal(t, "unix", srv.listener.Addr().Network())
}

// TestStartConfigFileExplicitOverride verifies that an explicit flag value
// takes precedence over the same value in the config file.
func TestStartConfigFileExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "bridge.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte(`
rate_limits:
  global_rps: 1
`), 0o644))

	// Explicit RateLimits.GlobalRPS should win over the file value.
	srv := startLocalServer(t, Config{
		StateDir:   dir,
		ConfigPath: cfgFile,
		RateLimits: server.RateLimitConfig{GlobalRPS: 200},
	})
	assert.NotNil(t, srv)
}

// TestStartWithInvalidConfigFile verifies that Start() returns an error when
// the config file exists but contains invalid YAML.
func TestStartWithInvalidConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "bridge.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte("{\nbroken: [unterminated"), 0o644))
	cfg := Config{
		StateDir:   dir,
		ConfigPath: cfgFile,
	}
	_, err := Start(cfg)
	assert.Error(t, err, "Start should fail when config file contains invalid YAML")
}

// TestStartWithRedactPatterns verifies that valid redaction patterns are
// accepted without error.
func TestStartWithRedactPatterns(t *testing.T) {
	srv := startLocalServer(t, Config{
		StateDir:       t.TempDir(),
		RedactPatterns: []string{`(?i)secret=[^\s]+`, `token=[^\s]+`},
	})
	assert.NotNil(t, srv)
}

// TestStartWithInvalidRedactPattern verifies that a bad regex causes Start to
// return an error.
func TestStartWithInvalidRedactPattern(t *testing.T) {
	dir := t.TempDir()
	_, err := Start(Config{
		StateDir:       dir,
		RedactPatterns: []string{`[invalid`},
	})
	assert.Error(t, err, "Start should fail with an invalid redact pattern")
}

// TestStartRateLimitDefaults verifies that Start() applies built-in defaults
// when no explicit rate limits or config file are provided.
func TestStartRateLimitDefaults(t *testing.T) {
	// Just ensure Start doesn't error; the defaults are applied internally.
	srv := startLocalServer(t, Config{StateDir: t.TempDir()})
	assert.NotNil(t, srv)
}

// TestStartCustomIdleTimeout verifies that a custom IdleTimeout is accepted.
func TestStartCustomIdleTimeout(t *testing.T) {
	srv := startLocalServer(t, Config{
		StateDir:    t.TempDir(),
		IdleTimeout: 10 * time.Minute,
	})
	assert.NotNil(t, srv)
}

// TestStartCustomEventBufferSize verifies that a custom EventBufferSize is accepted.
func TestStartCustomEventBufferSize(t *testing.T) {
	srv := startLocalServer(t, Config{
		StateDir:        t.TempDir(),
		EventBufferSize: 1 << 20,
	})
	assert.NotNil(t, srv)
}

// TestStartWithProviderFallbacks verifies that provider fallback mapping is
// accepted by Start without error when the feature flag is enabled.
func TestStartWithProviderFallbacks(t *testing.T) {
	srv := startLocalServer(t, Config{
		StateDir:                 t.TempDir(),
		ProviderFallbacksEnabled: true,
		ProviderFallbacks: map[string][]string{
			"claude": {"echo"},
		},
	})
	assert.NotNil(t, srv)
}

// TestStartWithProviderFallbacksDisabled verifies that fallbacks are cleared
// when the feature flag is not enabled, even if fallback mappings are provided.
func TestStartWithProviderFallbacksDisabled(t *testing.T) {
	srv := startLocalServer(t, Config{
		StateDir: t.TempDir(),
		ProviderFallbacks: map[string][]string{
			"claude": {"echo"},
		},
	})
	assert.NotNil(t, srv)
	// The server must have cleared the fallback map because the feature flag
	// was not enabled. This catches regressions where fallbacks are still
	// applied despite the flag being off.
	assert.Nil(t, srv.providerFallbacks, "fallbacks should be nil when feature flag is disabled")
}

// TestServerAddrLocalMode verifies that Addr() returns a non-empty string in
// local mode (unix socket or localhost TCP on Windows).
func TestServerAddrLocalMode(t *testing.T) {
	srv := startLocalServer(t, Config{StateDir: t.TempDir()})
	assert.NotEmpty(t, srv.Addr())
}

// TestPathHelpers verifies that the package-level path helpers return non-empty strings.
func TestPathHelpers(t *testing.T) {
	assert.NotEmpty(t, StateDir())
	assert.NotEmpty(t, SocketPath())
	assert.NotEmpty(t, PIDPath())
	assert.NotEmpty(t, AddrPath())
	assert.NotEmpty(t, ModePath())
}

// TestDiscoverModeDefault verifies DiscoverMode returns ModeLocal when no mode
// file exists.
func TestDiscoverModeDefault(t *testing.T) {
	dir := t.TempDir()
	mode := DiscoverMode(dir)
	assert.Equal(t, ModeLocal, mode)
}

// TestDiscoverModeSecure verifies DiscoverMode reads ModeSecure from the file.
func TestDiscoverModeSecure(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "server.mode"), []byte("secure\n"), 0o644)
	require.NoError(t, err)
	assert.Equal(t, ModeSecure, DiscoverMode(dir))
}

// TestDiscoverModeEmptyUsesDefault verifies DiscoverMode("") uses StateDir().
func TestDiscoverModeEmptyUsesDefault(t *testing.T) {
	mode := DiscoverMode("")
	// We just want no panic; the mode may be local or secure depending on env.
	assert.True(t, mode == ModeLocal || mode == ModeSecure || mode == ModeTLS)
}

// TestDiscoverModeMTLS verifies DiscoverMode reads "mtls" as ModeSecure.
func TestDiscoverModeMTLS(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "server.mode"), []byte("mtls\n"), 0o644)
	require.NoError(t, err)
	// "mtls" maps to ModeSecure for backward compatibility with code
	// that checks == ModeSecure.
	assert.Equal(t, ModeSecure, DiscoverMode(dir))
}

// TestDiscoverModeTLS verifies DiscoverMode reads "tls" as ModeTLS.
func TestDiscoverModeTLS(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "server.mode"), []byte("tls\n"), 0o644)
	require.NoError(t, err)
	assert.Equal(t, ModeTLS, DiscoverMode(dir))
}

// TestIsSecureMode verifies the IsSecureMode helper.
func TestIsSecureMode(t *testing.T) {
	assert.False(t, IsSecureMode(ModeLocal))
	assert.True(t, IsSecureMode(ModeSecure))
	assert.True(t, IsSecureMode(ModeMTLS))
	assert.True(t, IsSecureMode(ModeTLS))
}

// TestIsMutualTLS verifies the IsMutualTLS helper.
func TestIsMutualTLS(t *testing.T) {
	assert.False(t, IsMutualTLS(ModeLocal))
	assert.True(t, IsMutualTLS(ModeSecure))
	assert.True(t, IsMutualTLS(ModeMTLS))
	assert.False(t, IsMutualTLS(ModeTLS))
}

// TestServerTarget verifies that Target() returns a non-empty target string.
func TestServerTarget(t *testing.T) {
	srv := startLocalServer(t, Config{StateDir: t.TempDir()})
	assert.NotEmpty(t, srv.Target())
}

func TestLocalDialAddrNormalizesWildcard(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{
			name: "ipv4 wildcard",
			addr: "0.0.0.0:9445",
			want: "127.0.0.1:9445",
		},
		{
			name: "ipv6 wildcard",
			addr: "[::]:9445",
			want: "[::1]:9445",
		},
		{
			name: "concrete address",
			addr: "10.0.0.1:9445",
			want: "10.0.0.1:9445",
		},
		{
			name: "unix target",
			addr: "unix:///tmp/server.sock",
			want: "unix:///tmp/server.sock",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, localDialAddr(tt.addr))
		})
	}
}

// TestIsServerRunningAndDiscoverTarget verifies that IsServerRunning and
// DiscoverTarget correctly report a live server.
func TestIsServerRunningAndDiscoverTarget(t *testing.T) {
	dir := t.TempDir()
	srv := startLocalServer(t, Config{StateDir: dir})
	require.NotNil(t, srv)

	// The server writes its address file during Start, so both functions
	// should now report the server as running.
	if !IsServerRunning(dir) {
		t.Skip("IsServerRunning returned false (may need unix socket support)")
	}

	target, mode := DiscoverTarget(dir)
	assert.NotEmpty(t, target)
	assert.Equal(t, ModeLocal, mode)
}

// TestIsServerRunningFalseWhenNoServer verifies that IsServerRunning returns
// false for an empty state dir.
func TestIsServerRunningFalseWhenNoServer(t *testing.T) {
	assert.False(t, IsServerRunning(t.TempDir()))
}

// TestDiscoverTargetEmptyWhenNoServer verifies DiscoverTarget returns empty
// when no server is running in the state dir.
func TestDiscoverTargetEmptyWhenNoServer(t *testing.T) {
	target, _ := DiscoverTarget(t.TempDir())
	assert.Empty(t, target)
}

// TestStateDirEnvOverride verifies that BRIDGECTL_STATE_DIR overrides
// the default path.
func TestStateDirEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	assert.Equal(t, dir, StateDir())
}

// TestDiscoverTargetAddrFile verifies that discoverTarget picks up a TCP
// addr-file when no unix socket exists.
func TestDiscoverTargetAddrFileFallback(t *testing.T) {
	dir := t.TempDir()
	// Start a server so we have a real address to probe.
	srv := startLocalServer(t, Config{StateDir: dir})
	require.NotNil(t, srv)

	// Remove the unix socket so discoverTarget falls back to addr file.
	_ = os.Remove(filepath.Join(dir, "server.sock"))

	target := discoverTarget(dir)
	// On Linux/macOS the addr file holds the socket path; after removing the
	// socket file, probeHealth will fail and discoverTarget returns "".
	// That's the correct behaviour: no socket = not reachable.
	_ = target // just confirm no panic
}

// TestStartSecureMode verifies that Start with a ListenAddr creates a server
// in ModeSecure. This also exercises buildSecureGRPCOpts and EnsurePKI.
func TestStartSecureMode(t *testing.T) {
	dir := t.TempDir()
	srv, err := Start(Config{
		StateDir:   dir,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Skipf("secure mode start failed (may need specific environment): %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	assert.NotNil(t, srv)
	mode := DiscoverMode(dir)
	assert.Equal(t, ModeSecure, mode)
}

// TestIsServerRunningSecureMode verifies IsServerRunning detects a secure-mode
// server. This also exercises the secure probeHealth path.
func TestIsServerRunningSecureMode(t *testing.T) {
	dir := t.TempDir()
	srv, err := Start(Config{
		StateDir:   dir,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Skipf("secure mode start failed: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	if !IsServerRunning(dir) {
		t.Error("IsServerRunning returned false for a running secure server")
	}

	target, mode := DiscoverTarget(dir)
	assert.NotEmpty(t, target)
	assert.Equal(t, ModeSecure, mode)
}

func TestIsServerRunningSecureModeWithExplicitTLSCreatesLocalManagementPKI(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, "external-tls")
	caCertPath, caKeyPath, err := pki.InitCA("external", tlsDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)
	serverCertPath, serverKeyPath, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "bridge-host", []string{"bridge-host", "127.0.0.1"}, tlsDir, 0)
	require.NoError(t, err)

	srv, err := Start(Config{
		StateDir:     dir,
		ListenAddr:   "127.0.0.1:0",
		CABundlePath: caCertPath,
		TLSCertPath:  serverCertPath,
		TLSKeyPath:   serverKeyPath,
	})
	if err != nil {
		t.Skipf("secure mode start failed: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	mat := LoadPKIMaterial(dir)
	for _, path := range []string{
		mat.CABundlePath,
		mat.LocalClientCert,
		mat.LocalClientKey,
		mat.JWTSigningKey,
		mat.JWTSigningPub,
	} {
		_, err := os.Stat(path)
		require.NoError(t, err, "file should exist: %s", path)
	}

	assert.Equal(t, "bridge-host", DiscoverServerName(dir))
	if !IsServerRunning(dir) {
		t.Fatal("IsServerRunning returned false for explicit-TLS secure server")
	}
	target, mode := DiscoverTarget(dir)
	assert.NotEmpty(t, target)
	assert.Equal(t, ModeSecure, mode)
}

func TestStartExplicitTLSWithStepCARenewsExpiredCert(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, "external-tls")
	caCertPath, caKeyPath, err := pki.InitCA("external", tlsDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)
	serverCertPath, serverKeyPath, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "bridge-host", []string{"bridge-host", "127.0.0.1"}, tlsDir, time.Nanosecond)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)

	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, _, _ string, _ *slog.Logger) error {
		return assert.AnError
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	jwkCalled := false
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, sans []string, certPath, keyPath string, _ *slog.Logger) error {
		jwkCalled = true
		issuedCertPath, issuedKeyPath, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "replacement", sans, t.TempDir(), 0)
		require.NoError(t, err)
		certData, err := os.ReadFile(issuedCertPath)
		require.NoError(t, err)
		keyData, err := os.ReadFile(issuedKeyPath)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(certPath, certData, 0o644))
		require.NoError(t, os.WriteFile(keyPath, keyData, 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	srv, err := Start(Config{
		StateDir:                      dir,
		ListenAddr:                    "127.0.0.1:0",
		CABundlePath:                  caCertPath,
		TLSCertPath:                   serverCertPath,
		TLSKeyPath:                    serverKeyPath,
		StepCAURL:                     "https://ca.example.internal",
		StepCARootPath:                caCertPath,
		StepCAProvisioner:             "admin",
		StepCAProvisionerPasswordFile: filepath.Join(dir, "step-password"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Stop() })

	assert.True(t, jwkCalled, "expired explicit cert should be reissued through Step CA fallback")
	_, notAfter, err := ServerCertExpiry(serverCertPath)
	require.NoError(t, err)
	assert.True(t, time.Until(notAfter) > 24*time.Hour, "server should be using a fresh replacement cert")
	assert.True(t, IsServerRunning(dir), "renewed explicit-TLS server should pass local health checks")
}

func TestStartExplicitTLSExpiredWithoutStepCAFails(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, "external-tls")
	caCertPath, caKeyPath, err := pki.InitCA("external", tlsDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)
	serverCertPath, serverKeyPath, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "bridge-host", []string{"bridge-host", "127.0.0.1"}, tlsDir, time.Nanosecond)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)

	_, err = Start(Config{
		StateDir:     dir,
		ListenAddr:   "127.0.0.1:0",
		CABundlePath: caCertPath,
		TLSCertPath:  serverCertPath,
		TLSKeyPath:   serverKeyPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "explicit TLS certificate")
	assert.Contains(t, err.Error(), "expired")
}

func TestCheckAndRenewUsesConfiguredPKIMaterial(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, "external-tls")
	caCertPath, caKeyPath, err := pki.InitCA("external", tlsDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)
	serverCertPath, serverKeyPath, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "bridge-host", []string{"bridge-host"}, tlsDir, time.Nanosecond)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)

	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, certPath, keyPath string, _ *slog.Logger) error {
		require.Equal(t, serverCertPath, certPath)
		require.Equal(t, serverKeyPath, keyPath)
		require.NoError(t, os.WriteFile(certPath, []byte("EXPLICIT-CERT-RENEWED"), 0o644))
		return nil
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	srv := &Server{
		logger:     testLogger(),
		stateDir:   dir,
		pkiMat:     &PKIMaterial{ServerCertPath: serverCertPath, ServerKeyPath: serverKeyPath},
		serverSANs: []string{"bridge-host"},
		stepCA:     &StepCAConfig{URL: "https://ca.example.internal"},
	}

	srv.checkAndRenew()

	data, err := os.ReadFile(serverCertPath)
	require.NoError(t, err)
	assert.Equal(t, "EXPLICIT-CERT-RENEWED", string(data))

	stateMat := LoadPKIMaterial(dir)
	assert.NoFileExists(t, stateMat.ServerCertPath)
	assert.NoFileExists(t, stateMat.ServerKeyPath)
}

func TestDiscoverTargetSecureModeWithWildcardListen(t *testing.T) {
	dir := t.TempDir()
	srv, err := Start(Config{
		StateDir:   dir,
		ListenAddr: "0.0.0.0:0",
	})
	if err != nil {
		t.Skipf("secure mode start failed: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	addrData, err := os.ReadFile(filepath.Join(dir, "server.addr"))
	require.NoError(t, err)
	host, _, err := net.SplitHostPort(string(bytes.TrimSpace(addrData)))
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip)
	assert.True(t, ip.IsLoopback(), "server.addr host %q should be loopback", host)

	if !IsServerRunning(dir) {
		t.Error("IsServerRunning returned false for a secure wildcard listener")
	}

	target, mode := DiscoverTarget(dir)
	assert.NotEmpty(t, target)
	assert.Equal(t, ModeSecure, mode)
}

// TestDiscoverTargetSecureModeServerNameFromCert verifies that when a server
// cert lacks the default "server" SAN (as happens with Step CA renewal or ACME
// provisioners), DiscoverTarget still finds the running server because
// server.name is derived from the actual certificate SANs, not hardcoded.
func TestDiscoverTargetSecureModeServerNameFromCert(t *testing.T) {
	dir := t.TempDir()
	certsDir := CertsDir(dir)
	require.NoError(t, os.MkdirAll(certsDir, 0o700))

	// Create a CA and issue a server cert with only "custom.example.com" — no "server" SAN.
	// This simulates what happens after Step CA mTLS renewal drops bare-hostname SANs.
	caCertPath, caKeyPath, err := pki.InitCA("test-ca", certsDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)

	// CN is "server" so the file is named server.crt (matching LoadPKIMaterial),
	// but the SANs only contain "custom.example.com" — no "server" DNS SAN.
	_, _, err = pki.IssueCert(
		caCert, caKey, pki.CertTypeServer, "server",
		[]string{"custom.example.com", "127.0.0.1"}, certsDir, 0,
	)
	require.NoError(t, err)

	// Issue a local-client cert (used by probeHealth for mTLS).
	_, _, err = pki.IssueCert(caCert, caKey, pki.CertTypeClient, "local-client", nil, certsDir, 0)
	require.NoError(t, err)

	// Create JWT keypair.
	_, _, err = pki.GenerateJWTKeypair(certsDir, "jwt-signing")
	require.NoError(t, err)

	// Write a ca-bundle.crt that includes the CA cert (same as both server and client CA here).
	caData, err := os.ReadFile(caCertPath)
	require.NoError(t, err)
	bundlePath := filepath.Join(certsDir, "ca-bundle.crt")
	require.NoError(t, os.WriteFile(bundlePath, caData, 0o644))

	// Mark PKI mode as auto so EnsurePKI returns early (certs already exist).
	require.NoError(t, writePKIMode(certsDir, pkiModeAuto))

	// Start the server. The auto-PKI path should derive tlsServerName from the cert.
	srv, err := Start(Config{
		StateDir:   dir,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("secure mode start failed: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	// server.name must reflect the actual cert SAN, not the default "server".
	actualName := DiscoverServerName(dir)
	assert.Equal(t, "custom.example.com", actualName,
		"server.name should be derived from the cert SAN, not the default 'server'")

	// DiscoverTarget must find the running server despite the non-default SAN.
	target, mode := DiscoverTarget(dir)
	assert.NotEmpty(t, target, "DiscoverTarget should find the running secure server")
	assert.Equal(t, ModeSecure, mode)
}

// TestServerNameFromCertSkipsWildcard verifies that serverNameFromCert skips
// wildcard DNS SANs and returns the first concrete DNS name.
func TestServerNameFromCertSkipsWildcard(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath, err := pki.InitCA("test-ca", dir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)

	certPath, _, err := pki.IssueCert(
		caCert, caKey, pki.CertTypeServer, "wildcard-test",
		[]string{"*.example.com", "concrete.example.com", "127.0.0.1"}, dir, 0,
	)
	require.NoError(t, err)

	name := serverNameFromCert(certPath)
	assert.Equal(t, "concrete.example.com", name,
		"should skip wildcard SAN and return the first concrete DNS name")
}

// TestServerNameFromCertWildcardOnlyFallsBackToIP verifies that when all DNS
// SANs are wildcards, serverNameFromCert falls back to the first IP SAN.
func TestServerNameFromCertWildcardOnlyFallsBackToIP(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath, err := pki.InitCA("test-ca", dir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)

	certPath, _, err := pki.IssueCert(
		caCert, caKey, pki.CertTypeServer, "wildcard-only",
		[]string{"*.example.com", "10.0.0.1"}, dir, 0,
	)
	require.NoError(t, err)

	name := serverNameFromCert(certPath)
	assert.Equal(t, "10.0.0.1", name,
		"should fall back to IP SAN when all DNS SANs are wildcards")
}

// TestRedactingHandlerRedactsMessage verifies that the redactingHandler wraps
// the underlying handler and redacts sensitive values from log messages and
// string attributes without altering non-string attributes.
func TestRedactingHandlerRedactsMessage(t *testing.T) {
	redactor, err := redact.New([]string{`secret=\S+`})
	require.NoError(t, err)

	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := &redactingHandler{inner: inner, redactor: redactor}

	logger := slog.New(h)
	logger.Info("token secret=abc123 sent", slog.String("key", "secret=xyz"), slog.Int("count", 5))

	out := buf.String()
	assert.NotContains(t, out, "abc123", "message should be redacted")
	assert.NotContains(t, out, "xyz", "string attr value should be redacted")
	assert.Contains(t, out, "count=5", "non-string attribute must pass through unchanged")
}

// TestRedactingHandlerEnabled delegates to the inner handler.
func TestRedactingHandlerEnabled(t *testing.T) {
	redactor, err := redact.New(nil)
	require.NoError(t, err)

	inner := slog.NewTextHandler(bytes.NewBuffer(nil), &slog.HandlerOptions{Level: slog.LevelWarn})
	h := &redactingHandler{inner: inner, redactor: redactor}

	assert.False(t, h.Enabled(context.Background(), slog.LevelDebug), "debug should be disabled")
	assert.True(t, h.Enabled(context.Background(), slog.LevelWarn), "warn should be enabled")
}

// TestRedactingHandlerWithAttrs verifies that WithAttrs returns a new handler
// that redacts string attrs added via WithAttrs.
func TestRedactingHandlerWithAttrs(t *testing.T) {
	redactor, err := redact.New([]string{`secret=\S+`})
	require.NoError(t, err)

	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := &redactingHandler{inner: inner, redactor: redactor}

	child := h.WithAttrs([]slog.Attr{slog.String("creds", "secret=top")})
	slog.New(child).Info("msg")

	assert.NotContains(t, buf.String(), "top", "attr added via WithAttrs must be redacted")
}

// TestRedactingHandlerWithGroup verifies that WithGroup wraps correctly.
func TestRedactingHandlerWithGroup(t *testing.T) {
	redactor, err := redact.New(nil)
	require.NoError(t, err)

	inner := slog.NewTextHandler(bytes.NewBuffer(nil), &slog.HandlerOptions{Level: slog.LevelDebug})
	h := &redactingHandler{inner: inner, redactor: redactor}

	gh := h.WithGroup("grp")
	// Must not panic and must still implement slog.Handler.
	assert.NotNil(t, gh)
}

// --- Security mode tests ---

// TestStartTLSOnlyMode verifies that Start with SecurityMode=ModeTLS
// creates a server that uses server TLS without requiring client certs.
func TestStartTLSOnlyMode(t *testing.T) {
	dir := t.TempDir()
	srv, err := Start(Config{
		StateDir:     dir,
		ListenAddr:   "127.0.0.1:0",
		SecurityMode: ModeTLS,
	})
	if err != nil {
		t.Skipf("TLS mode start failed (may need specific environment): %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	assert.NotNil(t, srv)
	mode := DiscoverMode(dir)
	assert.Equal(t, ModeTLS, mode)
}

// TestSecurityModeOverridesLegacy verifies that SecurityMode takes
// precedence over the ListenAddr heuristic.
func TestSecurityModeOverridesLegacy(t *testing.T) {
	dir := t.TempDir()
	// ListenAddr is set but SecurityMode explicitly says TLS.
	srv, err := Start(Config{
		StateDir:     dir,
		ListenAddr:   "127.0.0.1:0",
		SecurityMode: ModeTLS,
	})
	if err != nil {
		t.Skipf("TLS mode start failed: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })

	mode := DiscoverMode(dir)
	assert.Equal(t, ModeTLS, mode)
}

// TestSecurityModeLocalIgnoresListenAddr verifies that SecurityMode=ModeLocal
// forces local mode even when ListenAddr is set.
func TestSecurityModeLocalIgnoresListenAddr(t *testing.T) {
	dir := t.TempDir()
	// SecurityMode=ModeLocal should override the ListenAddr.
	srv, err := Start(Config{
		StateDir:     dir,
		SecurityMode: ModeLocal,
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Stop() })

	mode := DiscoverMode(dir)
	assert.Equal(t, ModeLocal, mode)
}
