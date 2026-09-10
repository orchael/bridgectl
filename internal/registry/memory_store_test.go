package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func generateTestKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub
}

func TestMemoryStore_PutAndGetKey(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	pub := generateTestKey(t)

	err := store.PutKey(ctx, "test-issuer", pub)
	require.NoError(t, err)

	got, err := store.GetKey(ctx, "test-issuer")
	require.NoError(t, err)
	assert.Equal(t, pub, got)
}

func TestMemoryStore_GetKey_NotFound(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	got, err := store.GetKey(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMemoryStore_PutKey_Replaces(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	pub1 := generateTestKey(t)
	pub2 := generateTestKey(t)

	require.NoError(t, store.PutKey(ctx, "issuer", pub1))
	require.NoError(t, store.PutKey(ctx, "issuer", pub2))

	got, err := store.GetKey(ctx, "issuer")
	require.NoError(t, err)
	assert.Equal(t, pub2, got)
}

func TestMemoryStore_ListKeys(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	pub1 := generateTestKey(t)
	pub2 := generateTestKey(t)

	require.NoError(t, store.PutKey(ctx, "issuer-a", pub1))
	require.NoError(t, store.PutKey(ctx, "issuer-b", pub2))

	keys, err := store.ListKeys(ctx)
	require.NoError(t, err)
	assert.Len(t, keys, 2)

	issuerSet := make(map[string]bool)
	for _, k := range keys {
		issuerSet[k.Issuer] = true
	}
	assert.True(t, issuerSet["issuer-a"])
	assert.True(t, issuerSet["issuer-b"])
}

func TestMemoryStore_DeleteKey(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	pub := generateTestKey(t)

	require.NoError(t, store.PutKey(ctx, "del-me", pub))
	require.NoError(t, store.DeleteKey(ctx, "del-me"))

	got, err := store.GetKey(ctx, "del-me")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMemoryStore_DeleteKey_Nonexistent(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	err := store.DeleteKey(ctx, "nope")
	assert.NoError(t, err)
}
