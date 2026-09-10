package registry

import (
	"context"
	"crypto/ed25519"
)

// ClientKey holds a registered client's public key and metadata.
type ClientKey struct {
	Issuer    string
	PublicKey ed25519.PublicKey
}

// Store defines the interface for persisting and retrieving client public keys.
type Store interface {
	// PutKey stores or replaces the public key for the given issuer.
	PutKey(ctx context.Context, issuer string, pubKey ed25519.PublicKey) error

	// GetKey retrieves the public key for the given issuer.
	// Returns nil, nil if the issuer is not found.
	GetKey(ctx context.Context, issuer string) (ed25519.PublicKey, error)

	// ListKeys returns all registered client keys.
	ListKeys(ctx context.Context) ([]ClientKey, error)

	// DeleteKey removes the key for the given issuer.
	DeleteKey(ctx context.Context, issuer string) error
}
