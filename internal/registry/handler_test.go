package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testHandler(t *testing.T) (*Handler, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	cfg := ServerConfig{
		AllowedIssuers: map[string][]string{
			"server-1": {"issuer-a", "issuer-b"},
			"server-2": {"issuer-a"},
		},
	}
	h := NewHandler(store, cfg, testLogger())
	return h, store
}

func makePEM(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func withMTLS(r *http.Request, cn string) {
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}},
		},
	}
}

func TestHandler_Health(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"ok"`)
}

func TestHandler_UploadKey_Success(t *testing.T) {
	h, store := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := makePEM(t, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/clients/test-issuer/jwt-public-key", strings.NewReader(body))
	withMTLS(req, "test-issuer")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	// Verify key was stored
	got, err := store.GetKey(req.Context(), "test-issuer")
	require.NoError(t, err)
	assert.Equal(t, pub, got)
}

func TestHandler_UploadKey_CNMismatch(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := makePEM(t, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/clients/test-issuer/jwt-public-key", strings.NewReader(body))
	withMTLS(req, "wrong-cn")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandler_UploadKey_NoMTLS(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := makePEM(t, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/clients/test-issuer/jwt-public-key", strings.NewReader(body))
	// No TLS set
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandler_UploadKey_InvalidKey(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/clients/test-issuer/jwt-public-key", strings.NewReader("not a pem"))
	withMTLS(req, "test-issuer")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandler_UploadKey_InvalidIssuer(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/clients/bad%20issuer%21/jwt-public-key", strings.NewReader(""))
	withMTLS(req, "bad issuer!")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandler_GetJWKS_Success(t *testing.T) {
	h, store := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	pub3, _, _ := ed25519.GenerateKey(rand.Reader)

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	require.NoError(t, store.PutKey(ctx, "issuer-a", pub1))
	require.NoError(t, store.PutKey(ctx, "issuer-b", pub2))
	require.NoError(t, store.PutKey(ctx, "issuer-c", pub3)) // not allowed for server-1

	req := httptest.NewRequest(http.MethodGet, "/v1/servers/server-1/jwks.json", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var jwks JWKS
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &jwks))
	assert.Len(t, jwks.Keys, 2, "should only include issuer-a and issuer-b")

	kids := map[string]bool{}
	for _, k := range jwks.Keys {
		kids[k.Kid] = true
		assert.Equal(t, "OKP", k.Kty)
		assert.Equal(t, "Ed25519", k.Crv)
		assert.Equal(t, "sig", k.Use)
	}
	assert.True(t, kids["issuer-a"])
	assert.True(t, kids["issuer-b"])
	assert.False(t, kids["issuer-c"])
}

func TestHandler_GetJWKS_UnknownServer(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/servers/unknown/jwks.json", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandler_GetJWKS_Empty(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/servers/server-1/jwks.json", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var jwks JWKS
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &jwks))
	assert.Empty(t, jwks.Keys)
}

func TestValidateEd25519PublicKey_Valid(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pemStr := makePEM(t, pub)

	got, err := validateEd25519PublicKey([]byte(pemStr))
	require.NoError(t, err)
	assert.Equal(t, pub, got)
}

func TestValidateEd25519PublicKey_NoPEM(t *testing.T) {
	_, err := validateEd25519PublicKey([]byte("not pem data"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no PEM block")
}

func TestValidateEd25519PublicKey_WrongPEMType(t *testing.T) {
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: []byte("fake")})
	_, err := validateEd25519PublicKey(block)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected PEM type")
}
