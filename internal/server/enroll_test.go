package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/auth"
)

// mtlsContext returns a context with a fake mTLS peer certificate.
func mtlsContext(cn string) context.Context {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.IPAddr{},
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{cert},
			},
		},
	})
}

// testEnrollServer creates a BridgeServer with a JWTVerifier and temp certsDir.
func testEnrollServer(t *testing.T) (*BridgeServer, string) {
	t.Helper()
	certsDir := t.TempDir()
	verifier := &auth.JWTVerifier{
		Keys:     make(map[string]ed25519.PublicKey),
		Audience: "bridge",
	}
	s := New(nil, nil, nil, RateLimitConfig{
		GlobalRPS:   100,
		GlobalBurst: 100,
	}, "test", nil, verifier, certsDir, "")
	return s, certsDir
}

// genTestPubKeyDER generates an Ed25519 keypair and returns the DER-encoded public key.
func genTestPubKeyDER(t *testing.T) ([]byte, ed25519.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return der, pub
}

func TestRegisterJWTKey(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		s, certsDir := testEnrollServer(t)
		pubDER, _ := genTestPubKeyDER(t)

		resp, err := s.RegisterJWTKey(mtlsContext("test-client"), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: pubDER,
			Issuer:    "test-client",
		})
		require.NoError(t, err)
		assert.Equal(t, "test-client", resp.Issuer)
		assert.Contains(t, resp.Message, "test-client")

		// Key was persisted to disk.
		_, statErr := os.Stat(filepath.Join(certsDir, "jwt-clients", "test-client.pub"))
		assert.NoError(t, statErr)

		// Key was hot-added to verifier.
		assert.True(t, s.jwtVerifier.HasKey("test-client"))
	})

	t.Run("issuer defaults to mTLS CN", func(t *testing.T) {
		s, _ := testEnrollServer(t)
		pubDER, _ := genTestPubKeyDER(t)

		resp, err := s.RegisterJWTKey(mtlsContext("my-machine"), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: pubDER,
		})
		require.NoError(t, err)
		assert.Equal(t, "my-machine", resp.Issuer)
		assert.True(t, s.jwtVerifier.HasKey("my-machine"))
	})

	t.Run("no mTLS cert", func(t *testing.T) {
		s, _ := testEnrollServer(t)
		pubDER, _ := genTestPubKeyDER(t)

		_, err := s.RegisterJWTKey(context.Background(), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: pubDER,
			Issuer:    "test",
		})
		require.Error(t, err)
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("empty public key", func(t *testing.T) {
		s, _ := testEnrollServer(t)

		_, err := s.RegisterJWTKey(mtlsContext("test-client"), &bridgev1.RegisterJWTKeyRequest{
			Issuer: "test-client",
		})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("invalid public key bytes", func(t *testing.T) {
		s, _ := testEnrollServer(t)

		_, err := s.RegisterJWTKey(mtlsContext("test-client"), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: []byte("not a valid key"),
			Issuer:    "test-client",
		})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("invalid issuer name", func(t *testing.T) {
		s, _ := testEnrollServer(t)
		pubDER, _ := genTestPubKeyDER(t)

		_, err := s.RegisterJWTKey(mtlsContext("test-client"), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: pubDER,
			Issuer:    "../traversal",
		})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("nil verifier (local mode)", func(t *testing.T) {
		s := New(nil, nil, nil, RateLimitConfig{
			GlobalRPS:   100,
			GlobalBurst: 100,
		}, "test", nil, nil, "", "")
		pubDER, _ := genTestPubKeyDER(t)

		_, err := s.RegisterJWTKey(mtlsContext("test-client"), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: pubDER,
			Issuer:    "test-client",
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("key rotation allowed", func(t *testing.T) {
		// A client may re-register its own issuer slot (issuer == CN) with a
		// different key, enabling self-service key rotation.
		s, _ := testEnrollServer(t)
		pubDER1, _ := genTestPubKeyDER(t)
		pubDER2, _ := genTestPubKeyDER(t)

		_, err := s.RegisterJWTKey(mtlsContext("test-client"), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: pubDER1,
			Issuer:    "test-client",
		})
		require.NoError(t, err)

		// Second registration with a different key and the same CN/issuer should succeed.
		_, err = s.RegisterJWTKey(mtlsContext("test-client"), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: pubDER2,
			Issuer:    "test-client",
		})
		require.NoError(t, err)
		assert.True(t, s.jwtVerifier.HasKey("test-client"))
	})

	t.Run("cross-issuer overwrite rejected", func(t *testing.T) {
		// A client must not be able to claim or overwrite a foreign issuer slot.
		s, _ := testEnrollServer(t)
		pubDER, _ := genTestPubKeyDER(t)

		_, err := s.RegisterJWTKey(mtlsContext("test-client"), &bridgev1.RegisterJWTKeyRequest{
			PublicKey: pubDER,
			Issuer:    "other-client",
		})
		require.Error(t, err)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	})
}
