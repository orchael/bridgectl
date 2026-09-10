package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testK8sLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestK8sStore_PutAndGetKey(t *testing.T) {
	dir := t.TempDir()
	store, err := NewK8sStore(dir, testK8sLogger())
	require.NoError(t, err)

	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)

	require.NoError(t, store.PutKey(ctx, "test-issuer", pub))

	got, err := store.GetKey(ctx, "test-issuer")
	require.NoError(t, err)
	assert.Equal(t, pub, got)
}

func TestK8sStore_GetKey_NotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewK8sStore(dir, testK8sLogger())
	require.NoError(t, err)

	got, err := store.GetKey(context.Background(), "nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestK8sStore_ListKeys(t *testing.T) {
	dir := t.TempDir()
	store, err := NewK8sStore(dir, testK8sLogger())
	require.NoError(t, err)

	ctx := context.Background()
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)

	require.NoError(t, store.PutKey(ctx, "issuer-a", pub1))
	require.NoError(t, store.PutKey(ctx, "issuer-b", pub2))

	keys, err := store.ListKeys(ctx)
	require.NoError(t, err)
	assert.Len(t, keys, 2)

	issuers := map[string]bool{}
	for _, k := range keys {
		issuers[k.Issuer] = true
	}
	assert.True(t, issuers["issuer-a"])
	assert.True(t, issuers["issuer-b"])
}

func TestK8sStore_DeleteKey(t *testing.T) {
	dir := t.TempDir()
	store, err := NewK8sStore(dir, testK8sLogger())
	require.NoError(t, err)

	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)

	require.NoError(t, store.PutKey(ctx, "del-me", pub))
	require.NoError(t, store.DeleteKey(ctx, "del-me"))

	got, err := store.GetKey(ctx, "del-me")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestK8sStore_PutKey_Replaces(t *testing.T) {
	dir := t.TempDir()
	store, err := NewK8sStore(dir, testK8sLogger())
	require.NoError(t, err)

	ctx := context.Background()
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)

	require.NoError(t, store.PutKey(ctx, "issuer", pub1))
	require.NoError(t, store.PutKey(ctx, "issuer", pub2))

	got, err := store.GetKey(ctx, "issuer")
	require.NoError(t, err)
	assert.Equal(t, pub2, got)
}
