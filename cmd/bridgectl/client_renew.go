package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ca "github.com/smallstep/certificates/ca"
	"github.com/spf13/cobra"
	"go.step.sm/crypto/jose"

	"github.com/orchael/bridgectl/internal/localserver"
)

type renewMode int

const (
	renewModeStepCLI renewMode = iota
	renewModeNativeInsecureTLS
)

func newClientRenewCmd() *cobra.Command {
	var (
		stepCAURL string
		rootPath  string
		certPath  string
		keyPath   string
		before    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "renew",
		Short: "Renew a client certificate from Step CA",
		Long: `Renew an existing client certificate via the Step CA /renew endpoint.

Uses the 'step' CLI to perform renewal with a signed token (the same
mechanism as 'step ca renew'). The cert file is overwritten in-place;
the key is unchanged.

Use --before to only renew when the cert expires within a given window,
making this safe to call from a cron job or systemd timer:

  bridgectl client renew --step-ca-url https://step-ca.example.com --before 2h`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClientRenew(stepCAURL, rootPath, cmd.Flags().Changed("step-ca-root"), certPath, keyPath, before)
		},
	}

	stateDir := localserver.StateDir()
	certsDir := filepath.Join(stateDir, "certs")

	cmd.Flags().StringVar(&stepCAURL, "step-ca-url", "", "Step CA server URL (e.g. https://step-ca-dev.example.com)")
	_ = cmd.MarkFlagRequired("step-ca-url")
	cmd.Flags().StringVar(&rootPath, "step-ca-root", filepath.Join(certsDir, "step-ca-root.crt"), "path to Step CA HTTPS root certificate")
	cmd.Flags().StringVar(&certPath, "cert", "", "path to client certificate to renew (default: <state-dir>/certs/<hostname>.crt)")
	cmd.Flags().StringVar(&keyPath, "key", "", "path to client private key (default: <state-dir>/certs/<hostname>.key)")
	cmd.Flags().DurationVar(&before, "before", 0, "only renew if the cert expires within this duration (e.g. 2h); 0 always renews")

	return cmd
}

func runClientRenew(stepCAURL, rootPath string, rootExplicit bool, certPath, keyPath string, before time.Duration) error {
	stepCAURL = strings.TrimRight(stepCAURL, "/")

	stateDir := localserver.StateDir()
	certsDir := filepath.Join(stateDir, "certs")

	// Default cert/key to <state-dir>/certs/<hostname>.{crt,key}
	if certPath == "" || keyPath == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("resolve default cert path: %w", err)
		}
		if certPath == "" {
			certPath = filepath.Join(certsDir, hostname+".crt")
		}
		if keyPath == "" {
			keyPath = filepath.Join(certsDir, hostname+".key")
		}
	}

	// Parse the existing cert to check expiry and show info.
	existing, err := loadLeafCert(certPath)
	if err != nil {
		return fmt.Errorf("read existing cert: %w", err)
	}

	fmt.Printf("Current certificate:\n")
	fmt.Printf("  CN:      %s\n", existing.Subject.CommonName)
	fmt.Printf("  Expires: %s (%s)\n", existing.NotAfter.Format(time.RFC3339), timeUntil(existing.NotAfter))

	if time.Now().After(existing.NotAfter) {
		nameFlag := ""
		if existing.Subject.CommonName != "" {
			nameFlag = fmt.Sprintf(" --name %s", existing.Subject.CommonName)
		}
		return fmt.Errorf("certificate expired on %s; Step CA renewal requires an unexpired certificate. Re-issue it with: bridgectl client init --step-ca-url %s%s",
			existing.NotAfter.Format(time.RFC3339), stepCAURL, nameFlag)
	}

	// --before: skip if not expiring soon enough.
	if before > 0 && time.Until(existing.NotAfter) > before {
		fmt.Printf("\nCert not due for renewal (expires in %s, threshold %s). Nothing to do.\n",
			timeUntil(existing.NotAfter), before)
		return nil
	}

	fmt.Printf("\nRenewing certificate from %s...\n", stepCAURL)

	rootArg, mode, err := renewRootArg(stepCAURL, rootPath, rootExplicit)
	if err != nil {
		return err
	}
	if mode == renewModeNativeInsecureTLS {
		if err := renewClientCertNativeWithVerifiedFallback(stepCAURL, certPath, keyPath); err != nil {
			return fmt.Errorf("native token renew: %w", err)
		}
		return printRenewedCert(certPath)
	}

	stepBin, err := exec.LookPath("step")
	if err != nil {
		return fmt.Errorf("'step' CLI not found on PATH — required for certificate renewal; install from https://smallstep.com/cli/: %w", err)
	}

	// Force signed-token renewal instead of mTLS. This works through layer-7
	// HTTPS front doors such as Tailscale Serve/Funnel.
	args := []string{
		"ca", "renew",
		"--ca-url", stepCAURL,
		"--mtls=false",
	}
	if rootArg != "" {
		args = append(args, "--root", rootArg)
	}
	args = append(args,
		"--force",
		certPath,
		keyPath,
	)
	cmd := exec.Command(stepBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("step ca renew: %w", err)
	}

	return printRenewedCert(certPath)
}

