package localserver

import (
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/orchael/bridgectl/internal/pki"
)

// fakeStepDir writes a stub `step` binary into a temp directory and returns
// a cleanup function. The fake step writes empty placeholder files to the cert
// and key path arguments (positions 3 and 4 of `step ca certificate ...`).
func fakeStepDir(t *testing.T) (dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake step script not supported on Windows")
	}
	dir = t.TempDir()
	script := `#!/bin/sh
# Fake step CLI: write placeholder cert/key for "step ca certificate <name> <cert> <key> ..."
if [ "$1" = "ca" ] && [ "$2" = "certificate" ]; then
  echo "fake-cert" > "$4"
  echo "fake-key"  > "$5"
fi
exit 0
`
	scriptPath := filepath.Join(dir, "step")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestEnsurePKI(t *testing.T) {
	stateDir := t.TempDir()
	sans := []string{"10.0.0.1", "bridge.local"}

	mat, err := EnsurePKI(stateDir, sans, testLogger(), nil, 0)
	require.NoError(t, err)

	// Verify all files exist.
	for _, path := range []string{
		mat.CACertPath,
		mat.CAKeyPath,
		mat.ServerCertPath,
		mat.ServerKeyPath,
		mat.LocalClientCert,
		mat.LocalClientKey,
		mat.CABundlePath,
		mat.JWTSigningKey,
		mat.JWTSigningPub,
	} {
		_, err := os.Stat(path)
		assert.NoError(t, err, "file should exist: %s", path)
	}

	// Verify private keys have restricted permissions.
	for _, path := range []string{mat.CAKeyPath, mat.ServerKeyPath, mat.LocalClientKey, mat.JWTSigningKey} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		perm := info.Mode().Perm()
		assert.Equal(t, os.FileMode(0o600), perm, "private key %s should be 0600", path)
	}

	// Verify server cert has the expected SANs.
	serverCert, err := pki.LoadCert(mat.ServerCertPath)
	require.NoError(t, err)
	assert.Contains(t, serverCert.DNSNames, "bridge.local")
	foundIP := false
	for _, ip := range serverCert.IPAddresses {
		if ip.String() == "10.0.0.1" {
			foundIP = true
		}
	}
	assert.True(t, foundIP, "server cert should have IP SAN 10.0.0.1")

	// Verify server cert is signed by the CA.
	caCert, _, err := pki.LoadCA(mat.CACertPath, mat.CAKeyPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	_, err = serverCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	assert.NoError(t, err, "server cert should verify against CA")

	// Verify local-client cert is signed by the CA.
	clientCert, err := pki.LoadCert(mat.LocalClientCert)
	require.NoError(t, err)
	_, err = clientCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	assert.NoError(t, err, "client cert should verify against CA")

	// Verify JWT keypair loads successfully.
	_, err = pki.LoadEd25519PublicKey(mat.JWTSigningPub)
	assert.NoError(t, err, "JWT public key should load")
	_, err = pki.LoadEd25519PrivateKey(mat.JWTSigningKey)
	assert.NoError(t, err, "JWT private key should load")
}

func TestEnsurePKI_Idempotent(t *testing.T) {
	stateDir := t.TempDir()
	sans := []string{"10.0.0.1"}

	mat1, err := EnsurePKI(stateDir, sans, testLogger(), nil, 0)
	require.NoError(t, err)

	// Read the CA cert bytes from first run.
	ca1, err := os.ReadFile(mat1.CACertPath)
	require.NoError(t, err)

	// Second call should be a no-op.
	mat2, err := EnsurePKI(stateDir, sans, testLogger(), nil, 0)
	require.NoError(t, err)

	// CA cert should be identical (not regenerated).
	ca2, err := os.ReadFile(mat2.CACertPath)
	require.NoError(t, err)
	assert.Equal(t, ca1, ca2, "CA cert should not be regenerated on second call")
}

func TestEnsureLocalManagementPKIHandlesStateDirCABundle(t *testing.T) {
	stateDir := t.TempDir()
	certsDir := CertsDir(stateDir)
	require.NoError(t, os.MkdirAll(certsDir, 0o700))

	externalDir := filepath.Join(stateDir, "external")
	externalCAPath, _, err := pki.InitCA("external", externalDir)
	require.NoError(t, err)
	externalCA, err := os.ReadFile(externalCAPath)
	require.NoError(t, err)

	bundlePath := filepath.Join(certsDir, "ca-bundle.crt")
	require.NoError(t, os.WriteFile(bundlePath, externalCA, 0o644))

	mat, err := EnsureLocalManagementPKI(stateDir, bundlePath, testLogger())
	require.NoError(t, err)
	require.Equal(t, bundlePath, mat.CABundlePath)

	firstBundle, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	require.Contains(t, string(firstBundle), strings.TrimSpace(string(externalCA)))
	localCA, err := os.ReadFile(mat.CACertPath)
	require.NoError(t, err)
	localCAPEM := strings.TrimSpace(string(localCA))
	require.Equal(t, 1, strings.Count(string(firstBundle), localCAPEM))

	_, err = EnsureLocalManagementPKI(stateDir, bundlePath, testLogger())
	require.NoError(t, err)
	secondBundle, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	require.Contains(t, string(secondBundle), strings.TrimSpace(string(externalCA)))
	require.Equal(t, 1, strings.Count(string(secondBundle), localCAPEM))
}

