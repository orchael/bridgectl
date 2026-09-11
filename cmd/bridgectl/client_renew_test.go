package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	stepapi "github.com/smallstep/certificates/api"
)

func TestRunClientRenewUsesFetchedRootWhenItVerifiesEndpoint(t *testing.T) {
	caPEM, caKey := selfSignedCA(t)

	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGECTL_STATE_DIR", dir)

	rootPath := filepath.Join(certsDir, "step-ca-root.crt")
	certPath, keyPath, renewedCert := writeRenewTestFiles(t, certsDir, time.Now().Add(time.Hour))
	srv := tlsServerWithRoots(t, caPEM, caKey, caPEM, renewedCert)
	argsPath := installFakeStep(t, renewedCert)

	err := runClientRenew(srv.URL, rootPath, false, certPath, keyPath, 0)
	if err != nil {
		t.Fatalf("runClientRenew() error = %v", err)
	}

	args := readStepArgs(t, argsPath)
	if !containsArgPair(args, "--root", rootPath) {
		t.Fatalf("step args missing --root %s: %v", rootPath, args)
	}
	if !containsArg(args, "--mtls=false") {
		t.Fatalf("step args missing --mtls=false: %v", args)
	}
}

func TestRunClientRenewOmitsDefaultRootWhenEndpointUsesDifferentTLSRoot(t *testing.T) {
	servingCA, servingKey := selfSignedCA(t)
	stepCA, _ := selfSignedCA(t)

	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGECTL_STATE_DIR", dir)

	rootPath := filepath.Join(certsDir, "step-ca-root.crt")
	certPath, keyPath, renewedCert := writeRenewTestFiles(t, certsDir, time.Now().Add(time.Hour))
	srv := tlsServerWithRoots(t, servingCA, servingKey, stepCA, renewedCert)
	argsPath := installFakeStep(t, renewedCert)

	err := runClientRenew(srv.URL, rootPath, false, certPath, keyPath, 0)
	if err != nil {
		t.Fatalf("runClientRenew() error = %v", err)
	}

	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("step CLI should not be called when default root cannot verify HTTPS endpoint")
	}
	renewed, err := loadLeafCert(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(renewed.NotAfter) <= 0 {
		t.Fatalf("cert was not renewed: expires %s", renewed.NotAfter.Format(time.RFC3339))
	}
}

func TestRunClientRenewHonorsExplicitRoot(t *testing.T) {
	servingCA, servingKey := selfSignedCA(t)
	stepCA, _ := selfSignedCA(t)

	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGECTL_STATE_DIR", dir)

	rootPath := writePEM(t, certsDir, "custom-root.crt", stepCA)
	certPath, keyPath, renewedCert := writeRenewTestFiles(t, certsDir, time.Now().Add(time.Hour))
	srv := tlsServerWithRoots(t, servingCA, servingKey, stepCA, renewedCert)
	argsPath := installFakeStep(t, renewedCert)

	err := runClientRenew(srv.URL, rootPath, true, certPath, keyPath, 0)
	if err != nil {
		t.Fatalf("runClientRenew() error = %v", err)
	}

	args := readStepArgs(t, argsPath)
	if !containsArgPair(args, "--root", rootPath) {
		t.Fatalf("step args missing explicit --root %s: %v", rootPath, args)
	}
}

func TestRunClientRenewRejectsExpiredCertificateBeforeRenewal(t *testing.T) {
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGECTL_STATE_DIR", dir)

	rootPath := filepath.Join(certsDir, "step-ca-root.crt")
	certPath, keyPath, renewedCert := writeRenewTestFiles(t, certsDir, time.Now().Add(-time.Hour))
	argsPath := installFakeStep(t, renewedCert)

	err := runClientRenew("https://step-ca.example.com", rootPath, false, certPath, keyPath, 0)
	if err == nil {
		t.Fatal("runClientRenew() error = nil, want expired certificate error")
	}
	if !strings.Contains(err.Error(), "certificate expired") {
		t.Fatalf("runClientRenew() error = %v, want certificate expired", err)
	}
	if !strings.Contains(err.Error(), "bridgectl client init --step-ca-url https://step-ca.example.com --name client") {
		t.Fatalf("runClientRenew() error = %v, want client init recovery command", err)
	}
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("step CLI should not be called for an expired certificate")
	}
	if _, err := os.Stat(rootPath); !os.IsNotExist(err) {
		t.Fatalf("root cert should not be fetched for an expired certificate")
	}
}

