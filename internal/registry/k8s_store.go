package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// DefaultNamespace is the Kubernetes namespace for registry secrets.
	DefaultNamespace = "bridge-system"
	// SecretPrefix is the prefix for secret file names.
	SecretPrefix = "bridge-client-"
	// PublicKeyField is the key name within the secret data.
	PublicKeyField = "jwt-public-key.pem"
)

// K8sStore implements Store by reading and writing files in a directory
// structured as one subdirectory per issuer. In a Kubernetes deployment,
// a persistent volume or emptyDir backs the directory. Outside Kubernetes,
// it operates as a plain file-backed store.
type K8sStore struct {
	mu      sync.RWMutex
	baseDir string
	logger  *slog.Logger
}

// NewK8sStore creates a file-backed store at the given directory.
func NewK8sStore(baseDir string, logger *slog.Logger) (*K8sStore, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	return &K8sStore{
		baseDir: baseDir,
		logger:  logger,
	}, nil
}

func (s *K8sStore) issuerPath(issuer string) string {
	return filepath.Join(s.baseDir, SecretPrefix+issuer, PublicKeyField)
}

func (s *K8sStore) issuerDir(issuer string) string {
	return filepath.Join(s.baseDir, SecretPrefix+issuer)
}

// PutKey stores the public key as a PEM file under the issuer directory.
func (s *K8sStore) PutKey(_ context.Context, issuer string, pubKey ed25519.PublicKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.issuerDir(issuer)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create issuer dir: %w", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(pubKey)
	if err != nil {
		return fmt.Errorf("marshal public key: %w", err)
	}

	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	})

	path := s.issuerPath(issuer)
	if err := os.WriteFile(path, pemData, 0o644); err != nil {
		return fmt.Errorf("write key file: %w", err)
	}

	s.logger.Info("stored client public key", "issuer", issuer, "path", path)
	return nil
}

// GetKey reads the public key PEM for the given issuer.
func (s *K8sStore) GetKey(_ context.Context, issuer string) (ed25519.PublicKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.issuerPath(issuer))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read key file: %w", err)
	}

	return parseEd25519PEM(data)
}

// ListKeys enumerates all stored client keys.
func (s *K8sStore) ListKeys(_ context.Context) ([]ClientKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, fmt.Errorf("read store dir: %w", err)
	}

	var keys []ClientKey
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, SecretPrefix) {
			continue
		}
		issuer := strings.TrimPrefix(name, SecretPrefix)

		keyPath := filepath.Join(s.baseDir, name, PublicKeyField)
		data, err := os.ReadFile(keyPath)
		if err != nil {
			s.logger.Warn("skipping issuer with unreadable key", "issuer", issuer, "error", err)
			continue
		}

		pubKey, err := parseEd25519PEM(data)
		if err != nil {
			s.logger.Warn("skipping issuer with invalid key", "issuer", issuer, "error", err)
			continue
		}

		keys = append(keys, ClientKey{Issuer: issuer, PublicKey: pubKey})
	}

	return keys, nil
}

// DeleteKey removes the issuer directory and its key.
func (s *K8sStore) DeleteKey(_ context.Context, issuer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.issuerDir(issuer)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove issuer dir: %w", err)
	}
	s.logger.Info("deleted client public key", "issuer", issuer)
	return nil
}

func parseEd25519PEM(data []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("unexpected PEM type %q, expected PUBLIC KEY", block.Type)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	edKey, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key is not Ed25519")
	}
	return edKey, nil
}
