package registry

import (
	"crypto/ed25519"
	"encoding/base64"
)

// JWK represents a single JSON Web Key (RFC 7517).
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Use string `json:"use"`
}

// JWKS represents a JSON Web Key Set (RFC 7517).
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// Ed25519ToJWK converts an Ed25519 public key and issuer ID to a JWK.
func Ed25519ToJWK(pubKey ed25519.PublicKey, kid string) JWK {
	return JWK{
		Kty: "OKP",
		Crv: "Ed25519",
		X:   base64.RawURLEncoding.EncodeToString(pubKey),
		Kid: kid,
		Use: "sig",
	}
}

// ClientKeysToJWKS converts a slice of ClientKey to a JWKS.
func ClientKeysToJWKS(keys []ClientKey) JWKS {
	jwks := JWKS{Keys: make([]JWK, 0, len(keys))}
	for _, ck := range keys {
		jwks.Keys = append(jwks.Keys, Ed25519ToJWK(ck.PublicKey, ck.Issuer))
	}
	return jwks
}