func TestRenewAudienceUsesRenewEndpoint(t *testing.T) {
	got, err := renewAudience("https://step-ca.example.com:9443")
	if err != nil {
		t.Fatalf("renewAudience() error = %v", err)
	}
	want := "https://step-ca.example.com:9443/renew"
	if got != want {
		t.Fatalf("renewAudience() = %q, want %q", got, want)
	}
}

func TestIsTLSVerificationError(t *testing.T) {
	err := fmt.Errorf("renew with token: %w", x509.UnknownAuthorityError{})
	if !isTLSVerificationError(err) {
		t.Fatal("isTLSVerificationError() = false for UnknownAuthorityError, want true")
	}
	certVerifyErr := &tls.CertificateVerificationError{Err: fmt.Errorf("x509: certificate is not trusted")}
	if !isTLSVerificationError(fmt.Errorf("renew with token: %w", certVerifyErr)) {
		t.Fatal("isTLSVerificationError() = false for CertificateVerificationError, want true")
	}
	if isTLSVerificationError(fmt.Errorf("renew with token: unauthorized")) {
		t.Fatal("isTLSVerificationError() = true for non-TLS error, want false")
	}
}

func tlsServerWithRoots(t *testing.T, caPEM []byte, caKey *ecdsa.PrivateKey, rootsPEM []byte, renewedCertPath string) *httptest.Server {
	t.Helper()

	caCert, err := x509.ParseCertificate(mustDecode(t, caPEM))
	if err != nil {
		t.Fatal(err)
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(20),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/roots":
			if err := json.NewEncoder(w).Encode(struct {
				Crts []string `json:"crts"`
			}{Crts: []string{string(rootsPEM)}}); err != nil {
				t.Errorf("encode roots response: %v", err)
			}
		case "/renew":
			if r.Header.Get("Authorization") == "" {
				t.Errorf("renew request missing Authorization header")
			}
			renewed := parseCertFile(t, renewedCertPath)
			w.WriteHeader(http.StatusCreated)
			if err := json.NewEncoder(w).Encode(&stepapi.SignResponse{
				ServerPEM: stepapi.NewCertificate(renewed),
			}); err != nil {
				t.Errorf("encode renew response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{
			{
				Certificate: [][]byte{srvDER},
				PrivateKey:  srvKey,
			},
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func writeRenewTestFiles(t *testing.T, certsDir string, existingNotAfter time.Time) (string, string, string) {
	t.Helper()

	certPath := filepath.Join(certsDir, "client.crt")
	keyPath := filepath.Join(certsDir, "client.key")
	renewedCert := filepath.Join(certsDir, "renewed.crt")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(certPath, testLeafCertPEM(t, "client", existingNotAfter, key), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(renewedCert, testLeafCertPEM(t, "client", time.Now().Add(time.Hour), key), 0o644); err != nil {
		t.Fatal(err)
	}

	return certPath, keyPath, renewedCert
}

func testLeafCertPEM(t *testing.T, cn string, notAfter time.Time, key *ecdsa.PrivateKey) []byte {
	t.Helper()

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func parseCertFile(t *testing.T, path string) *x509.Certificate {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("no PEM block in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func installFakeStep(t *testing.T, renewedCert string) string {
	t.Helper()

	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	stepPath := filepath.Join(dir, "step")
	script := `#!/bin/sh
last=
prev=
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$STEP_ARGS_FILE"
  prev="$last"
  last="$arg"
done
cp "$RENEWED_CERT" "$prev"
`
	if err := os.WriteFile(stepPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STEP_ARGS_FILE", argsPath)
	t.Setenv("RENEWED_CERT", renewedCert)
	return argsPath
}

func readStepArgs(t *testing.T, argsPath string) []string {
	t.Helper()

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
