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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/orchael/bridgectl/internal/localserver"
)

const productionBridgeURL = "https://bridge.orchael.com"

type bridgeEnrollment struct {
	BridgeURL         string `json:"bridge_url"`
	OrganizationID    string `json:"organization_id"`
	OrganizationName  string `json:"organization_name,omitempty"`
	OrganizationURL   string `json:"organization_url,omitempty"`
	InstallationID    string `json:"installation_id"`
	InstallationName  string `json:"installation_name,omitempty"`
	TelemetryEndpoint string `json:"telemetry_endpoint"`
}
type bridgeSecret struct {
	CollectorCredential string `json:"collector_credential"`
}

func bridgeStatePaths() (string, string) {
	d := localserver.StateDir()
	return filepath.Join(d, "bridge-enrollment.json"), filepath.Join(d, "bridge-credentials.json")
}
func bridgeURL(flag string) (string, error) {
	raw := strings.TrimSpace(flag)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("BRIDGECTL_BRIDGE_URL"))
	}
	if raw == "" {
		// A missing/unreadable credential file must not hide the saved
		// origin: the documented precedence only needs the enrollment
		// metadata to pick the previously enrolled Bridge, not a working
		// credential.
		if enrollment, err := readEnrollmentMetadata(); err == nil {
			raw = strings.TrimSpace(enrollment.BridgeURL)
		}
	}
	if raw == "" {
		raw = productionBridgeURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("bridge URL must be an HTTPS origin")
	}
	return strings.TrimRight(raw, "/"), nil
}
func readEnrollment() (*bridgeEnrollment, *bridgeSecret, error) {
	e, err := readEnrollmentMetadata()
	if err != nil {
		return nil, nil, err
	}
	s, err := readBridgeSecret()
	if err != nil {
		return e, nil, err
	}
	return e, s, nil
}
func readEnrollmentMetadata() (*bridgeEnrollment, error) {
	mp, _ := bridgeStatePaths()
	b, err := secureRead(mp)
	if err != nil {
		return nil, err
	}
	var e bridgeEnrollment
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}
func readBridgeSecret() (*bridgeSecret, error) {
	_, sp := bridgeStatePaths()
	b, err := secureRead(sp)
	if err != nil {
		return nil, err
	}
	var s bridgeSecret
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
func secureRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("bridge state file is not regular")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("bridge state file %q has insecure permissions", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0600)
	return b, nil
}
func atomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, append(b, '\n'), 0600)
}

// snapshotForRestore captures the current contents of mp and sp (if any) and
// returns a function that restores exactly that prior state: the original
// bytes for a file that existed, or removal for one that did not. Used to
// undo a failed --force re-enrollment without destroying a still-valid
// enrollment that existed before this login attempt.
func snapshotForRestore(mp, sp string) func() {
	readOrNil := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		return b
	}
	oldMP, oldSP := readOrNil(mp), readOrNil(sp)
	restoreOne := func(path string, old []byte) {
		if old == nil {
			_ = os.Remove(path)
			return
		}
		_ = atomicWriteFile(path, old, 0600)
	}
	return func() {
		restoreOne(mp, oldMP)
		restoreOne(sp, oldSP)
	}
}

// atomicWriteFile writes data to path via a temp file + rename in the same
// directory, so a process interruption or concurrent reader can never
// observe a truncated or partially written file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err = tmp.Chmod(perm); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func httpJSON(ctx context.Context, client *http.Client, method, endpoint string, body any, out any) (int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err = json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode bridge response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}
type deviceToken struct {
	BridgeURL           string `json:"bridge_url"`
	APIVersion          string `json:"api_version"`
	OrganizationID      string `json:"organization_id"`
	OrganizationName    string `json:"organization_name"`
	OrganizationURL     string `json:"organization_url"`
	InstallationID      string `json:"installation_id"`
	TelemetryEndpoint   string `json:"telemetry_endpoint"`
	CollectorCredential string `json:"collector_credential"`
}

// supportedDeviceAPIVersions are the device-enrollment protocol versions this
// build understands. Bridge documents api_version as a versioned protocol
// marker (docs/bridgectl-device-enrollment.md), not a free-form string; an
// unrecognized value must fail enrollment explicitly instead of silently
// persisting a credential issued under a protocol this build cannot honor.
var supportedDeviceAPIVersions = map[string]bool{"v1": true}

// collectorCredentialPattern is shared with the server-start read path
// (internal/localserver.CollectorCredentialPattern) so a malformed
// credential is rejected the same way on write and on read.
var collectorCredentialPattern = localserver.CollectorCredentialPattern

type deviceTokenError struct {
	Error    string `json:"error"`
	Interval int    `json:"interval"`
}