func printRenewedCert(certPath string) error {
	renewed, err := loadLeafCert(certPath)
	if err != nil {
		return fmt.Errorf("read renewed cert: %w", err)
	}

	fmt.Printf("\nRenewed certificate:\n")
	fmt.Printf("  Cert:    %s\n", certPath)
	fmt.Printf("  Expires: %s (%s)\n", renewed.NotAfter.Format(time.RFC3339), timeUntil(renewed.NotAfter))

	return nil
}

func renewRootArg(stepCAURL, rootPath string, rootExplicit bool) (string, renewMode, error) {
	if rootExplicit {
		if _, err := os.Stat(rootPath); err != nil {
			return "", renewModeStepCLI, fmt.Errorf("read Step CA root %s: %w", rootPath, err)
		}
		return rootPath, renewModeStepCLI, nil
	}

	if rootPath == "" {
		return "", renewModeStepCLI, nil
	}

	if _, err := os.Stat(rootPath); os.IsNotExist(err) {
		fmt.Printf("Fetching root cert from %s...\n", stepCAURL)
		if err := fetchStepCARoot(stepCAURL, rootPath); err != nil {
			return "", renewModeStepCLI, fmt.Errorf("fetch Step CA root cert: %w", err)
		}
		fmt.Printf("  Saved to %s\n", rootPath)
	} else if err != nil {
		return "", renewModeStepCLI, fmt.Errorf("read Step CA root %s: %w", rootPath, err)
	}

	if err := verifyStepCARoot(stepCAURL, rootPath); err == nil {
		return rootPath, renewModeStepCLI, nil
	}

	fmt.Printf("Cached root cert does not verify %s; refreshing...\n", stepCAURL)
	if err := fetchStepCARoot(stepCAURL, rootPath); err != nil {
		return "", renewModeStepCLI, fmt.Errorf("fetch Step CA root cert: %w", err)
	}
	fmt.Printf("  Updated root cert saved to %s\n", rootPath)

	if err := verifyStepCARoot(stepCAURL, rootPath); err == nil {
		return rootPath, renewModeStepCLI, nil
	}

	fmt.Printf("Step CA root does not verify the HTTPS endpoint; using native token renewal for %s.\n", stepCAURL)
	return "", renewModeNativeInsecureTLS, nil
}

func renewClientCertNativeWithVerifiedFallback(stepCAURL, certPath, keyPath string) error {
	err := renewClientCertNative(stepCAURL, certPath, keyPath, false)
	if err == nil {
		return nil
	}
	if !isTLSVerificationError(err) {
		return err
	}
	return renewClientCertNative(stepCAURL, certPath, keyPath, true)
}

