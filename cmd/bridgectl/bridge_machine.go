package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Non-interactive enrollment: `bridgectl login --non-interactive` enrolls with
// a credential from the environment and never opens a browser or prompts.
//
// Two credential kinds are accepted (exactly one must be configured):
//
//	BRIDGECTL_CLIENT_ID + BRIDGECTL_CLIENT_SECRET
//	    A ZITADEL service account (client_credentials). Also requires
//	    BRIDGECTL_OIDC_ISSUER and BRIDGECTL_OIDC_PROJECT_ID, which Bridge shows
//	    next to the generated secret.
//	BRIDGECTL_ENROLL_KEY
//	    A Bridge-issued, expiring, use-limited enrollment key (bek_...).
//
// Secrets are read only from the environment, never argv, and are never
// printed. The organization comes from the credential, not from a flag.

const (
	envClientID   = "BRIDGECTL_CLIENT_ID"
	envClientPass = "BRIDGECTL_CLIENT_SECRET"
	envIssuer     = "BRIDGECTL_OIDC_ISSUER"
	envProjectID  = "BRIDGECTL_OIDC_PROJECT_ID"
	envEnrollKey  = "BRIDGECTL_ENROLL_KEY"
)

var (
	enrollKeyPattern = regexp.MustCompile(`^bek_[A-Za-z0-9_-]{43}$`)
	projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type machineCredential struct {
	enrollKey, clientID, clientSecret, issuer, projectID string
}

// machineCredentialFromEnv fails closed: a missing or ambiguous configuration
// is an error, never a fallback to the interactive device flow.
func machineCredentialFromEnv(getenv func(string) string) (machineCredential, error) {
	c := machineCredential{
		enrollKey:    strings.TrimSpace(getenv(envEnrollKey)),
		clientID:     strings.TrimSpace(getenv(envClientID)),
		clientSecret: getenv(envClientPass),
		issuer:       strings.TrimSpace(getenv(envIssuer)),
		projectID:    strings.TrimSpace(getenv(envProjectID)),
	}
	hasKey, hasClient := c.enrollKey != "", c.clientID != "" || c.clientSecret != ""
	switch {
	case hasKey && hasClient:
		return c, fmt.Errorf("set either %s or %s/%s, not both", envEnrollKey, envClientID, envClientPass)
	case hasKey:
		if !enrollKeyPattern.MatchString(c.enrollKey) {
			return c, fmt.Errorf("%s is not a valid enrollment key", envEnrollKey)
		}
		return c, nil
	case hasClient:
		if c.clientID == "" || c.clientSecret == "" {
			return c, fmt.Errorf("%s and %s must both be set", envClientID, envClientPass)
		}
		if c.issuer == "" || !projectIDPattern.MatchString(c.projectID) {
			return c, fmt.Errorf("%s and %s are required with a client secret", envIssuer, envProjectID)
		}
		if err := validateHTTPSURL(c.issuer); err != nil {
			return c, fmt.Errorf("%s: %w", envIssuer, err)
		}
		return c, nil
	}
	return c, fmt.Errorf("--non-interactive needs %s, or %s/%s with %s and %s", envEnrollKey, envClientID, envClientPass, envIssuer, envProjectID)
}

// accessToken returns the bearer credential for /v1/device/enroll.
func (c machineCredential) accessToken(ctx context.Context, client *http.Client) (string, error) {
	if c.enrollKey != "" {
		return c.enrollKey, nil
	}
	issuer, _ := url.Parse(c.issuer)
	var disc struct {
		Issuer        string `json:"issuer"`
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := getJSON(ctx, client, strings.TrimSuffix(c.issuer, "/")+"/.well-known/openid-configuration", &disc); err != nil {
		return "", fmt.Errorf("OIDC discovery: %w", err)
	}
	endpoint, err := url.Parse(disc.TokenEndpoint)
	if disc.Issuer != c.issuer || err != nil || validateHTTPSURL(disc.TokenEndpoint) != nil || endpoint.Host != issuer.Host {
		return "", errors.New("OIDC discovery returned an untrusted token endpoint")
	}
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {"openid urn:zitadel:iam:org:project:id:" + c.projectID + ":aud urn:zitadel:iam:org:projects:roles urn:zitadel:iam:org:project:role:bridge:enroll"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, disc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// RFC 6749 §2.3.1: form-encode id and secret before Basic encoding.
	req.SetBasicAuth(url.QueryEscape(c.clientID), url.QueryEscape(c.clientSecret))
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The body can describe the failure but never contains our secret.
		return "", fmt.Errorf("identity provider rejected the client credentials (HTTP %d)", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if json.Unmarshal(data, &tok) != nil || tok.AccessToken == "" || !strings.EqualFold(tok.TokenType, "bearer") {
		return "", errors.New("identity provider returned an invalid token response")
	}
	return tok.AccessToken, nil
}

func getJSON(ctx context.Context, client *http.Client, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// enrollMachine exchanges the environment credential for the same enrollment
// a browser approval produces.
func enrollMachine(ctx context.Context, client *http.Client, base, name string, c machineCredential) (*deviceToken, error) {
	token, err := c.accessToken(ctx, client)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"display_name": name})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/device/enroll", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Bridge enrollment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("bridge refused the machine credential (HTTP %d); check it is registered, unexpired and not revoked", resp.StatusCode)
	case http.StatusConflict:
		return nil, errors.New("bridge refused enrollment: installation limit reached for this credential")
	default:
		return nil, fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}
	var tok deviceToken
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("decode bridge response: %w", err)
	}
	return &tok, nil
}

func runBridgeMachineLogin(cmd *cobra.Command, bridge, name string, force bool) error {
	ctx, cancel := signalContext(cmd.Context())
	defer cancel()
	out := cmd.OutOrStdout()
	if _, _, err := readEnrollment(); err == nil && !force {
		_, _ = fmt.Fprintln(out, "Already logged into Bridge.\nUse --force to re-enroll.")
		return activateBridgeEnrollment(ctx, cmd)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) && !force {
		return fmt.Errorf("read existing enrollment: %w", err)
	}
	cred, err := machineCredentialFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	base, err := bridgeURL(bridge)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	installation := installationName(name)
	tok, err := enrollMachine(ctx, client, base, installation, cred)
	if err != nil {
		return err
	}
	if err := persistBridgeEnrollment(*tok, installation, base); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "✓ Machine credential accepted\n✓ Installation registered\n✓ Telemetry configured")
	return activateBridgeEnrollment(ctx, cmd)
}
