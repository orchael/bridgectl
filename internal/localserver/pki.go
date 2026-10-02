package localserver

import (
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/orchael/bridgectl/internal/pki"
)

// safeNameRe matches a simple filename component: alphanumeric, hyphens, underscores, dots.
var safeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

const (
	pkiModeAuto   = "auto"
	pkiModeStepCA = "step-ca"
	pkiModeTLS    = "tls"
	pkiModeFile   = ".pki-mode"
	pkiSANsFile   = ".pki-sans"
)

// readPKIMode returns the PKI mode recorded in certsDir/.pki-mode, or "" if absent.
func readPKIMode(certsDir string) string {
	data, err := os.ReadFile(filepath.Join(certsDir, pkiModeFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writePKIMode records the PKI mode in certsDir/.pki-mode.
func writePKIMode(certsDir, mode string) error {
	return os.WriteFile(filepath.Join(certsDir, pkiModeFile), []byte(mode), 0o644)
}

// normalizeSANs trims whitespace and drops empty entries from sans.
func normalizeSANs(sans []string) []string {
	out := make([]string, 0, len(sans))
	for _, s := range sans {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// stringSetsEqual reports whether a and b contain the same elements,
// ignoring order.
func stringSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// readPKISANs returns the server SANs recorded in certsDir/.pki-sans by the
// most recent successful EnsurePKI call, and whether a record exists at all.
// ok is false when the file is absent or unreadable (e.g. PKI material from
// a bridgectl version that predates this tracking, or material placed on
// disk out-of-band) — callers must treat that as "unknown", not "changed".
func readPKISANs(certsDir string) (sans []string, ok bool) {
	data, err := os.ReadFile(filepath.Join(certsDir, pkiSANsFile))
	if err != nil {
		return nil, false
	}
	return normalizeSANs(strings.Split(string(data), "\n")), true
}

// writePKISANs records the server SANs requested for the certificate
// currently on disk, so a later EnsurePKI call can detect an operator
// restarting with a different --san value (issue #224).
func writePKISANs(certsDir string, serverSANs []string) error {
	data := strings.Join(normalizeSANs(serverSANs), "\n") + "\n"
	return os.WriteFile(filepath.Join(certsDir, pkiSANsFile), []byte(data), 0o644)
}

// certSANsMatch reports whether cert's SANs are exactly the set described by
// requestedSANs (order-independent, already normalized), split into DNS
// names and IP addresses the same way pki.IssueCert does. Used only as a
// one-time migration fallback by pkiRequestedSANsChanged for auto-generated
// PKI that predates .pki-sans tracking — never for Step CA-issued certs,
// whose SAN content an external provisioner can legitimately narrow.
func certSANsMatch(cert *x509.Certificate, requestedSANs []string) bool {
	var wantDNS, wantIPs []string
	for _, san := range requestedSANs {
		if ip := net.ParseIP(san); ip != nil {
			wantIPs = append(wantIPs, ip.String())
		} else {
			wantDNS = append(wantDNS, san)
		}
	}
	gotIPs := make([]string, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		gotIPs = append(gotIPs, ip.String())
	}
	return stringSetsEqual(cert.DNSNames, wantDNS) && stringSetsEqual(gotIPs, wantIPs)
}

// pkiRequestedSANsChanged reports whether serverSANs differs from the SANs
// recorded by the prior EnsurePKI call for this certsDir.
//
// If a .pki-sans record exists, the comparison is against it, not against
// mat's certificate content: an external CA or provisioner (Step CA, ACME)
// can legitimately narrow the issued SAN set, which must not be mistaken
// for a changed --san request. See
// TestEnsurePKI_StepCAUnchangedSANsDoesNotForceRenewal.
//
// If no record exists yet (PKI material from a bridgectl version that
// predates this tracking), the fallback depends on mode:
//   - Auto-generated PKI (stepCA nil): bridgectl's own local CA issued
//     exactly what was requested, so it's safe to migrate by comparing
//     serverSANs against the existing certificate's actual content.
//   - Step CA-issued certs: for the same external-narrowing reason above,
//     comparing against the cert's own content would be unsafe here, so a
//     missing record is treated as unchanged; the normal expiry-driven
//     renewal eventually reconciles a real SAN change in this case.
func pkiRequestedSANsChanged(mat *PKIMaterial, certsDir string, serverSANs []string, stepCA *StepCAConfig) bool {
	requested := normalizeSANs(serverSANs)

	if recorded, ok := readPKISANs(certsDir); ok {
		return !stringSetsEqual(recorded, requested)
	}

	if stepCA != nil && stepCA.URL != "" {
		return false
	}

	cert, err := pki.LoadCert(mat.ServerCertPath)
	if err != nil {
		return false
	}
	return !certSANsMatch(cert, requested)
}

// StepCAConfig holds optional Step CA integration settings. When URL is set,
// EnsurePKI delegates certificate issuance to the Step CA instance instead of
// generating a self-signed CA. The `step` CLI must be on PATH.
type StepCAConfig struct {
	// URL is the Step CA server URL (e.g. "https://step-ca.internal:443").
	URL string
	// RootPath is the path to the Step CA root certificate used to verify
	// the CA server's TLS connection.
	RootPath string
	// OIDCProviderURL is the OIDC issuer URL configured as a Step CA
	// provisioner (e.g. "https://accounts.google.com"). Used by
	// IssueClientCertViaOIDC to select the correct provisioner.
	OIDCProviderURL string
	// Provisioner is the name of the Step CA provisioner to use for
	// certificate requests (e.g. "acme", "bridge-jwk"). When empty, the
	// step CLI selects the default provisioner.
	Provisioner string
	// ProvisionerPasswordFile is the path to a file containing the JWK
	// provisioner password. When set, it is passed as
	// --provisioner-password-file to `step ca certificate` so the command
	// can run non-interactively (e.g. in Docker containers without a TTY).
	ProvisionerPasswordFile string
}

// PKIMaterial holds resolved paths to all PKI files needed for secure mode.
type PKIMaterial struct {
	CACertPath      string // certs/ca.crt
	CAKeyPath       string // certs/ca.key
	ServerCertPath  string // certs/server.crt
	ServerKeyPath   string // certs/server.key
	LocalClientCert string // certs/local-client.crt
	LocalClientKey  string // certs/local-client.key
	CABundlePath    string // certs/ca-bundle.crt
	JWTSigningKey   string // certs/jwt-signing.key
	JWTSigningPub   string // certs/jwt-signing.pub
}

// CertsDir returns the path to the certs subdirectory within the state dir.
func CertsDir(stateDir string) string {
	return filepath.Join(stateDir, "certs")
}

// LoadPKIMaterial returns a PKIMaterial with resolved paths for an existing
// certs directory. It does not check whether the files exist.
func LoadPKIMaterial(stateDir string) *PKIMaterial {
	dir := CertsDir(stateDir)
	return &PKIMaterial{
		CACertPath:      filepath.Join(dir, "ca.crt"),
		CAKeyPath:       filepath.Join(dir, "ca.key"),
		ServerCertPath:  filepath.Join(dir, "server.crt"),
		ServerKeyPath:   filepath.Join(dir, "server.key"),
		LocalClientCert: filepath.Join(dir, "local-client.crt"),
		LocalClientKey:  filepath.Join(dir, "local-client.key"),
		CABundlePath:    filepath.Join(dir, "ca-bundle.crt"),
		JWTSigningKey:   filepath.Join(dir, "jwt-signing.key"),
		JWTSigningPub:   filepath.Join(dir, "jwt-signing.pub"),
	}
}

// EnsurePKI ensures that all PKI material exists in stateDir/certs/.
//
// When stepCA is nil (default), the entire set is auto-generated:
//   - CA cert/key (ECDSA P-384, 10-year validity)
//   - Server cert/key with the provided SANs (90-day validity)
//   - Local-client cert/key for CLI's own connections
//   - CA trust bundle
//   - Ed25519 JWT signing keypair
//
// When stepCA is non-nil, auto-generation is skipped. Instead, the Step CA
// root is copied to ca-bundle.crt and the server certificate is obtained from
// Step CA via the `step` CLI. The JWT keypair is still generated locally.
//
// If the CA bundle already exists, this is a no-op and returns existing paths.
func EnsurePKI(stateDir string, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig, certValidity time.Duration) (*PKIMaterial, error) {
	mat := LoadPKIMaterial(stateDir)
	certsDir := CertsDir(stateDir)

	// Check that all essential PKI files exist. If any are missing, regenerate.
	// This covers partial state from interrupted runs or manual cleanup.
	// We also check the PKI mode: if the server is restarted with --step-ca-url
	// after a previous auto-PKI run (or vice versa), the existing certs are
	// incompatible and must be regenerated.
	requestedMode := pkiModeAuto
	if stepCA != nil && stepCA.URL != "" {
		requestedMode = pkiModeStepCA
	}
	if _, err := os.Stat(mat.CABundlePath); err == nil {
		if _, err := os.Stat(mat.ServerCertPath); err == nil {
			if _, err := os.Stat(mat.ServerKeyPath); err == nil {
				if readPKIMode(certsDir) == requestedMode {
					// Renew synchronously before returning so the server
					// never starts listening with a stale or mismatched
					// cert: either the operator restarted with a different
					// --san value (issue #224), or the existing certificate
					// is expired or approaching expiry (issue #225).
					if pkiRequestedSANsChanged(mat, certsDir, serverSANs, stepCA) {
						logger.Info("requested server SANs changed since the last start, reissuing certificate before startup",
							"cert", mat.ServerCertPath, "requested_sans", serverSANs)
						if err := reissueServerCertMaterial(mat, serverSANs, logger, stepCA, certValidity); err != nil {
							return nil, err
						}
					} else if err := ensureServerCertFresh(mat, serverSANs, logger, stepCA, certValidity); err != nil {
						return nil, err
					}
					_ = writePKISANs(certsDir, serverSANs) // best-effort; worst case re-checks next start
					logger.Info("PKI material already exists", "dir", certsDir)
					return mat, nil
				}
				logger.Info("PKI mode changed, regenerating", "dir", certsDir, "mode", requestedMode)
			}
		}
	}

	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create certs dir: %w", err)
	}

	if stepCA != nil && stepCA.URL != "" {
		return ensurePKIStepCA(stateDir, serverSANs, logger, stepCA, mat, certsDir, certValidity)
	}
	return ensurePKIAutoGen(stateDir, serverSANs, logger, mat, certsDir, certValidity)
}

// EnsureLocalManagementPKI creates the local-only client credentials needed by
// same-machine bridgectl commands when the server uses externally managed TLS
// certificates. The returned CA bundle is a state-directory copy of the
// external server CA bundle plus the generated local management CA.
func EnsureLocalManagementPKI(stateDir, externalCABundle string, logger *slog.Logger) (*PKIMaterial, error) {
	mat := LoadPKIMaterial(stateDir)
	certsDir := CertsDir(stateDir)
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create certs dir: %w", err)
	}

	if !filesExist(mat.CACertPath, mat.CAKeyPath, mat.LocalClientCert, mat.LocalClientKey) {
		logger.Info("generating local management PKI material", "dir", certsDir)
		localCACert, localCAKey, err := pki.InitCA("bridgectl-local", certsDir)
		if err != nil {
			return nil, fmt.Errorf("init local management CA: %w", err)
		}
		mat.CACertPath = localCACert
		mat.CAKeyPath = localCAKey

		localCA, localKey, err := pki.LoadCA(localCACert, localCAKey)
		if err != nil {
			return nil, fmt.Errorf("load local management CA: %w", err)
		}
		clientCert, clientKey, err := pki.IssueCert(localCA, localKey, pki.CertTypeClient, "local-client", nil, certsDir, 0)
		if err != nil {
			return nil, fmt.Errorf("issue local-client cert: %w", err)
		}
		mat.LocalClientCert = clientCert
		mat.LocalClientKey = clientKey
		logger.Info("generated local-client cert", "cert", clientCert)
	}

	if !filesExist(mat.JWTSigningKey, mat.JWTSigningPub) {
		pubPath, privPath, err := pki.GenerateJWTKeypair(certsDir, "jwt-signing")
		if err != nil {
			return nil, fmt.Errorf("generate JWT keypair: %w", err)
		}
		mat.JWTSigningPub = pubPath
		mat.JWTSigningKey = privPath
		logger.Info("generated JWT signing keypair", "pub", pubPath)
	}

	if err := writeLocalManagementBundle(externalCABundle, mat.CABundlePath, mat.CACertPath); err != nil {
		return nil, err
	}
	_ = writePKIMode(certsDir, pkiModeTLS)

	return mat, nil
}

func writeLocalManagementBundle(externalCABundle, outPath, localCACertPath string) error {
	bundle, err := os.ReadFile(externalCABundle)
	if err != nil {
		return fmt.Errorf("read external CA bundle: %w", err)
	}
	localCA, err := os.ReadFile(localCACertPath)
	if err != nil {
		return fmt.Errorf("read local management CA: %w", err)
	}
	if !strings.HasSuffix(string(bundle), "\n") {
		bundle = append(bundle, '\n')
	}
	if !strings.Contains(string(bundle), strings.TrimSpace(string(localCA))) {
		bundle = append(bundle, localCA...)
		if !strings.HasSuffix(string(bundle), "\n") {
			bundle = append(bundle, '\n')
		}
	}
	if err := os.WriteFile(outPath, bundle, 0o644); err != nil {
		return fmt.Errorf("write local management CA bundle: %w", err)
	}
	return nil
}

func filesExist(paths ...string) bool {
	for _, path := range paths {
		if path == "" {
			return false
		}
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

// ensurePKIAutoGen generates a self-signed CA and all derived material.
func ensurePKIAutoGen(stateDir string, serverSANs []string, logger *slog.Logger, mat *PKIMaterial, certsDir string, certValidity time.Duration) (*PKIMaterial, error) {
	logger.Info("generating PKI material", "dir", certsDir)

	// 1. Generate CA.
	caCertPath, caKeyPath, err := pki.InitCA("bridgectl", certsDir)
	if err != nil {
		return nil, fmt.Errorf("init CA: %w", err)
	}
	mat.CACertPath = caCertPath
	mat.CAKeyPath = caKeyPath
	logger.Info("generated CA", "cert", caCertPath)

	// Load CA for signing.
	caCert, caKey, err := pki.LoadCA(caCertPath, caKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}

	// 2. Issue server certificate with SANs.
	serverCert, serverKey, err := pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", serverSANs, certsDir, certValidity)
	if err != nil {
		return nil, fmt.Errorf("issue server cert: %w", err)
	}
	mat.ServerCertPath = serverCert
	mat.ServerKeyPath = serverKey
	logger.Info("generated server cert", "cert", serverCert, "sans", serverSANs)

	// 3. Issue local-client certificate for CLI connections.
	clientCert, clientKey, err := pki.IssueCert(caCert, caKey, pki.CertTypeClient, "local-client", nil, certsDir, 0)
	if err != nil {
		return nil, fmt.Errorf("issue local-client cert: %w", err)
	}
	mat.LocalClientCert = clientCert
	mat.LocalClientKey = clientKey
	logger.Info("generated local-client cert", "cert", clientCert)

	// 4. Build CA trust bundle (just the CA cert for now).
	bundlePath := filepath.Join(certsDir, "ca-bundle.crt")
	if err := pki.BuildBundle(bundlePath, caCertPath); err != nil {
		return nil, fmt.Errorf("build CA bundle: %w", err)
	}
	mat.CABundlePath = bundlePath
	logger.Info("built CA bundle", "path", bundlePath)

	// 5. Generate Ed25519 JWT signing keypair.
	pubPath, privPath, err := pki.GenerateJWTKeypair(certsDir, "jwt-signing")
	if err != nil {
		return nil, fmt.Errorf("generate JWT keypair: %w", err)
	}
	mat.JWTSigningPub = pubPath
	mat.JWTSigningKey = privPath
	logger.Info("generated JWT signing keypair", "pub", pubPath)

	// Record mode and requested SANs so EnsurePKI can detect a mode switch
	// or a changed --san value on the next start.
	_ = writePKIMode(certsDir, pkiModeAuto)
	_ = writePKISANs(certsDir, serverSANs)

	return mat, nil
}

// ensurePKIStepCA uses the Step CA CLI to obtain a server certificate.
// The Step CA root cert is copied to ca-bundle.crt; JWT material is generated
// locally as in auto-PKI mode (tokens are still validated per-client).
func ensurePKIStepCA(stateDir string, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig, mat *PKIMaterial, certsDir string, certValidity time.Duration) (*PKIMaterial, error) {
	logger.Info("using Step CA for PKI", "url", stepCA.URL)

	// 1. Copy Step CA root cert to ca-bundle.crt so clients can verify the server.
	if stepCA.RootPath == "" {
		return nil, fmt.Errorf("step-ca-root is required when --step-ca-url is set")
	}
	bundlePath := filepath.Join(certsDir, "ca-bundle.crt")
	if err := copyFile(stepCA.RootPath, bundlePath); err != nil {
		return nil, fmt.Errorf("copy Step CA root to ca-bundle: %w", err)
	}
	mat.CABundlePath = bundlePath
	// In Step CA mode there is no local CA key — leave mat.CACertPath and
	// mat.CAKeyPath empty; they are only used by IssueClientCert (auto-PKI path).
	mat.CACertPath = bundlePath
	logger.Info("copied Step CA root", "bundle", bundlePath)

	// 2. Obtain server certificate from Step CA.
	serverCert := filepath.Join(certsDir, "server.crt")
	serverKey := filepath.Join(certsDir, "server.key")

	// Dispatch to the native certificate request function based on provisioner type.
	switch strings.ToLower(stepCA.Provisioner) {
	case "acme":
		if err := requestCertACMEFn(stepCA, acmeSANs(serverSANs), serverCert, serverKey, logger); err != nil {
			return nil, fmt.Errorf("obtain server cert from Step CA (ACME): %w", err)
		}
	default:
		// JWK (internal Step CA) accepts all SANs including "server",
		// which must be present so the client can verify ServerName "server".
		if err := requestCertJWKFn(stepCA, serverSANs, serverCert, serverKey, logger); err != nil {
			return nil, fmt.Errorf("obtain server cert from Step CA (JWK): %w", err)
		}
	}
	mat.ServerCertPath = serverCert
	mat.ServerKeyPath = serverKey
	logger.Info("obtained server cert from Step CA", "cert", serverCert)

	// 3. Generate a local CA for CLI/local-client credentials.
	// The server cert comes from Step CA; this small CA signs only the local
	// management cert so operators can use bridgectl locally against a secure server.
	localCACert, localCAKey, err := pki.InitCA("bridgectl-local", certsDir)
	if err != nil {
		return nil, fmt.Errorf("init local CA for Step CA mode: %w", err)
	}
	mat.CACertPath = localCACert
	mat.CAKeyPath = localCAKey
	logger.Info("generated local CA for CLI credentials", "cert", localCACert)

	localCA, localKey, err := pki.LoadCA(localCACert, localCAKey)
	if err != nil {
		return nil, fmt.Errorf("load local CA: %w", err)
	}

	clientCert, clientKey, err := pki.IssueCert(localCA, localKey, pki.CertTypeClient, "local-client", nil, certsDir, 0)
	if err != nil {
		return nil, fmt.Errorf("issue local-client cert: %w", err)
	}
	mat.LocalClientCert = clientCert
	mat.LocalClientKey = clientKey
	logger.Info("generated local-client cert", "cert", clientCert)

	// Append the local CA to the trust bundle so the server accepts local-client.
	if err := pki.AppendBundle(bundlePath, localCACert); err != nil {
		return nil, fmt.Errorf("append local CA to bundle: %w", err)
	}
	logger.Info("appended local CA to trust bundle", "bundle", bundlePath)

	// 4. Generate JWT keypair locally (same as auto-PKI path).
	pubPath, privPath, err := pki.GenerateJWTKeypair(certsDir, "jwt-signing")
	if err != nil {
		return nil, fmt.Errorf("generate JWT keypair: %w", err)
	}
	mat.JWTSigningPub = pubPath
	mat.JWTSigningKey = privPath
	logger.Info("generated JWT signing keypair", "pub", pubPath)

	// Record mode and requested SANs so EnsurePKI can detect a mode switch
	// or a changed --san value on the next start.
	_ = writePKIMode(certsDir, pkiModeStepCA)
	_ = writePKISANs(certsDir, serverSANs)

	return mat, nil
}

// acmeSANs filters serverSANs for ACME compatibility. ACME / public CAs
// reject bare hostnames like "localhost" and raw IPs like "127.0.0.1" during
// HTTP-01 or DNS-01 challenges. This strips those but keeps "server": all
// bridgectl clients hard-code ServerName "server" for TLS verification, so
// the server cert must include it regardless of provisioner type.
func acmeSANs(serverSANs []string) []string {
	var sans []string
	for _, s := range serverSANs {
		switch s {
		case "localhost", "127.0.0.1":
			continue
		default:
			sans = append(sans, s)
		}
	}
	if len(sans) == 0 {
		hostname, _ := os.Hostname()
		if hostname != "" {
			sans = []string{hostname}
		} else {
			sans = []string{"bridge"}
		}
	}
	return sans
}

// RenewServerCert re-issues the server certificate in-place. For auto-PKI it
// re-signs using the existing CA; for Step CA it requests a new cert from the
// CA server. The new cert is written to the same file paths so the
// CertReloader picks it up on the next TLS handshake without a server restart.
func RenewServerCert(stateDir string, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig, certValidity time.Duration) error {
	mat := LoadPKIMaterial(stateDir)
	return RenewServerCertMaterial(mat, serverSANs, logger, stepCA, certValidity)
}

// RenewServerCertMaterial re-issues the certificate described by mat in-place.
// It is used both for state-dir managed certificates and for explicit TLS
// certificate paths loaded from config.
func RenewServerCertMaterial(mat *PKIMaterial, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig, certValidity time.Duration) error {
	if stepCA != nil && stepCA.URL != "" {
		return renewServerCertStepCA(mat, serverSANs, logger, stepCA)
	}
	return renewServerCertAutoGen(mat, serverSANs, logger, certValidity)
}

func renewServerCertAutoGen(mat *PKIMaterial, serverSANs []string, logger *slog.Logger, certValidity time.Duration) error {
	caCert, caKey, err := pki.LoadCA(mat.CACertPath, mat.CAKeyPath)
	if err != nil {
		return fmt.Errorf("load CA: %w", err)
	}

	certsDir := filepath.Dir(mat.ServerCertPath)
	_, _, err = pki.IssueCert(caCert, caKey, pki.CertTypeServer, "server", serverSANs, certsDir, certValidity)
	if err != nil {
		return fmt.Errorf("re-issue server cert: %w", err)
	}

	logger.Info("renewed server certificate (auto-PKI)", "cert", mat.ServerCertPath, "sans", serverSANs)
	return nil
}

// reissueServerCertMaterial issues a brand-new server certificate for
// serverSANs, bypassing any identity-preserving renewal path. Used
// specifically when serverSANs has changed (issue #224): Step CA's mTLS
// /renew endpoint preserves the existing certificate's SANs rather than
// accepting new ones (see stepca_native.go), so a SAN change must go
// straight to provisioner-based issuance (JWK/ACME) instead of
// RenewServerCertMaterial's mTLS-first path. For auto-generated PKI,
// renewServerCertAutoGen already always re-issues with serverSANs, so it is
// reused directly.
func reissueServerCertMaterial(mat *PKIMaterial, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig, certValidity time.Duration) error {
	if stepCA != nil && stepCA.URL != "" {
		return reissueServerCertStepCA(mat, serverSANs, logger, stepCA)
	}
	return renewServerCertAutoGen(mat, serverSANs, logger, certValidity)
}

// reissueServerCertStepCA requests a fresh certificate directly from the
// configured provisioner, skipping mTLS-based renewal, so the issued
// certificate reflects serverSANs rather than whatever SANs the previous
// certificate carried.
func reissueServerCertStepCA(mat *PKIMaterial, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig) error {
	switch strings.ToLower(stepCA.Provisioner) {
	case "acme":
		if err := requestCertACMEFn(stepCA, acmeSANs(serverSANs), mat.ServerCertPath, mat.ServerKeyPath, logger); err != nil {
			return fmt.Errorf("reissue server cert for changed SANs (ACME): %w", err)
		}
	default:
		if stepCA.ProvisionerPasswordFile == "" {
			return fmt.Errorf("reissue server cert for changed SANs (JWK): step_ca.provisioner_password_file is required for non-interactive issuance")
		}
		if err := requestCertJWKFn(stepCA, serverSANs, mat.ServerCertPath, mat.ServerKeyPath, logger); err != nil {
			return fmt.Errorf("reissue server cert for changed SANs (JWK): %w", err)
		}
	}

	logger.Info("reissued server certificate with new SANs (Step CA)", "cert", mat.ServerCertPath, "sans", serverSANs)
	return nil
}

// ensureServerCertFresh checks whether the server certificate at
// mat.ServerCertPath is expired or approaching expiry (remaining < 1/3 of
// lifetime), and renews it synchronously when so — the caller never proceeds
// with a stale cert (issue #225). stepCA is nil for auto-generated PKI and
// non-nil for Step CA-issued certs; RenewServerCertMaterial dispatches to the
// matching renewal path. If the certificate is unreadable (corrupt PEM,
// etc.) it is treated as expired and replaced.
//
// This intentionally only looks at expiry, never at the certificate's own
// SAN content — see pkiRequestedSANsChanged for why SAN-change detection is
// based on bridgectl's own recorded request instead of the issued cert.
func ensureServerCertFresh(mat *PKIMaterial, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig, certValidity time.Duration) error {
	source := "server"
	if stepCA != nil && stepCA.URL != "" {
		source = "Step CA server"
	}

	notBefore, notAfter, err := ServerCertExpiry(mat.ServerCertPath)
	if err != nil {
		logger.Warn(source+" certificate unreadable, renewing before startup",
			"cert", mat.ServerCertPath, "error", err)
		return RenewServerCertMaterial(mat, serverSANs, logger, stepCA, certValidity)
	}

	lifetime := notAfter.Sub(notBefore)
	remaining := time.Until(notAfter)

	// Guard against certs with zero or inverted validity windows —
	// treat them as expired so they are always renewed.
	if lifetime <= 0 {
		logger.Warn(source+" certificate has non-positive validity, renewing before startup",
			"cert", mat.ServerCertPath, "notBefore", notBefore.Format(time.RFC3339),
			"notAfter", notAfter.Format(time.RFC3339))
		return RenewServerCertMaterial(mat, serverSANs, logger, stepCA, certValidity)
	}

	renewThreshold := lifetime / 3
	if remaining > renewThreshold {
		return nil // still fresh
	}

	if remaining <= 0 {
		logger.Warn(source+" certificate has expired, renewing before startup",
			"cert", mat.ServerCertPath, "expired", notAfter.Format(time.RFC3339))
	} else {
		logger.Info(source+" certificate approaching expiry, renewing before startup",
			"cert", mat.ServerCertPath, "expires", notAfter.Format(time.RFC3339),
			"remaining", remaining.Round(time.Second))
	}
	return RenewServerCertMaterial(mat, serverSANs, logger, stepCA, certValidity)
}

func renewServerCertStepCA(mat *PKIMaterial, serverSANs []string, logger *slog.Logger, stepCA *StepCAConfig) error {
	// Try mTLS-based renewal first: present the existing cert/key to the Step CA
	// /renew endpoint. This works for any provisioner type (JWK, ACME, etc.)
	// and does not require the provisioner password or an HTTP-01 challenge.
	if err := renewCertMTLSFn(stepCA, mat.ServerCertPath, mat.ServerKeyPath, logger); err != nil {
		// mTLS renewal fails when the existing certificate has expired past
		// Step CA's acceptance window. Fall back to a full provisioner-based
		// certificate request (JWK or ACME) which authenticates independently.
		logger.Warn("mTLS renewal failed, falling back to provisioner-based certificate request", "error", err)

		switch strings.ToLower(stepCA.Provisioner) {
		case "acme":
			if err := requestCertACMEFn(stepCA, acmeSANs(serverSANs), mat.ServerCertPath, mat.ServerKeyPath, logger); err != nil {
				return fmt.Errorf("renew server cert (ACME fallback after mTLS failure): %w", err)
			}
		default:
			if stepCA.ProvisionerPasswordFile == "" {
				return fmt.Errorf("renew server cert (JWK fallback after mTLS failure): step_ca.provisioner_password_file is required for non-interactive renewal")
			}
			if err := requestCertJWKFn(stepCA, serverSANs, mat.ServerCertPath, mat.ServerKeyPath, logger); err != nil {
				return fmt.Errorf("renew server cert (JWK fallback after mTLS failure): %w", err)
			}
		}
	}

	logger.Info("renewed server certificate (Step CA)", "cert", mat.ServerCertPath)
	return nil
}

// ServerCertExpiry returns the NotBefore and NotAfter times of the server
// certificate at the given path. Used by the renewal loop to determine when
// to trigger renewal.
func ServerCertExpiry(certPath string) (notBefore, notAfter time.Time, err error) {
	cert, err := pki.LoadCert(certPath)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return cert.NotBefore, cert.NotAfter, nil
}

// IssueClientCert issues a new client certificate signed by the existing CA.
// The cert is written to stateDir/certs/clients/<clientName>/.
// Returns paths to the cert and key files.
func IssueClientCert(stateDir, clientName string, logger *slog.Logger) (certPath, keyPath string, err error) {
	// Validate client name to prevent path traversal (e.g. "../shared").
	if !safeNameRe.MatchString(clientName) {
		return "", "", fmt.Errorf("invalid client name %q: must be alphanumeric with hyphens, underscores, or dots", clientName)
	}

	mat := LoadPKIMaterial(stateDir)

	// Load existing CA.
	caCert, caKey, err := pki.LoadCA(mat.CACertPath, mat.CAKeyPath)
	if err != nil {
		return "", "", fmt.Errorf("load CA (run 'server start --listen' first to generate PKI): %w", err)
	}

	outDir := filepath.Join(CertsDir(stateDir), "clients", clientName)
	certPath, keyPath, err = pki.IssueCert(caCert, caKey, pki.CertTypeClient, clientName, nil, outDir, 0)
	if err != nil {
		return "", "", fmt.Errorf("issue client cert: %w", err)
	}

	// Generate a per-client JWT keypair so compromising one client doesn't
	// compromise JWT auth for all clients.
	jwtPubPath, jwtKeyPath, err := pki.GenerateJWTKeypair(outDir, "jwt-signing")
	if err != nil {
		return "", "", fmt.Errorf("generate client JWT keypair: %w", err)
	}

	// Register the client's public key with the server so the verifier
	// accepts tokens signed by this client.
	serverJWTDir := filepath.Join(CertsDir(stateDir), "jwt-clients")
	if err := os.MkdirAll(serverJWTDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create jwt-clients dir: %w", err)
	}
	pubData, err := os.ReadFile(jwtPubPath)
	if err != nil {
		return "", "", fmt.Errorf("read client JWT public key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(serverJWTDir, clientName+".pub"), pubData, 0o644); err != nil {
		return "", "", fmt.Errorf("write client JWT public key: %w", err)
	}

	logger.Info("issued client credentials", "name", clientName, "cert", certPath, "jwt_key", jwtKeyPath)
	return certPath, keyPath, nil
}

// IssueClientCertViaOIDC obtains a short-lived client certificate from Step CA
// using the OIDC provisioner. It opens a browser for the OIDC login flow and
// waits for Step CA to issue the certificate. The `step` CLI must be on PATH.
//
// The cert and key are written to stateDir/certs/clients/<clientName>/ (same
// layout as IssueClientCert) so the same SDK configuration works for both
// auto-PKI and Step CA clients.
func IssueClientCertViaOIDC(stateDir, clientName string, stepCA *StepCAConfig, logger *slog.Logger) (certPath, keyPath string, err error) {
	if !safeNameRe.MatchString(clientName) {
		return "", "", fmt.Errorf("invalid client name %q: must be alphanumeric with hyphens, underscores, or dots", clientName)
	}
	if stepCA == nil || stepCA.URL == "" {
		return "", "", fmt.Errorf("--step-ca-url is required for OIDC enrollment")
	}
	if stepCA.OIDCProviderURL == "" {
		return "", "", fmt.Errorf("--oidc-provider is required for OIDC enrollment")
	}
	if stepCA.RootPath == "" {
		return "", "", fmt.Errorf("--step-ca-root is required for OIDC enrollment")
	}

	outDir := filepath.Join(CertsDir(stateDir), "clients", clientName)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create client dir: %w", err)
	}

	certPath = filepath.Join(outDir, clientName+".crt")
	keyPath = filepath.Join(outDir, clientName+".key")

	stepArgs := []string{
		"ca", "certificate",
		clientName,
		certPath,
		keyPath,
		"--provisioner", stepCA.OIDCProviderURL,
		"--ca-url", stepCA.URL,
		"--root", stepCA.RootPath,
		"--not-after", "24h",
		"--force",
	}
	logger.Info("starting OIDC enrollment via Step CA", "name", clientName, "oidc_provider", stepCA.OIDCProviderURL)
	if err := runStep(stepArgs, logger); err != nil {
		return "", "", fmt.Errorf("obtain client cert via OIDC: %w", err)
	}

	// Generate a per-client JWT keypair (same as auto-PKI path).
	jwtPubPath, jwtKeyPath, err := pki.GenerateJWTKeypair(outDir, "jwt-signing")
	if err != nil {
		return "", "", fmt.Errorf("generate client JWT keypair: %w", err)
	}

	// Register the client's public key server-side.
	serverJWTDir := filepath.Join(CertsDir(stateDir), "jwt-clients")
	if err := os.MkdirAll(serverJWTDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create jwt-clients dir: %w", err)
	}
	pubData, err := os.ReadFile(jwtPubPath)
	if err != nil {
		return "", "", fmt.Errorf("read client JWT public key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(serverJWTDir, clientName+".pub"), pubData, 0o644); err != nil {
		return "", "", fmt.Errorf("write client JWT public key: %w", err)
	}

	logger.Info("issued OIDC client credentials", "name", clientName, "cert", certPath, "jwt_key", jwtKeyPath)
	return certPath, keyPath, nil
}

// runStep executes the `step` CLI with the given arguments, inheriting stderr
// and stdout (Step CA prints login URLs and status to stdout).
func runStep(args []string, logger *slog.Logger) error {
	stepBin, err := exec.LookPath("step")
	if err != nil {
		return fmt.Errorf("'step' CLI not found on PATH — required only for OIDC enrollment; install from https://smallstep.com/cli/: %w", err)
	}
	logger.Debug("running step CLI", "args", args)
	cmd := exec.Command(stepBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// copyFile copies src to dst, creating dst with mode 0o644.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if closeErr := out.Close(); closeErr != nil && copyErr == nil {
		return closeErr
	}
	return copyErr
}