// pollDeviceToken calls /v1/device/token directly rather than through
// httpJSON: the documented protocol (docs/bridgectl-device-enrollment.md)
// requires reading the granted polling interval from both the JSON body and
// the Retry-After header on a slow_down response, which a generic
// status-code check cannot do.
func pollDeviceToken(ctx context.Context, client *http.Client, endpoint, deviceCode string) (*deviceToken, *deviceTokenError, time.Duration, error) {
	body, err := json.Marshal(map[string]string{"device_code": deviceCode})
	if err != nil {
		return nil, nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, 0, err
	}
	var retryAfter time.Duration
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, convErr := strconv.Atoi(ra); convErr == nil && secs > 0 {
			retryAfter = time.Duration(secs) * time.Second
		}
	}
	if resp.StatusCode == 200 {
		var tok deviceToken
		if err := json.Unmarshal(data, &tok); err != nil {
			return nil, nil, retryAfter, fmt.Errorf("decode bridge response: %w", err)
		}
		return &tok, nil, retryAfter, nil
	}
	var derr deviceTokenError
	_ = json.Unmarshal(data, &derr)
	if derr.Error == "" {
		return nil, nil, retryAfter, fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil, &derr, retryAfter, nil
}

// nextSlowDownInterval implements the documented slow_down contract: the
// client must keep the greatest of its current interval, the server-granted
// interval, and Retry-After, and must never reduce it. There is deliberately
// no upper clamp — the outer authorization deadline already bounds
// worst-case wait time, and capping here would violate "never reduce" if
// Bridge legitimately asks for a longer wait under abuse mitigation.
func nextSlowDownInterval(current, granted, retryAfter time.Duration) time.Duration {
	next := current
	if granted > next {
		next = granted
	}
	if retryAfter > next {
		next = retryAfter
	}
	return next
}