func TestIssueClientCert(t *testing.T) {
	stateDir := t.TempDir()
	logger := testLogger()

	// First generate the CA via EnsurePKI.
	mat, err := EnsurePKI(stateDir, []string{"127.0.0.1"}, logger, nil, 0)
	require.NoError(t, err)

	// Issue a client cert.
	certPath, keyPath, err := IssueClientCert(stateDir, "remote-dev", logger)
	require.NoError(t, err)

	// Verify files exist in the expected location.
	expectedDir := filepath.Join(CertsDir(stateDir), "clients", "remote-dev")
	assert.Equal(t, filepath.Join(expectedDir, "remote-dev.crt"), certPath)
	assert.Equal(t, filepath.Join(expectedDir, "remote-dev.key"), keyPath)

	_, err = os.Stat(certPath)
	assert.NoError(t, err)
	_, err = os.Stat(keyPath)
	assert.NoError(t, err)

	// Verify the cert validates against the CA.
	caCert, _, err := pki.LoadCA(mat.CACertPath, mat.CAKeyPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	clientCert, err := pki.LoadCert(certPath)
	require.NoError(t, err)
	_, err = clientCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	assert.NoError(t, err, "issued client cert should verify against CA")
	assert.Equal(t, "remote-dev", clientCert.Subject.CommonName)

	// Verify per-client JWT keypair was created.
	clientJWTKey := filepath.Join(expectedDir, "jwt-signing.key")
	clientJWTPub := filepath.Join(expectedDir, "jwt-signing.pub")
	_, err = os.Stat(clientJWTKey)
	assert.NoError(t, err, "per-client JWT key should exist")
	_, err = os.Stat(clientJWTPub)
	assert.NoError(t, err, "per-client JWT pub should exist")

	// Verify server-side copy of the public key.
	serverPubCopy := filepath.Join(CertsDir(stateDir), "jwt-clients", "remote-dev.pub")
	_, err = os.Stat(serverPubCopy)
	assert.NoError(t, err, "server-side JWT pub copy should exist")
}

func TestIssueClientCert_RejectsPathTraversal(t *testing.T) {
	stateDir := t.TempDir()
	logger := testLogger()

	// Generate PKI first.
	_, err := EnsurePKI(stateDir, []string{"127.0.0.1"}, logger, nil, 0)
	require.NoError(t, err)

	badNames := []string{"../escape", "foo/bar", ".hidden", "", "a b c"}
	for _, name := range badNames {
		_, _, err := IssueClientCert(stateDir, name, logger)
		assert.Error(t, err, "should reject client name %q", name)
	}

	// Valid names should work.
	goodNames := []string{"laptop2", "dev-machine", "server.local", "test_01"}
	for _, name := range goodNames {
		_, _, err := IssueClientCert(stateDir, name, logger)
		assert.NoError(t, err, "should accept client name %q", name)
	}
}

// TestEnsurePKI_StepCASkipsAutoGen verifies that when a StepCAConfig is
// supplied and the native cert request fails, EnsurePKI does not generate a
// self-signed CA and instead returns a clear error.
func TestEnsurePKI_StepCASkipsAutoGen(t *testing.T) {
	stateDir := t.TempDir()
	// Write a fake root cert so the RootPath check passes.
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("placeholder"), 0o644))

	// Override JWK function to simulate failure (no real CA available).
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, _ []string, _, _ string, _ *slog.Logger) error {
		return fmt.Errorf("connection refused")
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	stepCfg := &StepCAConfig{
		URL:      "https://ca.example.internal:443",
		RootPath: rootPEM,
	}
	_, err := EnsurePKI(stateDir, []string{"127.0.0.1"}, testLogger(), stepCfg, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Step CA")

	// The auto-generated CA must NOT have been created.
	mat := LoadPKIMaterial(stateDir)
	_, statErr := os.Stat(mat.CAKeyPath)
	assert.True(t, os.IsNotExist(statErr), "CA key should not be auto-generated in Step CA mode")
}

// TestEnsurePKI_StepCAMissingRoot verifies that StepCAConfig without RootPath
// returns an error before calling the `step` binary.
func TestEnsurePKI_StepCAMissingRoot(t *testing.T) {
	stateDir := t.TempDir()
	stepCfg := &StepCAConfig{URL: "https://ca.example.internal:443"}
	_, err := EnsurePKI(stateDir, []string{"127.0.0.1"}, testLogger(), stepCfg, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "step-ca-root")
}

// TestEnsurePKI_StepCAHappyPath exercises the Step CA code path using a mock
// cert requester so the happy path is covered without a real Step CA server.
func TestEnsurePKI_StepCAHappyPath(t *testing.T) {
	stateDir := t.TempDir()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root-cert"), 0o644))

	// Override JWK function to write placeholder cert/key files.
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, _ []string, certPath, keyPath string, _ *slog.Logger) error {
		require.NoError(t, os.WriteFile(certPath, []byte("FAKE-SERVER-CERT"), 0o644))
		require.NoError(t, os.WriteFile(keyPath, []byte("FAKE-SERVER-KEY"), 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	stepCfg := &StepCAConfig{
		URL:      "https://ca.example.internal:443",
		RootPath: rootPEM,
	}
	mat, err := EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)

	// ca-bundle.crt should start with the Step CA root, followed by the local CA.
	bundle, err := os.ReadFile(mat.CABundlePath)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(bundle), "fake-root-cert"), "bundle should start with Step CA root")
	assert.Contains(t, string(bundle), "BEGIN CERTIFICATE", "bundle should also contain local CA cert")

	// Server cert and key files should exist (written by the mock function).
	_, err = os.Stat(mat.ServerCertPath)
	assert.NoError(t, err, "server cert should exist")
	_, err = os.Stat(mat.ServerKeyPath)
	assert.NoError(t, err, "server key should exist")

	// Local-client cert and key must exist so CLI probing works in Step CA mode.
	_, err = os.Stat(mat.LocalClientCert)
	assert.NoError(t, err, "local-client cert should exist")
	_, err = os.Stat(mat.LocalClientKey)
	assert.NoError(t, err, "local-client key should exist")

	// JWT keypair should be auto-generated locally even in Step CA mode.
	_, err = os.Stat(mat.JWTSigningPub)
	assert.NoError(t, err, "JWT pub should exist")
	_, err = os.Stat(mat.JWTSigningKey)
	assert.NoError(t, err, "JWT key should exist")
}

// TestEnsurePKI_StepCARootUpdate verifies that a changed configured root is
// installed on restart instead of silently reusing the old trust bundle.
// The JWK stub writes fake (unparseable) certs, so ensureStepCACertFresh
// treats them as unreadable and re-requests — the stub handles that idempotently.
func TestEnsurePKI_StepCARootUpdate(t *testing.T) {
	stateDir := t.TempDir()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root-cert"), 0o644))

	// Override JWK function to write placeholder cert/key files.
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, _ []string, certPath, keyPath string, _ *slog.Logger) error {
		require.NoError(t, os.WriteFile(certPath, []byte("FAKE-SERVER-CERT"), 0o644))
		require.NoError(t, os.WriteFile(keyPath, []byte("FAKE-SERVER-KEY"), 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	// Stub mTLS so the fallback to JWK is exercised when the unreadable cert
	// triggers a renewal attempt on the second EnsurePKI call.
	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, _, _ string, _ *slog.Logger) error {
		return fmt.Errorf("cert unreadable")
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	pwFile := filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(pwFile, []byte("test"), 0o600))

	stepCfg := &StepCAConfig{
		URL:                     "https://ca.example.internal:443",
		RootPath:                rootPEM,
		ProvisionerPasswordFile: pwFile,
	}
	_, err := EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)

	// Overwrite root file with different content.
	require.NoError(t, os.WriteFile(rootPEM, []byte("changed-root"), 0o644))
	// Second call: a changed configured root must replace the old bundle.
	mat2, err := EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)
	bundle, _ := os.ReadFile(mat2.CABundlePath)
	assert.True(t, strings.HasPrefix(string(bundle), "changed-root"), "bundle should start with the newly configured root")
}

