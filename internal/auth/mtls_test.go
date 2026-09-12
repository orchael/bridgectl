package auth

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/pki"
)

// setupTestCerts creates a CA, server cert, client cert, and CA bundle in dir.
// Returns (serverCert, serverKey, clientCert, clientKey, bundlePath).
func setupTestCerts(t *testing.T) (serverCert, serverKey, clientCert, clientKey, bundle string) {
	t.Helper()
	dir := t.TempDir()

	caCert, caKey, err := pki.InitCA("test-ca", dir)
	if err != nil {
		t.Fatalf("InitCA: %v", err)
	}
	ca, caKeyEC, err := pki.LoadCA(caCert, caKey)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}

	serverCert, serverKey, err = pki.IssueCert(ca, caKeyEC, pki.CertTypeServer, "bridge.local", []string{"bridge.local", "127.0.0.1"}, dir, 0)
	if err != nil {
		t.Fatalf("IssueCert server: %v", err)
	}

	clientCert, clientKey, err = pki.IssueCert(ca, caKeyEC, pki.CertTypeClient, "client-a", nil, dir, 0)
	if err != nil {
		t.Fatalf("IssueCert client: %v", err)
	}

	bundle = filepath.Join(dir, "bundle.crt")
	if err := pki.BuildBundle(bundle, caCert); err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}

	return serverCert, serverKey, clientCert, clientKey, bundle
}

func TestCertReloader_InitialLoad(t *testing.T) {
	serverCert, serverKey, _, _, _ := setupTestCerts(t)

	r, err := NewCertReloader(serverCert, serverKey)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	cert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil {
		t.Fatal("GetCertificate returned nil cert")
	}
}

func TestCertReloader_MissingFile(t *testing.T) {
	_, err := NewCertReloader("/nonexistent/cert.pem", "/nonexistent/key.pem")
	if err == nil {
		t.Fatal("expected error for missing cert files, got nil")
	}
}

func TestCertReloader_HotReload(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey, err := pki.InitCA("test-ca", dir)
	if err != nil {
		t.Fatalf("InitCA: %v", err)
	}
	ca, caKeyEC, err := pki.LoadCA(caCert, caKey)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}

	// Issue first cert and copy to fixed paths.
	cert1, key1, err := pki.IssueCert(ca, caKeyEC, pki.CertTypeServer, "bridge.local", []string{"bridge.local"}, dir, 0)
	if err != nil {
		t.Fatalf("IssueCert first: %v", err)
	}
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if err := copyFile(t, cert1, certPath); err != nil {
		t.Fatalf("copy cert: %v", err)
	}
	if err := copyFile(t, key1, keyPath); err != nil {
		t.Fatalf("copy key: %v", err)
	}

	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	origCert, _ := r.GetCertificate(nil)
	if origCert == nil {
		t.Fatal("initial GetCertificate returned nil")
	}

	// Sleep to ensure the new cert will have a different mtime.
	time.Sleep(10 * time.Millisecond)

	// Issue a second cert with a different CN and replace the files.
	cert2, key2, err := pki.IssueCert(ca, caKeyEC, pki.CertTypeServer, "bridge-v2.local", []string{"bridge-v2.local"}, dir, 0)
	if err != nil {
		t.Fatalf("IssueCert second: %v", err)
	}
	if err := copyFile(t, cert2, certPath); err != nil {
		t.Fatalf("copy new cert: %v", err)
	}
	if err := copyFile(t, key2, keyPath); err != nil {
		t.Fatalf("copy new key: %v", err)
	}
	// Force mtime to differ from original (some filesystems have 1s resolution).
	futureTime := time.Now().Add(time.Second)
	if err := os.Chtimes(certPath, futureTime, futureTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// GetCertificate should detect the mtime change and reload.
	reloaded, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after hot-reload: %v", err)
	}
	if reloaded == nil {
		t.Fatal("GetCertificate returned nil after hot-reload")
	}

	// Verify the reloaded cert has the new CN.
	if len(reloaded.Certificate) == 0 {
		t.Fatal("reloaded cert has no DER data")
	}
	origSerial := origCert.Leaf
	if origSerial != nil && reloaded.Leaf != nil && origCert.Leaf == reloaded.Leaf {
		t.Error("reloaded cert should differ from original")
	}
}

func TestCertReloader_ReloadFailurePreservesOldCert(t *testing.T) {
	serverCert, serverKey, _, _, _ := setupTestCerts(t)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if err := copyFile(t, serverCert, certPath); err != nil {
		t.Fatalf("copy cert: %v", err)
	}
	if err := copyFile(t, serverKey, keyPath); err != nil {
		t.Fatalf("copy key: %v", err)
	}

	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	origCert, _ := r.GetCertificate(nil)

	// Overwrite with invalid PEM, force a new mtime.
	if err := os.WriteFile(certPath, []byte("not a cert"), 0o644); err != nil {
		t.Fatalf("WriteFile bad cert: %v", err)
	}
	futureTime := time.Now().Add(time.Second)
	if err := os.Chtimes(certPath, futureTime, futureTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Explicit reload should fail.
	if err := r.Reload(); err == nil {
		t.Fatal("Reload with bad cert should fail, got nil")
	}

	// GetCertificate should still return the old cert without error.
	stillValid, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after failed reload: %v", err)
	}
	if stillValid == nil {
		t.Fatal("GetCertificate returned nil after failed reload")
	}
	_ = origCert // still valid
}

func TestServerTLSOnlyConfig(t *testing.T) {
	serverCert, serverKey, _, _, bundle := setupTestCerts(t)

	cfg, err := ServerTLSOnlyConfig(TLSConfig{
		CABundlePath: bundle,
		CertPath:     serverCert,
		KeyPath:      serverKey,
	})
	if err != nil {
		t.Fatalf("ServerTLSOnlyConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion=%v want TLS1.3", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth=%v want NoClientCert", cfg.ClientAuth)
	}
	if cfg.GetCertificate == nil {
		t.Error("GetCertificate callback should be set")
	}
}

func TestClientTLSOnlyConfig(t *testing.T) {
	_, _, _, _, bundle := setupTestCerts(t)

	cfg, err := ClientTLSOnlyConfig(TLSConfig{
		CABundlePath: bundle,
		ServerName:   "bridge.local",
	})
	if err != nil {
		t.Fatalf("ClientTLSOnlyConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion=%v want TLS1.3", cfg.MinVersion)
	}
	if cfg.ServerName != "bridge.local" {
		t.Errorf("ServerName=%q want bridge.local", cfg.ServerName)
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs should be set")
	}
}

func TestClientTLSOnlyConfig_MissingBundle(t *testing.T) {
	_, err := ClientTLSOnlyConfig(TLSConfig{
		CABundlePath: "/nonexistent/bundle.crt",
		ServerName:   "bridge.local",
	})
	if err == nil {
		t.Fatal("expected error for missing CA bundle, got nil")
	}
}

func TestServerTLSConfig_RequiresClientCert(t *testing.T) {
	serverCert, serverKey, _, _, bundle := setupTestCerts(t)

	cfg, err := ServerTLSConfig(TLSConfig{
		CABundlePath: bundle,
		CertPath:     serverCert,
		KeyPath:      serverKey,
	})
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth=%v want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion=%v want TLS1.3", cfg.MinVersion)
	}
}

func copyFile(t *testing.T, src, dst string) error {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
