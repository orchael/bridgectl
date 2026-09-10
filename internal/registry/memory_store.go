package registry

import (
	"context"
	"crypto/ed25519"
	"sync"
)

// MemoryStore is an in-memory implementation of Store for testing and
// non-Kubernetes environments.
type MemoryStore struct {
	mu   sync.RWMutex
	keys map[string]ed25519.PublicKey
}

// NewMemoryStore creates a new in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		keys: make(map[string]ed25519.PublicKey),
	}
}

// PutKey stores or replaces the public key for the given issuer.
func (s *MemoryStore) PutKey(_ context.Context, issuer string, pubKey ed25519.PublicKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[issuer] = pubKey
	return nil
}

// GetKey retrieves the public key for the given issuer.
func (s *MemoryStore) GetKey(_ context.Context, issuer string) (ed25519.PublicKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[issuer]
	if !ok {
		return nil, nil
	}
	return key, nil
}

// ListKeys returns all registered client keys.
func (s *MemoryStore) ListKeys(_ context.Context) ([]ClientKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ClientKey, 0, len(s.keys))
	for issuer, key := range s.keys {
		result = append(result, ClientKey{Issuer: issuer, PublicKey: key})
	}
	return result, nil
}

// DeleteKey removes the key for the given issuer.
func (s *MemoryStore) DeleteKey(_ context.Context, issuer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, issuer)
	return nil
}