func newBridgeLoginCmd() *cobra.Command {
	var bridge string
	var organization string
	var force bool
	cmd := &cobra.Command{Use: "login", Short: "Log into Bridge and enroll this installation", RunE: func(cmd *cobra.Command, _ []string) error { return runBridgeLogin(cmd, bridge, organization, force) }}
	cmd.Flags().StringVar(&bridge, "bridge", "", "Bridge HTTPS origin")
	cmd.Flags().StringVar(&organization, "organization", "", "default organization name to request")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing enrollment")
	return cmd
}
func defaultOrganization(flag string) string {
	if raw := strings.TrimSpace(flag); raw != "" {
		return raw
	}
	return strings.TrimSpace(os.Getenv("BRIDGECTL_ORGANIZATION"))
}
func authorizeRequestBody(organization string) map[string]string {
	body := map[string]string{"display_name": installationName()}
	if org := defaultOrganization(organization); org != "" {
		body["requested_organization"] = org
	}
	return body
}
func runBridgeLogin(cmd *cobra.Command, bridge, organization string, force bool) error {
	if _, _, err := readEnrollment(); err == nil && !force {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Already logged into Bridge.")
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Use --force to re-enroll.")
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) && !force {
		return fmt.Errorf("read existing enrollment: %w", err)
	}
	base, err := bridgeURL(bridge)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := signalContext(cmd.Context())
	defer cancel()
	var auth deviceAuthorization
	if _, err = httpJSON(ctx, client, "POST", base+"/v1/device/authorize", authorizeRequestBody(organization), &auth); err != nil {
		return fmt.Errorf("request Bridge authorization: %w", err)
	}
	if auth.ExpiresIn <= 0 || auth.ExpiresIn > 900 || auth.Interval <= 0 || auth.Interval > 60 {
		return fmt.Errorf("invalid Bridge authorization response")
	}
	// A compromised or misconfigured Bridge response could point the browser
	// at an attacker-controlled origin, tricking the user into entering their
	// real device code (or Google credentials) at a phishing site. Validate
	// both returned URLs match the Bridge origin we requested before ever
	// printing or opening them, per the documented protocol.
	if err := validateBridgeOriginURL(auth.VerificationURI, base, false); err != nil {
		return fmt.Errorf("invalid Bridge verification URI: %w", err)
	}
	if auth.VerificationURIComplete != "" {
		if err := validateBridgeOriginURL(auth.VerificationURIComplete, base, true); err != nil {
			return fmt.Errorf("invalid Bridge verification URI: %w", err)
		}
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Opening %s\n\nAuthorization code:\n\n    %s\n\n", auth.VerificationURI, auth.UserCode)
	if auth.VerificationURIComplete == "" {
		auth.VerificationURIComplete = auth.VerificationURI + "?user_code=" + url.QueryEscape(auth.UserCode)
	}
	if err := openBrowser(auth.VerificationURIComplete); err != nil {
		_, _ = fmt.Fprintf(out, "Open this URL in your browser: %s\n\n", auth.VerificationURIComplete)
	}
	_, _ = fmt.Fprintln(out, "Waiting for authorization...")
	deadline := time.NewTimer(time.Duration(auth.ExpiresIn) * time.Second)
	defer deadline.Stop()
	interval := time.Duration(auth.Interval) * time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("bridge authorization expired")
		case <-time.After(interval):
		}
		tok, derr, retryAfter, err := pollDeviceToken(ctx, client, base+"/v1/device/token", auth.DeviceCode)
		if err != nil {
			return fmt.Errorf("bridge authorization: %w", err)
		}
		if tok != nil {
			if err := persistBridgeEnrollment(*tok, installationName()); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(out, "✓ Bridge authorization complete\n✓ Installation registered\n✓ Organization selected\n✓ Telemetry configured")
			return nil
		}
		switch derr.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval = nextSlowDownInterval(interval, time.Duration(derr.Interval)*time.Second, retryAfter)
			continue
		default:
			return fmt.Errorf("bridge authorization: %s", derr.Error)
		}
	}
}
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt)
}
func installationName() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "bridgectl"
	}
	return h
}
func openBrowser(target string) error {
	for _, name := range []string{"xdg-open", "open"} {
		if _, err := exec.LookPath(name); err == nil {
			return exec.Command(name, target).Start()
		}
	}
	return errors.New("no browser opener found")
}
func persistBridgeEnrollment(tok deviceToken, name string) error {
	if !supportedDeviceAPIVersions[tok.APIVersion] {
		return fmt.Errorf("unsupported Bridge device-enrollment api_version %q", tok.APIVersion)
	}
	validatedBridge, err := bridgeURL(tok.BridgeURL)
	if err != nil {
		return fmt.Errorf("invalid Bridge URL: %w", err)
	}
	if err := validateHTTPSURL(tok.TelemetryEndpoint); err != nil {
		return fmt.Errorf("invalid telemetry endpoint: %w", err)
	}
	if tok.OrganizationID == "" || tok.InstallationID == "" || tok.CollectorCredential == "" {
		return errors.New("bridge returned incomplete enrollment")
	}
	if !collectorCredentialPattern.MatchString(tok.CollectorCredential) {
		return errors.New("bridge returned a malformed collector credential")
	}
	orgURL := strings.TrimSpace(tok.OrganizationURL)
	if orgURL != "" {
		if err := validateHTTPSURL(orgURL); err != nil {
			return fmt.Errorf("invalid organization URL: %w", err)
		}
	}
	e := bridgeEnrollment{BridgeURL: validatedBridge, OrganizationID: tok.OrganizationID, OrganizationName: strings.TrimSpace(tok.OrganizationName), OrganizationURL: orgURL, InstallationID: tok.InstallationID, InstallationName: name, TelemetryEndpoint: tok.TelemetryEndpoint}
	mp, sp := bridgeStatePaths()
	// With --force, mp/sp may already hold a working enrollment. Capture it
	// before overwriting so a later failure restores it instead of just
	// deleting the files (which would silently log a still-valid enrollment
	// out on a transient write/config error).
	restore := snapshotForRestore(mp, sp)
	if err := atomicJSON(mp, e); err != nil {
		return fmt.Errorf("save Bridge enrollment: %w", err)
	}
	if err := atomicJSON(sp, bridgeSecret{CollectorCredential: tok.CollectorCredential}); err != nil {
		restore()
		return fmt.Errorf("save Bridge credential: %w", err)
	}
	if err := configureBridgeTelemetry(e, mp); err != nil {
		restore()
		return fmt.Errorf("configure Bridge telemetry: %w", err)
	}
	return nil
}
func validateHTTPSURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("must be an HTTPS URL without credentials, query, or fragment")
	}
	return nil
}

// validateBridgeOriginURL checks that raw is an HTTPS URL with no
// credentials or fragment whose scheme+host exactly matches expectedOrigin
// (an already-validated "https://host" origin). allowQuery permits a query
// string, needed for verification_uri_complete's ?user_code=... parameter.
func validateBridgeOriginURL(raw, expectedOrigin string, allowQuery bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || (!allowQuery && (u.RawQuery != "" || u.ForceQuery)) {
		return errors.New("must be an HTTPS URL without credentials or fragment")
	}
	if origin := u.Scheme + "://" + u.Host; origin != expectedOrigin {
		return fmt.Errorf("origin %q does not match Bridge origin %q", origin, expectedOrigin)
	}
	return nil
}

