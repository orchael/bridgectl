package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestJWTMintAndVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	issuer := &JWTIssuer{
		Issuer:   "test-issuer",
		Audience: "bridge",
		Key:      priv,
		TTL:      5 * time.Minute,
	}

	token, err := issuer.Mint("user-1", "project-abc")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	verifier := &JWTVerifier{
		Audience: "bridge",
		MaxTTL:   10 * time.Minute,
		Keys: map[string]ed25519.PublicKey{
			"test-issuer": pub,
		},
	}

	claims, err := verifier.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if claims.ProjectID != "project-abc" {
		t.Errorf("ProjectID = %q, want %q", claims.ProjectID, "project-abc")
	}
	if claims.Subject != "user-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-1")
	}
}

func TestJWTWrongIssuer(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)

	issuer := &JWTIssuer{
		Issuer:   "evil-issuer",
		Audience: "bridge",
		Key:      priv,
		TTL:      5 * time.Minute,
	}
	token, _ := issuer.Mint("user-1", "project-abc")

	verifier := &JWTVerifier{
		Audience: "bridge",
		Keys: map[string]ed25519.PublicKey{
			"good-issuer": pub2,
		},
	}

	_, err := verifier.Verify(token)
	if err == nil {
		t.Error("expected error for unknown issuer")
	}
}

func TestJWTExceedsMaxTTL(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	issuer := &JWTIssuer{
		Issuer:   "test",
		Audience: "bridge",
		Key:      priv,
		TTL:      1 * time.Hour,
	}
	token, _ := issuer.Mint("user-1", "project-abc")

	verifier := &JWTVerifier{
		Audience: "bridge",
		MaxTTL:   5 * time.Minute,
		Keys:     map[string]ed25519.PublicKey{"test": pub},
	}

	_, err := verifier.Verify(token)
	if err == nil {
		t.Error("expected error for TTL exceeding max")
	}
}

func TestJWTWrongAudience(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	issuer := &JWTIssuer{
		Issuer:   "test",
		Audience: "wrong-aud",
		Key:      priv,
		TTL:      5 * time.Minute,
	}
	token, _ := issuer.Mint("user-1", "project-abc")

	verifier := &JWTVerifier{
		Audience: "bridge",
		Keys:     map[string]ed25519.PublicKey{"test": pub},
	}

	_, err := verifier.Verify(token)
	if err == nil {
		t.Error("expected error for wrong audience")
	}
}

func TestJWTMalformedToken(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &JWTVerifier{
		Audience: "bridge",
		Keys:     map[string]ed25519.PublicKey{"test": pub},
	}

	cases := []struct {
		name  string
		token string
	}{
		{"empty string", ""},
		{"not a jwt", "this is not a jwt"},
		{"only two parts", "header.payload"},
		{"invalid base64", "!!.!!.!!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifier.Verify(tc.token)
			if err == nil {
				t.Errorf("Verify(%q) expected error, got nil", tc.token)
			}
		})
	}
}

func TestJWTTamperedSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	issuer := &JWTIssuer{
		Issuer:   "test",
		Audience: "bridge",
		Key:      priv,
		TTL:      5 * time.Minute,
	}
	token, err := issuer.Mint("user-1", "project-abc")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Flip the last byte of the signature (base64url-encoded third segment).
	parts := splitJWT(token)
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	sig := []byte(parts[2])
	sig[len(sig)-1] ^= 0xFF // corrupt one byte
	tampered := parts[0] + "." + parts[1] + "." + string(sig)

	verifier := &JWTVerifier{
		Audience: "bridge",
		MaxTTL:   10 * time.Minute,
		Keys:     map[string]ed25519.PublicKey{"test": pub},
	}
	_, err = verifier.Verify(tampered)
	if err == nil {
		t.Error("Verify accepted a tampered signature")
	}
}

func TestJWTExpiredToken(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	// Negative TTL produces a token whose exp is already in the past.
	issuer := &JWTIssuer{
		Issuer:   "test",
		Audience: "bridge",
		Key:      priv,
		TTL:      -time.Second,
	}
	token, err := issuer.Mint("user-1", "project-abc")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	verifier := &JWTVerifier{
		Audience: "bridge",
		MaxTTL:   time.Hour,
		Keys:     map[string]ed25519.PublicKey{"test": pub},
	}
	_, err = verifier.Verify(token)
	if err == nil {
		t.Error("Verify accepted an already-expired token")
	}
}

func TestJWTAddKeyAndHasKey(t *testing.T) {
	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)

	verifier := &JWTVerifier{
		Audience: "bridge",
		MaxTTL:   time.Hour,
		Keys:     map[string]ed25519.PublicKey{"issuer-1": pub1},
	}

	if !verifier.HasKey("issuer-1") {
		t.Error("HasKey returned false for registered issuer")
	}
	if verifier.HasKey("issuer-2") {
		t.Error("HasKey returned true for unregistered issuer")
	}

	// Token from issuer-2 should fail before AddKey.
	issuer2 := &JWTIssuer{Issuer: "issuer-2", Audience: "bridge", Key: priv2, TTL: time.Minute}
	tok2, _ := issuer2.Mint("u", "p")
	if _, err := verifier.Verify(tok2); err == nil {
		t.Error("expected error for unknown issuer before AddKey")
	}

	// Register issuer-2 and verify it now works.
	verifier.AddKey("issuer-2", pub2)
	if !verifier.HasKey("issuer-2") {
		t.Error("HasKey returned false after AddKey")
	}
	if _, err := verifier.Verify(tok2); err != nil {
		t.Errorf("Verify failed after AddKey: %v", err)
	}

	// AddKey replaces an existing key — token signed with old priv1 should now fail.
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)
	verifier.AddKey("issuer-1", newPub)
	issuer1 := &JWTIssuer{Issuer: "issuer-1", Audience: "bridge", Key: priv1, TTL: time.Minute}
	tok1, _ := issuer1.Mint("u", "p")
	if _, err := verifier.Verify(tok1); err == nil {
		t.Error("expected Verify to fail after replacing issuer-1 key")
	}
}

// splitJWT splits a JWT into its three dot-separated parts.
func splitJWT(token string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	return parts
}
