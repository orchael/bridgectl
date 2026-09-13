package certprovider

import (
	"path/filepath"
	"testing"
)

func TestNew_Filesystem(t *testing.T) {
	caCert, cert, key := setupTestPKI(t)

	p, err := New("filesystem", ProviderConfig{
		Filesystem: &FilesystemConfig{
			CACertPath: caCert,
			CertPath:   cert,
			KeyPath:    key,
		},
	})
	if err != nil {
		t.Fatalf("New(filesystem): %v", err)
	}
	if p == nil {
		t.Fatal("provider is nil")
	}
}

func TestNew_FilesystemMissingConfig(t *testing.T) {
	_, err := New("filesystem", ProviderConfig{})
	if err == nil {
		t.Error("expected error for missing filesystem config")
	}
}

func TestNew_Auto(t *testing.T) {
	dir := t.TempDir()
	p, err := New("auto", ProviderConfig{
		AutoCertsDir: filepath.Join(dir, "certs"),
		AutoCAName:   "test-ca",
	})
	if err != nil {
		t.Fatalf("New(auto): %v", err)
	}
	if p == nil {
		t.Fatal("provider is nil")
	}
}

func TestNew_StepCAMissingConfig(t *testing.T) {
	_, err := New("stepca", ProviderConfig{})
	if err == nil {
		t.Error("expected error for missing stepca config")
	}
}

func TestNew_Unknown(t *testing.T) {
	_, err := New("unknown-provider", ProviderConfig{})
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}