func renewClientCertNative(stepCAURL, certPath, keyPath string, insecureTLS bool) error {
	token, err := renewalToken(stepCAURL, certPath, keyPath)
	if err != nil {
		return fmt.Errorf("create renewal token: %w", err)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: insecureTLS, //nolint:gosec // fallback only after fetching the CA root from this endpoint and finding it is not the HTTPS serving root.
		},
	}
	client, err := ca.NewClient(stepCAURL, ca.WithTransport(tr))
	if err != nil {
		return fmt.Errorf("create Step CA client: %w", err)
	}
	resp, err := client.RenewWithToken(token)
	if err != nil {
		return fmt.Errorf("renew with token: %w", err)
	}
	if err := localserver.WriteCertPEM(certPath, resp); err != nil {
		return fmt.Errorf("write renewed certificate: %w", err)
	}
	return nil
}

func isTLSVerificationError(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostnameError x509.HostnameError
	if errors.As(err, &hostnameError) {
		return true
	}
	var invalidCert x509.CertificateInvalidError
	if errors.As(err, &invalidCert) {
		return true
	}
	var certVerifyErr *tls.CertificateVerificationError
	return errors.As(err, &certVerifyErr)
}

func renewalToken(stepCAURL, certPath, keyPath string) (string, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return "", err
	}
	if len(cert.Certificate) == 0 {
		return "", fmt.Errorf("no certificates in %s", certPath)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", err
	}
	if time.Now().After(leaf.NotAfter) {
		return "", fmt.Errorf("certificate expired on %s; Step CA renewal requires an unexpired certificate", leaf.NotAfter.Format(time.RFC3339))
	}

	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return "", fmt.Errorf("private key in %s does not implement crypto.Signer", keyPath)
	}
	alg, err := signingAlgorithm(signer.Public())
	if err != nil {
		return "", err
	}

	x5c := make([]string, 0, len(cert.Certificate))
	for _, der := range cert.Certificate {
		x5c = append(x5c, base64.StdEncoding.EncodeToString(der))
	}

	now := time.Now().UTC()
	audience, err := renewAudience(stepCAURL)
	if err != nil {
		return "", err
	}
	claims := jose.Claims{
		Audience:  []string{audience},
		Subject:   leaf.Subject.CommonName,
		Issuer:    "step-ca-client/1.0",
		NotBefore: jose.NewNumericDate(now),
		IssuedAt:  jose.NewNumericDate(now),
		Expiry:    jose.NewNumericDate(now.Add(5 * time.Minute)),
	}
	opts := new(jose.SignerOptions).WithType("JWT").WithHeader("x5cInsecure", x5c)
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: signer}, opts)
	if err != nil {
		return "", err
	}
	return jose.Signed(sig).Claims(claims).CompactSerialize()
}

func renewAudience(stepCAURL string) (string, error) {
	u, err := url.Parse(stepCAURL)
	if err != nil {
		return "", err
	}
	return u.ResolveReference(&url.URL{Path: "/renew"}).String(), nil
}

func signingAlgorithm(pub crypto.PublicKey) (jose.SignatureAlgorithm, error) {
	switch key := pub.(type) {
	case *ecdsa.PublicKey:
		switch key.Curve.Params().BitSize {
		case 256:
			return jose.ES256, nil
		case 384:
			return jose.ES384, nil
		case 521:
			return jose.ES512, nil
		default:
			return "", fmt.Errorf("unsupported ECDSA curve size %d", key.Curve.Params().BitSize)
		}
	case *rsa.PublicKey:
		return jose.RS256, nil
	case ed25519.PublicKey:
		return jose.EdDSA, nil
	default:
		return "", fmt.Errorf("unsupported private key public type %T", pub)
	}
}

// loadLeafCert reads a PEM cert file and returns the first (leaf) certificate.
func loadLeafCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func timeUntil(t time.Time) string {
	d := time.Until(t)
	if d < 0 {
		return "EXPIRED"
	}
	h := int(d.Hours())
	switch {
	case h >= 24:
		return fmt.Sprintf("%dd", h/24)
	case h >= 1:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}