// bridgeConfigPath resolves the same config file server start would use
// (localserver.StateDir()/bridge.yaml, an XDG config, or a legacy config
// path — see defaultServerConfigPath), falling back to the default
// stateDir/bridge.yaml only when none of those candidates exist yet. Writing
// to a hardcoded stateDir/bridge.yaml instead would, the first time it's
// created, silently outrank an existing XDG config in that resolution
// order — shadowing the user's real providers/security/session settings on
// every later server start.
func bridgeConfigPath() string {
	stateDir := localserver.StateDir()
	if p := defaultServerConfigPath(stateDir); p != "" {
		return p
	}
	return filepath.Join(stateDir, "bridge.yaml")
}

func hasExplicitCollector(t map[string]any) bool {
	url, _ := t["collector_url"].(string)
	target, _ := t["collector_target"].(string)
	return url != "" || target != ""
}

func configureBridgeTelemetry(e bridgeEnrollment, metadataPath string) error {
	path := bridgeConfigPath()
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &m); err != nil {
			return err
		}
	}
	t, ok := m["telemetry"].(map[string]any)
	if !ok {
		t = map[string]any{}
	}
	managed, _ := t["managed_by_bridge"].(bool)
	if !hasExplicitCollector(t) || managed {
		t["collector_url"] = e.TelemetryEndpoint
		t["collector_credential_file"] = filepath.Join(localserver.StateDir(), "bridge-credentials.json")
		// Only default enabled to true when the user has not already set it
		// (to either value): forcing it on would override an explicit
		// enabled: false left alongside other standalone telemetry options.
		// managed_enabled_default records that Bridge, not the user, set it,
		// so logout knows to remove it again.
		if _, exists := t["enabled"]; !exists {
			t["enabled"] = true
			t["managed_enabled_default"] = true
		}
		t["managed_by_bridge"] = true
		m["telemetry"] = t
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, b, 0600)
}

func newBridgeWhoamiCmd() *cobra.Command {
	return &cobra.Command{Use: "whoami", Short: "Show Bridge enrollment status", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := readEnrollmentMetadata()
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Not logged into Bridge.")
			return nil
		}
		if err != nil {
			return err
		}
		status := "logged in"
		if _, secretErr := readBridgeSecret(); secretErr != nil {
			status = "credential missing"
		}
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "Bridge          %s\nOrganization    %s\n", e.BridgeURL, display(e.OrganizationName, e.OrganizationID))
		if e.OrganizationURL != "" {
			_, _ = fmt.Fprintf(out, "Org URL         %s\n", e.OrganizationURL)
		}
		_, _ = fmt.Fprintf(out, "Installation    %s\nStatus          %s\n", display(e.InstallationName, e.InstallationID), status)
		return nil
	}}
}
func display(name, id string) string {
	if name != "" {
		return name
	}
	return id
}
func newBridgeLogoutCmd() *cobra.Command {
	return &cobra.Command{Use: "logout", Short: "Log out of Bridge", RunE: func(cmd *cobra.Command, _ []string) error {
		mp, sp := bridgeStatePaths()
		_, mpErr := os.Stat(mp)
		_, spErr := os.Stat(sp)
		if errors.Is(mpErr, os.ErrNotExist) && errors.Is(spErr, os.ErrNotExist) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Not logged into Bridge.")
			return nil
		}
		if err := removeManagedTelemetry(); err != nil {
			return fmt.Errorf("update telemetry configuration: %w", err)
		}
		if err := os.Remove(mp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Bridge enrollment removed locally.")
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Remote revocation is not exposed by this Bridge protocol.")
		return nil
	}}
}

func removeManagedTelemetry() error {
	path := bridgeConfigPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return err
	}
	t, ok := m["telemetry"].(map[string]any)
	if !ok {
		return nil
	}
	managed, _ := t["managed_by_bridge"].(bool)
	if !managed {
		return nil
	}
	delete(t, "collector_url")
	delete(t, "collector_credential_file")
	delete(t, "managed_by_bridge")
	// Only remove enabled if login set it itself (managed_enabled_default);
	// an enabled value the user already had (true or false) alongside other
	// standalone telemetry options must survive logout untouched.
	if addedEnabled, _ := t["managed_enabled_default"].(bool); addedEnabled {
		delete(t, "enabled")
	}
	delete(t, "managed_enabled_default")
	if len(t) == 0 {
		delete(m, "telemetry")
	} else {
		m["telemetry"] = t
	}
	// A config file that login created solely to hold managed enrollment
	// settings and that now has nothing left in it must be removed, not
	// written back empty: otherwise it keeps outranking (and shadowing) an
	// existing XDG config in server start's resolution order forever.
	if len(m) == 0 && path == filepath.Join(localserver.StateDir(), "bridge.yaml") {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, out, 0600)
}