func TestEnsurePKI_StepCASwitch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		changeURL  bool
		changeRoot bool
		legacy     bool
	}{
		{name: "URL changes with same root", changeURL: true},
		{name: "root changes in legacy state", changeRoot: true, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			rootPath := filepath.Join(stateDir, "root.crt")
			require.NoError(t, os.WriteFile(rootPath, []byte("first-root"), 0o644))
			caCertPath, caKeyPath, err := pki.InitCA("test-ca", t.TempDir())
			require.NoError(t, err)
			caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
			require.NoError(t, err)

			issued := 0
			failIssue := false
			oldJWK := requestCertJWKFn
			requestCertJWKFn = func(_ *StepCAConfig, _ []string, certPath, keyPath string, _ *slog.Logger) error {
				issued++
				if failIssue {
					return fmt.Errorf("simulated CA failure")
				}
				srcCert, srcKey, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", []string{"10.0.0.1"}, t.TempDir(), 24*time.Hour)
				if err != nil {
					return err
				}
				certData, err := os.ReadFile(srcCert)
				if err != nil {
					return err
				}
				keyData, err := os.ReadFile(srcKey)
				if err != nil {
					return err
				}
				require.NoError(t, os.WriteFile(certPath, certData, 0o644))
				return os.WriteFile(keyPath, keyData, 0o600)
			}
			t.Cleanup(func() { requestCertJWKFn = oldJWK })

			cfg := &StepCAConfig{URL: "https://first.example", RootPath: rootPath}
			mat, err := EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), cfg, 0)
			require.NoError(t, err)
			require.Equal(t, 1, issued)
			_, err = EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), cfg, 0)
			require.NoError(t, err)
			require.Equal(t, 1, issued, "unchanged CA must reuse a fresh certificate")
			jwtBefore, err := os.ReadFile(mat.JWTSigningPub)
			require.NoError(t, err)
			clientBefore, err := os.ReadFile(mat.LocalClientCert)
			require.NoError(t, err)
			if tc.legacy {
				err := os.Remove(filepath.Join(CertsDir(stateDir), ".pki-step-ca-url"))
				require.True(t, err == nil || os.IsNotExist(err))
			}
			if tc.changeURL {
				cfg.URL = "https://second.example"
			}
			if tc.changeRoot {
				require.NoError(t, os.WriteFile(rootPath, []byte("second-root"), 0o644))
			}

			mat, err = EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), cfg, 0)
			require.NoError(t, err)
			require.Equal(t, 2, issued, "CA change must issue a new certificate")
			bundle, err := os.ReadFile(mat.CABundlePath)
			require.NoError(t, err)
			if tc.changeRoot {
				require.True(t, strings.HasPrefix(string(bundle), "second-root"))
			}
			jwtAfter, err := os.ReadFile(mat.JWTSigningPub)
			require.NoError(t, err)
			clientAfter, err := os.ReadFile(mat.LocalClientCert)
			require.NoError(t, err)
			require.Equal(t, jwtBefore, jwtAfter)
			require.Equal(t, clientBefore, clientAfter)

			certBefore, err := os.ReadFile(mat.ServerCertPath)
			require.NoError(t, err)
			bundleBefore := bundle
			failIssue = true
			cfg.URL = "https://unavailable.example"
			_, err = EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), cfg, 0)
			require.ErrorContains(t, err, "simulated CA failure")
			certAfter, err := os.ReadFile(mat.ServerCertPath)
			require.NoError(t, err)
			bundleAfter, err := os.ReadFile(mat.CABundlePath)
			require.NoError(t, err)
			require.Equal(t, certBefore, certAfter)
			require.Equal(t, bundleBefore, bundleAfter)
		})
	}
}

func TestStepCAChangedRejectsUnreadableState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, certsDir, rootPath, bundlePath string)
		wantError string
	}{
		{
			name: "missing configured root",
			setup: func(t *testing.T, _, _, bundlePath string) {
				require.NoError(t, os.WriteFile(bundlePath, []byte("old-root"), 0o644))
			},
			wantError: "read Step CA root",
		},
		{
			name: "missing saved bundle",
			setup: func(t *testing.T, _, rootPath, _ string) {
				require.NoError(t, os.WriteFile(rootPath, []byte("root"), 0o644))
			},
			wantError: "read Step CA trust bundle",
		},
		{
			name: "unreadable URL record",
			setup: func(t *testing.T, certsDir, rootPath, bundlePath string) {
				require.NoError(t, os.WriteFile(rootPath, []byte("root"), 0o644))
				require.NoError(t, os.WriteFile(bundlePath, []byte("root-and-local-ca"), 0o644))
				require.NoError(t, os.Mkdir(filepath.Join(certsDir, pkiStepCAURLFile), 0o700))
			},
			wantError: "read recorded Step CA URL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			certsDir := t.TempDir()
			rootPath := filepath.Join(certsDir, "root.crt")
			bundlePath := filepath.Join(certsDir, "ca-bundle.crt")
			tc.setup(t, certsDir, rootPath, bundlePath)
			_, err := stepCAChanged(certsDir, &PKIMaterial{CABundlePath: bundlePath}, &StepCAConfig{URL: "https://ca.example", RootPath: rootPath})
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}

// TestEnsurePKI_StepCAExpiredCertRenewsAtStartup verifies that EnsurePKI renews
// an expired Step CA certificate synchronously at startup instead of deferring
// renewal to the background loop.
func TestEnsurePKI_StepCAExpiredCertRenewsAtStartup(t *testing.T) {
	stateDir := t.TempDir()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root-cert"), 0o644))

	// Create a CA in a separate temp dir for issuing real certs.
	caDir := filepath.Join(t.TempDir(), "ca")
	caCertPath, caKeyPath, err := pki.InitCA("test-ca", caDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)

	// Issue an expired server cert in a scratch dir (not certsDir) so the
	// first EnsurePKI call doesn't overwrite it with stub content.
	scratchDir := filepath.Join(t.TempDir(), "scratch")
	expiredCertPath, expiredKeyPath, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", []string{"10.0.0.1"}, scratchDir, time.Nanosecond)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond) // let it expire
	expiredCertData, err := os.ReadFile(expiredCertPath)
	require.NoError(t, err)
	expiredKeyData, err := os.ReadFile(expiredKeyPath)
	require.NoError(t, err)

	jwkCalled := 0
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, sans []string, cp, kp string, _ *slog.Logger) error {
		jwkCalled++
		if jwkCalled == 1 {
			// First call (initial setup): write stub content.
			require.NoError(t, os.WriteFile(cp, []byte("STUB"), 0o644))
			require.NoError(t, os.WriteFile(kp, []byte("STUB"), 0o600))
			return nil
		}
		// Renewal call: issue a proper long-lived cert.
		freshCert, freshKey, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", sans, t.TempDir(), 24*time.Hour)
		if err != nil {
			return err
		}
		certData, err := os.ReadFile(freshCert)
		if err != nil {
			return fmt.Errorf("read fresh cert: %w", err)
		}
		keyData, err := os.ReadFile(freshKey)
		if err != nil {
			return fmt.Errorf("read fresh key: %w", err)
		}
		require.NoError(t, os.WriteFile(cp, certData, 0o644))
		require.NoError(t, os.WriteFile(kp, keyData, 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, _, _ string, _ *slog.Logger) error {
		return fmt.Errorf("cert expired")
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	stepCfg := &StepCAConfig{
		URL:                     "https://ca.example.internal:443",
		RootPath:                rootPEM,
		ProvisionerPasswordFile: filepath.Join(t.TempDir(), "password"),
	}
	require.NoError(t, os.WriteFile(stepCfg.ProvisionerPasswordFile, []byte("test"), 0o600))

	// First call: sets up all PKI files (ca-bundle, local-client, JWT, server cert stub).
	_, err = EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)

	// Overwrite the server cert with the real expired cert so the second call
	// can parse it and detect expiry.
	certsDir := CertsDir(stateDir)
	require.NoError(t, os.WriteFile(filepath.Join(certsDir, "server.crt"), expiredCertData, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(certsDir, "server.key"), expiredKeyData, 0o600))

	// Second call: should detect the expired cert and renew at startup.
	mat2, err := EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)

	assert.Equal(t, 2, jwkCalled, "JWK provisioner should have been called twice: initial setup + renewal")

	// Verify the cert was renewed (should now be valid for >23 hours).
	_, notAfter, err := ServerCertExpiry(mat2.ServerCertPath)
	require.NoError(t, err)
	assert.True(t, time.Until(notAfter) > 23*time.Hour, "cert should have been renewed at startup, got remaining=%v", time.Until(notAfter))
}

// TestEnsurePKI_AutoGenExpiredCertRenewsAtStartup verifies that EnsurePKI
// renews an expired auto-generated server certificate synchronously at
// startup instead of reusing it indefinitely (issue #225).
func TestEnsurePKI_AutoGenExpiredCertRenewsAtStartup(t *testing.T) {
	stateDir := t.TempDir()
	sans := []string{"10.0.0.1"}

	// First call generates the CA and a (default ~90-day) server cert.
	mat, err := EnsurePKI(stateDir, sans, testLogger(), nil, 0)
	require.NoError(t, err)

	// Mint an already-expired server cert signed by the SAME CA and drop it
	// in place of the fresh one, simulating a daemon that was last started
	// long enough ago for the cert to have expired.
	caCert, caKey, err := pki.LoadCA(mat.CACertPath, mat.CAKeyPath)
	require.NoError(t, err)
	expiredCertPath, expiredKeyPath, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", sans, filepath.Join(t.TempDir(), "scratch"), time.Nanosecond)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond) // let it expire
	expiredCertData, err := os.ReadFile(expiredCertPath)
	require.NoError(t, err)
	expiredKeyData, err := os.ReadFile(expiredKeyPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(mat.ServerCertPath, expiredCertData, 0o644))
	require.NoError(t, os.WriteFile(mat.ServerKeyPath, expiredKeyData, 0o600))

	// Second call should detect the expired cert and renew it at startup.
	mat2, err := EnsurePKI(stateDir, sans, testLogger(), nil, 0)
	require.NoError(t, err)

	_, notAfter, err := ServerCertExpiry(mat2.ServerCertPath)
	require.NoError(t, err)
	assert.True(t, time.Until(notAfter) > 24*time.Hour, "cert should have been renewed at startup with the default ~90-day validity, got remaining=%v", time.Until(notAfter))
}

// TestEnsurePKI_AutoGenSANMismatchRenewsAtStartup verifies that restarting
// with a different --san value regenerates the auto-generated server cert
// instead of reusing one that no longer covers the requested names
// (issue #224).
func TestEnsurePKI_AutoGenSANMismatchRenewsAtStartup(t *testing.T) {
	stateDir := t.TempDir()

	mat, err := EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), nil, 0)
	require.NoError(t, err)
	cert1, err := pki.LoadCert(mat.ServerCertPath)
	require.NoError(t, err)
	require.Len(t, cert1.IPAddresses, 1)
	assert.Equal(t, "10.0.0.1", cert1.IPAddresses[0].String())

	// Restart with a different --san value.
	mat2, err := EnsurePKI(stateDir, []string{"10.0.0.2"}, testLogger(), nil, 0)
	require.NoError(t, err)
	cert2, err := pki.LoadCert(mat2.ServerCertPath)
	require.NoError(t, err)
	require.Len(t, cert2.IPAddresses, 1)
	assert.Equal(t, "10.0.0.2", cert2.IPAddresses[0].String(), "cert should be regenerated to cover the new SAN")
	assert.NotEqual(t, cert1.SerialNumber, cert2.SerialNumber, "cert should have been reissued, not reused")
}

