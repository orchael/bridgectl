package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEd25519ToJWK(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	jwk := Ed25519ToJWK(pub, "test-kid")

	assert.Equal(t, "OKP", jwk.Kty)
	assert.Equal(t, "Ed25519", jwk.Crv)
	assert.Equal(t, "test-kid", jwk.Kid)
	assert.Equal(t, "sig", jwk.Use)

	// Verify X is the raw-URL-encoded public key bytes
	decoded, err := base64.RawURLEncoding.DecodeString(jwk.X)
	require.NoError(t, err)
	assert.Equal(t, []byte(pub), decoded)
}

func TestClientKeysToJWKS_Empty(t *testing.T) {
	jwks := ClientKeysToJWKS(nil)
	assert.NotNil(t, jwks.Keys)
	assert.Empty(t, jwks.Keys)
}

func TestClientKeysToJWKS_Multiple(t *testing.T) {
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)

	keys := []ClientKey{
		{Issuer: "issuer-a", PublicKey: pub1},
		{Issuer: "issuer-b", PublicKey: pub2},
	}

	jwks := ClientKeysToJWKS(keys)
	assert.Len(t, jwks.Keys, 2)
	assert.Equal(t, "issuer-a", jwks.Keys[0].Kid)
	assert.Equal(t, "issuer-b", jwks.Keys[1].Kid)
}
