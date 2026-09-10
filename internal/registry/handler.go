package registry

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

const (
	// MaxKeyUploadBytes limits the size of a public key upload body.
	MaxKeyUploadBytes = 4096
)

// issuerPattern validates issuer names: alphanumeric, hyphens, dots, underscores.
var issuerPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ServerConfig maps server IDs to their allowed issuers.
type ServerConfig struct {
	AllowedIssuers map[string][]string // server_id -> []issuer
}

// Handler implements the HTTP handlers for the key registry.
type Handler struct {
	store        Store
	serverConfig ServerConfig
	logger       *slog.Logger
}

// NewHandler creates a new Handler with the given store and config.
func NewHandler(store Store, cfg ServerConfig, logger *slog.Logger) *Handler {
	return &Handler{
		store:        store,
		serverConfig: cfg,
		logger:       logger,
	}
}

// RegisterRoutes registers all handler routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/clients/{issuer}/jwt-public-key", h.UploadKey)
	mux.HandleFunc("GET /v1/servers/{server_id}/jwks.json", h.GetJWKS)
	mux.HandleFunc("GET /healthz", h.Health)
}

// Health returns a simple health check response.
func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// UploadKey handles POST /v1/clients/{issuer}/jwt-public-key.
// It requires mTLS — the client certificate CN must match the issuer path param.
func (h *Handler) UploadKey(w http.ResponseWriter, r *http.Request) {
	issuer := r.PathValue("issuer")
	if issuer == "" || !issuerPattern.MatchString(issuer) {
		h.writeError(w, http.StatusBadRequest, "invalid issuer name")
		return
	}

	// Verify mTLS client cert CN matches issuer
	if err := h.verifyCertCN(r, issuer); err != nil {
		h.logger.Warn("cert CN mismatch", "issuer", issuer, "error", err)
		h.writeError(w, http.StatusForbidden, err.Error())
		return
	}

	// Read and validate the public key — enforce a hard body size limit at the HTTP layer
	r.Body = http.MaxBytesReader(w, r.Body, MaxKeyUploadBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	pubKey, err := validateEd25519PublicKey(body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid public key: %v", err))
		return
	}

	if err := h.store.PutKey(r.Context(), issuer, pubKey); err != nil {
		h.logger.Error("failed to store key", "issuer", issuer, "error", err)
		h.writeError(w, http.StatusInternalServerError, "failed to store key")
		return
	}

	h.logger.Info("uploaded public key", "issuer", issuer)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"status":"created"}`))
}

// GetJWKS handles GET /v1/servers/{server_id}/jwks.json.
// Returns only the keys for issuers allowed on the given server.
func (h *Handler) GetJWKS(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("server_id")
	if serverID == "" {
		h.writeError(w, http.StatusBadRequest, "missing server_id")
		return
	}

	allowed, ok := h.serverConfig.AllowedIssuers[serverID]
	if !ok {
		h.writeError(w, http.StatusNotFound, "unknown server")
		return
	}

	allKeys, err := h.store.ListKeys(r.Context())
	if err != nil {
		h.logger.Error("failed to list keys", "error", err)
		h.writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}

	// Filter to only allowed issuers for this server (case-insensitive to
	// match the case-insensitive CN check used during upload authorization).
	allowedSet := make(map[string]bool, len(allowed))
	for _, iss := range allowed {
		allowedSet[strings.ToLower(iss)] = true
	}

	var filtered []ClientKey
	for _, ck := range allKeys {
		if allowedSet[strings.ToLower(ck.Issuer)] {
			filtered = append(filtered, ck)
		}
	}

	jwks := ClientKeysToJWKS(filtered)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(jwks)
}

// verifyCertCN checks that the TLS peer certificate CN matches the expected issuer.
func (h *Handler) verifyCertCN(r *http.Request, expectedIssuer string) error {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return fmt.Errorf("mTLS required: no client certificate presented")
	}

	cn := r.TLS.PeerCertificates[0].Subject.CommonName
	if !strings.EqualFold(cn, expectedIssuer) {
		return fmt.Errorf("cert CN %q does not match issuer %q", cn, expectedIssuer)
	}

	return nil
}

// validateEd25519PublicKey parses PEM-encoded data and verifies it is an Ed25519 public key.
func validateEd25519PublicKey(data []byte) (ed25519.PublicKey, error) {
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
		return nil, fmt.Errorf("key is not Ed25519 (got %T)", key)
	}

	return edKey, nil
}

func (h *Handler) writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := map[string]string{"error": msg}
	_ = json.NewEncoder(w).Encode(resp)
}