// TestEnsurePKI_StepCASANChangeRenewsAtStartup is the Step CA-mode
// counterpart of TestEnsurePKI_AutoGenSANMismatchRenewsAtStartup: restarting
// with a different --san value must renew the cert even though the
// previous one has not expired (issue #224). The comparison is against the
// SANs bridgectl itself recorded as requested on the prior call (see
// pkiRequestedSANsChanged), not against the issued cert's actual content —
// see TestEnsurePKI_ACMEKeepsServerSAN and
// TestDiscoverTargetSecureModeServerNameFromCert for why comparing against
// the cert's own content would be unsafe (an external CA/provisioner can
// legitimately narrow the issued SAN set).
func TestEnsurePKI_StepCASANChangeRenewsAtStartup(t *testing.T) {
	stateDir := t.TempDir()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root-cert"), 0o644))

	caDir := filepath.Join(t.TempDir(), "ca")
	caCertPath, caKeyPath, err := pki.InitCA("test-ca", caDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)

	jwkCalled := 0
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, sans []string, certPath, keyPath string, _ *slog.Logger) error {
		jwkCalled++
		freshCert, freshKey, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", sans, t.TempDir(), 24*time.Hour)
		if err != nil {
			return err
		}
		certData, err := os.ReadFile(freshCert)
		if err != nil {
			return err
		}
		keyData, err := os.ReadFile(freshKey)
		if err != nil {
			return err
		}
		require.NoError(t, os.WriteFile(certPath, certData, 0o644))
		require.NoError(t, os.WriteFile(keyPath, keyData, 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	pwFile := filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(pwFile, []byte("test"), 0o600))
	stepCfg := &StepCAConfig{
		URL:                     "https://ca.example.internal:443",
		RootPath:                rootPEM,
		ProvisionerPasswordFile: pwFile,
	}

	mat, err := EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)
	require.Equal(t, 1, jwkCalled, "first call should issue once")
	cert1, err := pki.LoadCert(mat.ServerCertPath)
	require.NoError(t, err)
	require.Len(t, cert1.IPAddresses, 1)
	assert.Equal(t, "10.0.0.1", cert1.IPAddresses[0].String())

	// Restart with a different --san value; the cert is still fresh
	// (24h validity, just issued) so only the recorded-SAN check should
	// trigger renewal.
	mat2, err := EnsurePKI(stateDir, []string{"10.0.0.2"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, jwkCalled, "a recorded --san change must trigger re-issuance against the Step CA")
	cert2, err := pki.LoadCert(mat2.ServerCertPath)
	require.NoError(t, err)
	require.Len(t, cert2.IPAddresses, 1)
	assert.Equal(t, "10.0.0.2", cert2.IPAddresses[0].String(), "cert should have been renewed to cover the new SAN")
}

// TestEnsurePKI_StepCAUnchangedSANsDoesNotForceRenewal verifies that when
// the requested SANs have not changed between starts, a fresh Step CA
// cert is left untouched even if its actual issued SANs happen to differ
// from what was requested (e.g. an ACME provisioner trimming them) —
// pkiRequestedSANsChanged only compares against bridgectl's own recorded
// request, never against the cert's own content.
func TestEnsurePKI_StepCAUnchangedSANsDoesNotForceRenewal(t *testing.T) {
	stateDir := t.TempDir()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root-cert"), 0o644))

	caDir := filepath.Join(t.TempDir(), "ca")
	caCertPath, caKeyPath, err := pki.InitCA("test-ca", caDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)

	jwkCalled := 0
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, _ []string, certPath, keyPath string, _ *slog.Logger) error {
		jwkCalled++
		// Issue a cert whose actual SANs ("trimmed.example.com") differ from
		// what was requested, simulating a provisioner that narrows the set.
		freshCert, freshKey, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", []string{"trimmed.example.com"}, t.TempDir(), 24*time.Hour)
		if err != nil {
			return err
		}
		certData, err := os.ReadFile(freshCert)
		if err != nil {
			return err
		}
		keyData, err := os.ReadFile(freshKey)
		if err != nil {
			return err
		}
		require.NoError(t, os.WriteFile(certPath, certData, 0o644))
		require.NoError(t, os.WriteFile(keyPath, keyData, 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	pwFile := filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(pwFile, []byte("test"), 0o600))
	stepCfg := &StepCAConfig{
		URL:                     "https://ca.example.internal:443",
		RootPath:                rootPEM,
		ProvisionerPasswordFile: pwFile,
	}

	_, err = EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)
	require.Equal(t, 1, jwkCalled)

	// Restart with the SAME requested SAN. Even though the issued cert's
	// actual content ("trimmed.example.com") differs, nothing should renew.
	_, err = EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, jwkCalled, "unchanged requested SANs must not force renewal even if the issued cert's own SANs differ")
}

