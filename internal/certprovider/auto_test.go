package certprovider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAutoProvider_Enroll_Server(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAutoProvider(dir, "test-auto-ca")
	if err != nil {
		t.Fatalf("NewAutoProvider: %v", err)
	}

	id, err := p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "bridge.local",
		SANs:       []string{"bridge.local", "127.0.0.1"},
		Role:       RoleServer,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if id.Provider != "auto" {
		t.Errorf("Provider = %q, want %q", id.Provider, "auto")
	}
	if id.CommonName != "bridge.local" {
		t.Errorf("CommonName = %q, want %q", id.CommonName, "bridge.local")
	}
	if !id.Renewable {
		t.Error("auto identity should be renewable")
	}
	if id.NotAfter.IsZero() {
		t.Error("NotAfter should not be zero")
	}

	// Verify the CA was created.
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Errorf("CA cert not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil {
		t.Errorf("CA key not created: %v", err)
	}
}

func TestAutoProvider_Enroll_Client(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAutoProvider(dir, "test-auto-ca")
	if err != nil {
		t.Fatalf("NewAutoProvider: %v", err)
	}

	id, err := p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "test-client",
		Role:       RoleClient,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if id.CommonName != "test-client" {
		t.Errorf("CommonName = %q, want %q", id.CommonName, "test-client")
	}
}

func TestAutoProvider_Enroll_CreatesCALazily(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAutoProvider(dir, "lazy-ca")
	if err != nil {
		t.Fatalf("NewAutoProvider: %v", err)
	}

	// CA should not exist yet.
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err == nil {
		t.Fatal("CA cert should not exist before first Enroll")
	}

	_, err = p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "server",
		Role:       RoleServer,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	// CA should now exist.
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Errorf("CA cert should exist after Enroll: %v", err)
	}
}

func TestAutoProvider_Enroll_ReusesExistingCA(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAutoProvider(dir, "reuse-ca")
	if err != nil {
		t.Fatalf("NewAutoProvider: %v", err)
	}

	// First enrollment creates the CA.
	_, err = p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "server1",
		Role:       RoleServer,
	})
	if err != nil {
		t.Fatalf("first Enroll: %v", err)
	}

	caInfo1, _ := os.Stat(filepath.Join(dir, "ca.crt"))

	// Second enrollment should reuse the same CA (same mod time).
	_, err = p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "server2",
		Role:       RoleServer,
		OutDir:     filepath.Join(dir, "second"),
	})
	if err != nil {
		t.Fatalf("second Enroll: %v", err)
	}

	caInfo2, _ := os.Stat(filepath.Join(dir, "ca.crt"))
	if !caInfo1.ModTime().Equal(caInfo2.ModTime()) {
		t.Error("CA cert was regenerated; should have been reused")
	}
}

func TestAutoProvider_Renew(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAutoProvider(dir, "renew-ca")
	if err != nil {
		t.Fatalf("NewAutoProvider: %v", err)
	}

	id, err := p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "bridge.local",
		SANs:       []string{"bridge.local"},
		Role:       RoleServer,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	origBefore := id.NotBefore
	origCertPath := id.CertPath

	err = p.Renew(context.Background(), id)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}

	// The renewed cert should have a new NotBefore (equal or later).
	if id.NotBefore.Before(origBefore) {
		t.Errorf("renewed NotBefore %v should not be before original %v", id.NotBefore, origBefore)
	}
	// The cert path may be the same file (re-issued in same dir).
	if id.CertPath == "" {
		t.Error("renewed CertPath should not be empty")
	}
	_ = origCertPath
}

func TestAutoProvider_Roots(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAutoProvider(dir, "roots-ca")
	if err != nil {
		t.Fatalf("NewAutoProvider: %v", err)
	}

	// Need to create the CA first.
	_, err = p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "server",
		Role:       RoleServer,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	roots, err := p.Roots(context.Background())
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("Roots returned %d certs, want 1", len(roots))
	}
	if roots[0].Subject.CommonName != "roots-ca" {
		t.Errorf("Root CN = %q, want %q", roots[0].Subject.CommonName, "roots-ca")
	}
}

func TestAutoProvider_EmptyCertsDir(t *testing.T) {
	_, err := NewAutoProvider("", "test")
	if err == nil {
		t.Error("expected error for empty certs dir")
	}
}

func TestAutoProvider_DefaultCAName(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAutoProvider(dir, "")
	if err != nil {
		t.Fatalf("NewAutoProvider: %v", err)
	}

	// The default name should be used.
	_, err = p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "server",
		Role:       RoleServer,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	roots, err := p.Roots(context.Background())
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if roots[0].Subject.CommonName != "bridgectl-auto-ca" {
		t.Errorf("default CA name = %q, want %q", roots[0].Subject.CommonName, "bridgectl-auto-ca")
	}
}

func TestAutoProvider_CAKeyPath(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewAutoProvider(dir, "test-ca")
	got := p.CAKeyPath()
	if got != filepath.Join(dir, "ca.key") {
		t.Errorf("CAKeyPath = %q, want %q", got, filepath.Join(dir, "ca.key"))
	}
}

func TestAutoProvider_CACertPath(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewAutoProvider(dir, "test-ca")
	got := p.CACertPath()
	if got != filepath.Join(dir, "ca.crt") {
		t.Errorf("CACertPath = %q, want %q", got, filepath.Join(dir, "ca.crt"))
	}
}

func TestAutoProvider_Enroll_CustomOutDir(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "custom")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p, _ := NewAutoProvider(dir, "test-ca")

	id, err := p.Enroll(context.Background(), EnrollmentRequest{
		CommonName: "client",
		Role:       RoleClient,
		OutDir:     outDir,
	})
	if err != nil {
		t.Fatalf("Enroll with custom OutDir: %v", err)
	}
	if id.CertPath == "" {
		t.Error("CertPath should not be empty")
	}
}

func TestAutoProvider_Roots_NoCA(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewAutoProvider(dir, "test-ca")

	// No CA exists yet; Roots should return an error.
	_, err := p.Roots(context.Background())
	if err == nil {
		t.Error("expected error from Roots when CA does not exist")
	}
}

func TestAutoProvider_Renew_MissingCert(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewAutoProvider(dir, "test-ca")

	// Renew with a non-existent cert path.
	id := &Identity{CertPath: filepath.Join(dir, "nonexistent.crt")}
	err := p.Renew(context.Background(), id)
	if err == nil {
		t.Error("expected error from Renew when cert does not exist")
	}
}