// TestEnsurePKI_AutoGenMigratesFromCertWhenNoSANsRecorded is a regression
// test for a gap flagged in review: an auto-gen certsDir from a bridgectl
// version that predates .pki-sans tracking has no record file at all. A
// changed --san value must still be detected on that first post-upgrade
// start by falling back to the existing certificate's actual SAN content
// (safe for auto-gen only — see pkiRequestedSANsChanged) rather than
// silently recording the new request as already satisfied (issue #224).
func TestEnsurePKI_AutoGenMigratesFromCertWhenNoSANsRecorded(t *testing.T) {
	stateDir := t.TempDir()
	certsDir := CertsDir(stateDir)
	require.NoError(t, os.MkdirAll(certsDir, 0o700))

	caCertPath, caKeyPath, err := pki.InitCA("bridgectl", certsDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)

	_, _, err = pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", []string{"10.0.0.1"}, certsDir, 0)
	require.NoError(t, err)
	require.NoError(t, pki.BuildBundle(filepath.Join(certsDir, "ca-bundle.crt"), caCertPath))
	require.NoError(t, writePKIMode(certsDir, pkiModeAuto))
	// Deliberately no .pki-sans file, simulating state from before this
	// tracking existed.

	mat, err := EnsurePKI(stateDir, []string{"10.0.0.2"}, testLogger(), nil, 0)
	require.NoError(t, err)

	cert, err := pki.LoadCert(mat.ServerCertPath)
	require.NoError(t, err)
	require.Len(t, cert.IPAddresses, 1)
	assert.Equal(t, "10.0.0.2", cert.IPAddresses[0].String(), "cert should be reissued to cover the new SAN even with no prior .pki-sans record")

	recorded, ok := readPKISANs(certsDir)
	require.True(t, ok, ".pki-sans should now be recorded")
	assert.Equal(t, []string{"10.0.0.2"}, recorded)
}

// TestEnsurePKI_StepCASANChangeSkipsMTLSEvenWhenItWouldSucceed is a
// regression test for a gap flagged in review: a SAN change must bypass
// Step CA's mTLS /renew path entirely, even when that path would otherwise
// succeed, because mTLS renewal preserves the existing certificate's SANs
// rather than accepting new ones (issue #224). Without this, a successful
// mTLS renewal would silently keep the old SANs while .pki-sans recorded
// the new request as already satisfied.
func TestEnsurePKI_StepCASANChangeSkipsMTLSEvenWhenItWouldSucceed(t *testing.T) {
	stateDir := t.TempDir()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root-cert"), 0o644))

	caDir := filepath.Join(t.TempDir(), "ca")
	caCertPath, caKeyPath, err := pki.InitCA("test-ca", caDir)
	require.NoError(t, err)
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	require.NoError(t, err)

	mtlsCalled := 0
	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, _, _ string, _ *slog.Logger) error {
		mtlsCalled++
		// Pretend mTLS renewal succeeded. If a SAN change ever routed
		// through this path, the cert on disk would be left with its old
		// SANs since mTLS /renew preserves identity.
		return nil
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	jwkCalled := 0
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, sans []string, certPath, keyPath string, _ *slog.Logger) error {
		jwkCalled++
		freshCert, freshKey, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", sans, t.TempDir(), 24*time.Hour)
		if err != nil {
			return err
		}
		certData, err := os.ReadFile(freshCert)
		if err != nil {
			return err
		}
		keyData, err := os.ReadFile(freshKey)
		if err != nil {
			return err
		}
		require.NoError(t, os.WriteFile(certPath, certData, 0o644))
		require.NoError(t, os.WriteFile(keyPath, keyData, 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	pwFile := filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(pwFile, []byte("test"), 0o600))
	stepCfg := &StepCAConfig{
		URL:                     "https://ca.example.internal:443",
		RootPath:                rootPEM,
		ProvisionerPasswordFile: pwFile,
	}

	_, err = EnsurePKI(stateDir, []string{"10.0.0.1"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)
	require.Equal(t, 1, jwkCalled)
	require.Equal(t, 0, mtlsCalled)

	// Restart with a different --san value. mTLS must never be attempted
	// for this SAN-change path, even though the stub above would succeed.
	mat2, err := EnsurePKI(stateDir, []string{"10.0.0.2"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, mtlsCalled, "mTLS renewal must not be attempted for a SAN change")
	assert.Equal(t, 2, jwkCalled, "JWK issuance must be used directly for a SAN change")

	cert2, err := pki.LoadCert(mat2.ServerCertPath)
	require.NoError(t, err)
	require.Len(t, cert2.IPAddresses, 1)
	assert.Equal(t, "10.0.0.2", cert2.IPAddresses[0].String())
}

// TestIssueClientCertViaOIDC_HappyPath exercises the full OIDC enrollment path
// using a stub `step` binary.
func TestIssueClientCertViaOIDC_HappyPath(t *testing.T) {
	fakeStepDir(t)
	stateDir := t.TempDir()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root"), 0o644))

	stepCfg := &StepCAConfig{
		URL:             "https://ca.example.internal:443",
		RootPath:        rootPEM,
		OIDCProviderURL: "https://accounts.google.com",
	}
	logger := testLogger()
	certPath, keyPath, err := IssueClientCertViaOIDC(stateDir, "mark", stepCfg, logger)
	require.NoError(t, err)

	expectedDir := filepath.Join(CertsDir(stateDir), "clients", "mark")
	assert.Equal(t, filepath.Join(expectedDir, "mark.crt"), certPath)
	assert.Equal(t, filepath.Join(expectedDir, "mark.key"), keyPath)

	_, err = os.Stat(certPath)
	assert.NoError(t, err, "cert file should exist")
	_, err = os.Stat(keyPath)
	assert.NoError(t, err, "key file should exist")

	// JWT keypair should be generated locally.
	_, err = os.Stat(filepath.Join(expectedDir, "jwt-signing.key"))
	assert.NoError(t, err, "client JWT key should exist")
	// Server-side copy of the JWT public key.
	serverPub := filepath.Join(CertsDir(stateDir), "jwt-clients", "mark.pub")
	_, err = os.Stat(serverPub)
	assert.NoError(t, err, "server-side JWT pub copy should exist")
}

// TestCopyFile_ErrorOnMissingSource verifies that copyFile returns an error when
// the source file does not exist.
func TestCopyFile_ErrorOnMissingSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst.txt")
	err := copyFile("/nonexistent/source.txt", dst)
	require.Error(t, err)
}

// TestCopyFile_ErrorOnUnwritableDest verifies that copyFile returns an error
// when the destination directory does not exist.
func TestCopyFile_ErrorOnUnwritableDest(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o644))
	err := copyFile(src, "/nonexistent/dir/dst.txt")
	require.Error(t, err)
}

// TestIssueClientCertViaOIDC_RequiresStepBinary verifies that
// IssueClientCertViaOIDC returns a useful error when the `step` CLI is absent.
func TestIssueClientCertViaOIDC_RequiresStepBinary(t *testing.T) {
	if _, err := exec.LookPath("step"); err == nil {
		t.Skip("step binary is installed; cannot test missing-binary error")
	}
	stateDir := t.TempDir()
	logger := testLogger()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("placeholder"), 0o644))

	stepCfg := &StepCAConfig{
		URL:             "https://ca.example.internal:443",
		RootPath:        rootPEM,
		OIDCProviderURL: "https://accounts.google.com",
	}
	_, _, err := IssueClientCertViaOIDC(stateDir, "mark", stepCfg, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "step")
}

// TestIssueClientCertViaOIDC_Validation verifies that IssueClientCertViaOIDC
// validates its inputs before trying to run `step`.
func TestIssueClientCertViaOIDC_Validation(t *testing.T) {
	stateDir := t.TempDir()
	logger := testLogger()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("placeholder"), 0o644))

	tests := []struct {
		name    string
		client  string
		stepCA  *StepCAConfig
		wantErr string
	}{
		{
			name:    "invalid client name",
			client:  "../escape",
			stepCA:  &StepCAConfig{URL: "https://ca.example", RootPath: rootPEM, OIDCProviderURL: "https://idp"},
			wantErr: "invalid client name",
		},
		{
			name:    "missing step-ca-url",
			client:  "alice",
			stepCA:  nil,
			wantErr: "step-ca-url",
		},
		{
			name:    "missing oidc-provider",
			client:  "alice",
			stepCA:  &StepCAConfig{URL: "https://ca.example", RootPath: rootPEM},
			wantErr: "oidc-provider",
		},
		{
			name:    "missing step-ca-root",
			client:  "alice",
			stepCA:  &StepCAConfig{URL: "https://ca.example", OIDCProviderURL: "https://idp"},
			wantErr: "step-ca-root",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := IssueClientCertViaOIDC(stateDir, tc.client, tc.stepCA, logger)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestRenewServerCertStepCA_UsesMTLS verifies that renewServerCertStepCA calls
// renewCertMTLSFn (not requestCertJWKFn or requestCertACMEFn) so that renewal
// does not require the provisioner password or an ACME challenge.
func TestRenewServerCertStepCA_UsesMTLS(t *testing.T) {
	for _, provName := range []string{"", "bridge-jwk", "acme", "ACME"} {
		t.Run("provisioner="+provName, func(t *testing.T) {
			stateDir := t.TempDir()
			certsDir := filepath.Join(stateDir, "certs")
			require.NoError(t, os.MkdirAll(certsDir, 0o700))

			rootPath := filepath.Join(t.TempDir(), "root.crt")
			require.NoError(t, os.WriteFile(rootPath, []byte("FAKE-ROOT"), 0o644))

			// Write placeholder server cert/key that the mTLS renewer would read.
			serverCert := filepath.Join(certsDir, "server.crt")
			serverKey := filepath.Join(certsDir, "server.key")
			require.NoError(t, os.WriteFile(serverCert, []byte("OLD-CERT"), 0o644))
			require.NoError(t, os.WriteFile(serverKey, []byte("OLD-KEY"), 0o600))

			// Track which function was called.
			mtlsCalled := false
			oldMTLS := renewCertMTLSFn
			renewCertMTLSFn = func(_ *StepCAConfig, certPath, keyPath string, _ *slog.Logger) error {
				mtlsCalled = true
				require.NoError(t, os.WriteFile(certPath, []byte("RENEWED-CERT"), 0o644))
				return nil
			}
			t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

			// Ensure JWK and ACME functions are NOT called.
			oldJWK := requestCertJWKFn
			requestCertJWKFn = func(_ *StepCAConfig, _ []string, _, _ string, _ *slog.Logger) error {
				t.Fatal("requestCertJWKFn should NOT be called during renewal")
				return nil
			}
			t.Cleanup(func() { requestCertJWKFn = oldJWK })

			oldACME := requestCertACMEFn
			requestCertACMEFn = func(_ *StepCAConfig, _ []string, _, _ string, _ *slog.Logger) error {
				t.Fatal("requestCertACMEFn should NOT be called during renewal")
				return nil
			}
			t.Cleanup(func() { requestCertACMEFn = oldACME })

			mat := &PKIMaterial{
				ServerCertPath: serverCert,
				ServerKeyPath:  serverKey,
			}
			stepCfg := &StepCAConfig{
				URL:         "https://ca.example.com",
				RootPath:    rootPath,
				Provisioner: provName,
			}

			err := renewServerCertStepCA(mat, []string{"server", "10.0.0.1"}, testLogger(), stepCfg)
			require.NoError(t, err)
			assert.True(t, mtlsCalled, "renewCertMTLSFn should have been called")

			// Verify the cert was overwritten.
			data, err := os.ReadFile(serverCert)
			require.NoError(t, err)
			assert.Equal(t, "RENEWED-CERT", string(data))
		})
	}
}

// TestRenewServerCertStepCA_MTLSError_FallsBackToJWK verifies that when mTLS
// renewal fails (e.g. expired cert), the renewal falls back to the JWK
// provisioner and succeeds.
func TestRenewServerCertStepCA_MTLSError_FallsBackToJWK(t *testing.T) {
	stateDir := t.TempDir()
	certsDir := filepath.Join(stateDir, "certs")
	require.NoError(t, os.MkdirAll(certsDir, 0o700))

	rootPath := filepath.Join(t.TempDir(), "root.crt")
	require.NoError(t, os.WriteFile(rootPath, []byte("FAKE-ROOT"), 0o644))

	serverCert := filepath.Join(certsDir, "server.crt")
	serverKey := filepath.Join(certsDir, "server.key")
	require.NoError(t, os.WriteFile(serverCert, []byte("OLD-CERT"), 0o644))
	require.NoError(t, os.WriteFile(serverKey, []byte("OLD-KEY"), 0o600))

	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, _, _ string, _ *slog.Logger) error {
		return fmt.Errorf("missing client certificate")
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	jwkCalled := false
	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, _ []string, certPath, _ string, _ *slog.Logger) error {
		jwkCalled = true
		require.NoError(t, os.WriteFile(certPath, []byte("JWK-RENEWED-CERT"), 0o644))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	mat := &PKIMaterial{
		ServerCertPath: serverCert,
		ServerKeyPath:  serverKey,
	}
	stepCfg := &StepCAConfig{
		URL:                     "https://ca.example.com",
		RootPath:                rootPath,
		ProvisionerPasswordFile: filepath.Join(t.TempDir(), "password"),
	}

	err := renewServerCertStepCA(mat, []string{"server"}, testLogger(), stepCfg)
	require.NoError(t, err)
	assert.True(t, jwkCalled, "JWK fallback should have been called")

	data, err := os.ReadFile(serverCert)
	require.NoError(t, err)
	assert.Equal(t, "JWK-RENEWED-CERT", string(data))
}

func TestRenewServerCertStepCA_MTLSErrorWithoutPasswordFileFailsFast(t *testing.T) {
	stateDir := t.TempDir()
	certsDir := filepath.Join(stateDir, "certs")
	require.NoError(t, os.MkdirAll(certsDir, 0o700))

	rootPath := filepath.Join(t.TempDir(), "root.crt")
	require.NoError(t, os.WriteFile(rootPath, []byte("FAKE-ROOT"), 0o644))

	serverCert := filepath.Join(certsDir, "server.crt")
	serverKey := filepath.Join(certsDir, "server.key")
	require.NoError(t, os.WriteFile(serverCert, []byte("OLD-CERT"), 0o644))
	require.NoError(t, os.WriteFile(serverKey, []byte("OLD-KEY"), 0o600))

	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, _, _ string, _ *slog.Logger) error {
		return fmt.Errorf("certificate expired")
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, _ []string, _, _ string, _ *slog.Logger) error {
		t.Fatal("requestCertJWKFn should not be called without a password file")
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	mat := &PKIMaterial{
		ServerCertPath: serverCert,
		ServerKeyPath:  serverKey,
	}
	stepCfg := &StepCAConfig{
		URL:      "https://ca.example.com",
		RootPath: rootPath,
	}

	err := renewServerCertStepCA(mat, []string{"server"}, testLogger(), stepCfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provisioner_password_file")
}

// TestRenewServerCertStepCA_MTLSError_FallsBackToACME verifies that when mTLS
// renewal fails and the provisioner is ACME, the renewal falls back to ACME.
func TestRenewServerCertStepCA_MTLSError_FallsBackToACME(t *testing.T) {
	stateDir := t.TempDir()
	certsDir := filepath.Join(stateDir, "certs")
	require.NoError(t, os.MkdirAll(certsDir, 0o700))

	rootPath := filepath.Join(t.TempDir(), "root.crt")
	require.NoError(t, os.WriteFile(rootPath, []byte("FAKE-ROOT"), 0o644))

	serverCert := filepath.Join(certsDir, "server.crt")
	serverKey := filepath.Join(certsDir, "server.key")
	require.NoError(t, os.WriteFile(serverCert, []byte("OLD-CERT"), 0o644))
	require.NoError(t, os.WriteFile(serverKey, []byte("OLD-KEY"), 0o600))

	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, _, _ string, _ *slog.Logger) error {
		return fmt.Errorf("missing client certificate")
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	acmeCalled := false
	oldACME := requestCertACMEFn
	requestCertACMEFn = func(_ *StepCAConfig, sans []string, certPath, _ string, _ *slog.Logger) error {
		acmeCalled = true
		// Verify ACME SAN filtering was applied.
		assert.NotContains(t, sans, "localhost")
		assert.NotContains(t, sans, "127.0.0.1")
		assert.Contains(t, sans, "server")
		require.NoError(t, os.WriteFile(certPath, []byte("ACME-RENEWED-CERT"), 0o644))
		return nil
	}
	t.Cleanup(func() { requestCertACMEFn = oldACME })

	mat := &PKIMaterial{
		ServerCertPath: serverCert,
		ServerKeyPath:  serverKey,
	}
	stepCfg := &StepCAConfig{
		URL:         "https://ca.example.com",
		RootPath:    rootPath,
		Provisioner: "acme",
	}

	err := renewServerCertStepCA(mat, []string{"server", "localhost", "127.0.0.1"}, testLogger(), stepCfg)
	require.NoError(t, err)
	assert.True(t, acmeCalled, "ACME fallback should have been called")

	data, err := os.ReadFile(serverCert)
	require.NoError(t, err)
	assert.Equal(t, "ACME-RENEWED-CERT", string(data))
}

// TestRenewServerCertStepCA_MTLSAndFallbackBothFail verifies that when both
// mTLS renewal and the provisioner fallback fail, the error from the fallback
// is returned.
func TestRenewServerCertStepCA_MTLSAndFallbackBothFail(t *testing.T) {
	stateDir := t.TempDir()
	certsDir := filepath.Join(stateDir, "certs")
	require.NoError(t, os.MkdirAll(certsDir, 0o700))

	rootPath := filepath.Join(t.TempDir(), "root.crt")
	require.NoError(t, os.WriteFile(rootPath, []byte("FAKE-ROOT"), 0o644))

	serverCert := filepath.Join(certsDir, "server.crt")
	serverKey := filepath.Join(certsDir, "server.key")
	require.NoError(t, os.WriteFile(serverCert, []byte("OLD-CERT"), 0o644))
	require.NoError(t, os.WriteFile(serverKey, []byte("OLD-KEY"), 0o600))

	oldMTLS := renewCertMTLSFn
	renewCertMTLSFn = func(_ *StepCAConfig, _, _ string, _ *slog.Logger) error {
		return fmt.Errorf("missing client certificate")
	}
	t.Cleanup(func() { renewCertMTLSFn = oldMTLS })

	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, _ []string, _, _ string, _ *slog.Logger) error {
		return fmt.Errorf("provisioner password incorrect")
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	mat := &PKIMaterial{
		ServerCertPath: serverCert,
		ServerKeyPath:  serverKey,
	}
	stepCfg := &StepCAConfig{
		URL:                     "https://ca.example.com",
		RootPath:                rootPath,
		ProvisionerPasswordFile: filepath.Join(t.TempDir(), "password"),
	}

	err := renewServerCertStepCA(mat, []string{"server"}, testLogger(), stepCfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JWK fallback")
	assert.Contains(t, err.Error(), "provisioner password incorrect")
}

// TestEnsurePKI_ACMEKeepsServerSAN verifies that the ACME provisioner path does
// not strip the "server" SAN. Clients hard-code ServerName "server" for TLS
// verification, so it must be present in the issued certificate.
func TestEnsurePKI_ACMEKeepsServerSAN(t *testing.T) {
	stateDir := t.TempDir()
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root-cert"), 0o644))

	var capturedSANs []string
	oldACME := requestCertACMEFn
	requestCertACMEFn = func(_ *StepCAConfig, sans []string, certPath, keyPath string, _ *slog.Logger) error {
		capturedSANs = sans
		require.NoError(t, os.WriteFile(certPath, []byte("FAKE-SERVER-CERT"), 0o644))
		require.NoError(t, os.WriteFile(keyPath, []byte("FAKE-SERVER-KEY"), 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertACMEFn = oldACME })

	stepCfg := &StepCAConfig{
		URL:         "https://ca.example.internal:443",
		RootPath:    rootPEM,
		Provisioner: "acme",
	}
	_, err := EnsurePKI(stateDir, []string{"server", "localhost", "127.0.0.1", "bridge.local"}, testLogger(), stepCfg, 0)
	require.NoError(t, err)

	assert.Contains(t, capturedSANs, "server", "ACME path must keep 'server' SAN for client TLS verification")
	assert.Contains(t, capturedSANs, "bridge.local", "ACME path must keep non-loopback SANs")
	assert.NotContains(t, capturedSANs, "localhost", "ACME path should strip 'localhost'")
	assert.NotContains(t, capturedSANs, "127.0.0.1", "ACME path should strip '127.0.0.1'")
}

// TestEnsurePKI_ModeChangeTriggerRegeneration verifies that switching from
// auto-PKI to Step CA mode (or vice versa) forces PKI regeneration even when
// the cert files from the previous run still exist on disk.
func TestEnsurePKI_ModeChangeTriggerRegeneration(t *testing.T) {
	stateDir := t.TempDir()
	logger := testLogger()

	// First call: auto-PKI.
	_, err := EnsurePKI(stateDir, []string{"127.0.0.1"}, logger, nil, 0)
	require.NoError(t, err)

	// Capture the auto-PKI CA bundle content.
	certsDir := CertsDir(stateDir)
	autoBundle, err := os.ReadFile(filepath.Join(certsDir, "ca-bundle.crt"))
	require.NoError(t, err)
	assert.NotContains(t, string(autoBundle), "fake-root-cert")

	// Prepare Step CA mock.
	rootPEM := filepath.Join(stateDir, "root.crt")
	require.NoError(t, os.WriteFile(rootPEM, []byte("fake-root-cert"), 0o644))

	oldJWK := requestCertJWKFn
	requestCertJWKFn = func(_ *StepCAConfig, _ []string, certPath, keyPath string, _ *slog.Logger) error {
		require.NoError(t, os.WriteFile(certPath, []byte("STEP-SERVER-CERT"), 0o644))
		require.NoError(t, os.WriteFile(keyPath, []byte("STEP-SERVER-KEY"), 0o600))
		return nil
	}
	t.Cleanup(func() { requestCertJWKFn = oldJWK })

	stepCfg := &StepCAConfig{URL: "https://ca.example.internal:443", RootPath: rootPEM}

	// Second call: Step CA mode — must regenerate despite existing files.
	mat, err := EnsurePKI(stateDir, []string{"127.0.0.1"}, logger, stepCfg, 0)
	require.NoError(t, err)

	bundle, err := os.ReadFile(mat.CABundlePath)
	require.NoError(t, err)
	assert.Contains(t, string(bundle), "fake-root-cert", "bundle should now start with the Step CA root after mode switch")
}

func TestLoadPKIMaterial(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "test-state")
	mat := LoadPKIMaterial(stateDir)

	certs := filepath.Join(stateDir, "certs")
	assert.Equal(t, filepath.Join(certs, "ca.crt"), mat.CACertPath)
	assert.Equal(t, filepath.Join(certs, "server.crt"), mat.ServerCertPath)
	assert.Equal(t, filepath.Join(certs, "local-client.crt"), mat.LocalClientCert)
	assert.Equal(t, filepath.Join(certs, "jwt-signing.key"), mat.JWTSigningKey)
}

func TestBuildServerSANs(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		extra      []string
		wantHas    []string
		wantNot    []string
	}{
		{
			name:       "extracts host from addr",
			listenAddr: "10.0.0.1:9445",
			extra:      nil,
			wantHas:    []string{"10.0.0.1", "127.0.0.1", "localhost"},
		},
		{
			name:       "skips wildcard 0.0.0.0",
			listenAddr: "0.0.0.0:9445",
			extra:      []string{"vpn.example.com"},
			wantHas:    []string{"127.0.0.1", "localhost", "vpn.example.com"},
			wantNot:    []string{"0.0.0.0"},
		},
		{
			name:       "deduplicates",
			listenAddr: "10.0.0.1:9445",
			extra:      []string{"10.0.0.1", "extra.local"},
			wantHas:    []string{"10.0.0.1", "extra.local", "127.0.0.1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := BuildServerSANs(tc.listenAddr, tc.extra)
			for _, want := range tc.wantHas {
				assert.Contains(t, result, want)
			}
			for _, not := range tc.wantNot {
				assert.NotContains(t, result, not)
			}
		})
	}
}

// TestEffectiveAllowedPaths covers issue #238's "$HOME is always included in
// the effective allowed-path list" requirement: $HOME must be present with
// no configuration at all (the safe default), and must still be present
// alongside explicit configured entries (additive, not replaced).
func TestEffectiveAllowedPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	t.Run("no configured paths still includes HOME", func(t *testing.T) {
		got, err := EffectiveAllowedPaths(nil)
		require.NoError(t, err)
		assert.Equal(t, []string{home}, got)
	})

	t.Run("configured paths extend rather than replace HOME", func(t *testing.T) {
		got, err := EffectiveAllowedPaths([]string{"/srv/repos"})
		require.NoError(t, err)
		assert.Equal(t, []string{home, "/srv/repos"}, got)
	})
}

// TestEffectiveAllowedPathsFailsClosedWithoutHome covers the Copilot review
// finding on PR #279: when $HOME cannot be resolved, EffectiveAllowedPaths
// must error rather than silently returning an allow-list that, combined
// with no configured paths, would make bridge.Policy.ValidateRepoPath allow
// every path on the filesystem.
func TestEffectiveAllowedPathsFailsClosedWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	_, err := EffectiveAllowedPaths(nil)
	require.Error(t, err)
}

// TestKnownProviderIDs covers issue #238's provider-shortcut requirement:
// the built-in providers are all present, and the "codex-app-server"
// compatibility alias (which shares codex's binary) does not get its own
// duplicate shortcut.
func TestKnownProviderIDs(t *testing.T) {
	ids := KnownProviderIDs()
	for _, want := range []string{"claude", "codex", "opencode", "gemini"} {
		assert.Contains(t, ids, want)
	}
	assert.NotContains(t, ids, "codex-app-server")
}
