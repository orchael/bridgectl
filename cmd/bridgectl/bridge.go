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
		if enrollment, _, err := readEnrollment(); err == nil {
			raw = strings.TrimSpace(enrollment.BridgeURL)
		}
	}
	if raw == "" {
		raw = productionBridgeURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("bridge URL must be an HTTPS origin")
	}
	return strings.TrimRight(raw, "/"), nil
}
func readEnrollment() (*bridgeEnrollment, *bridgeSecret, error) {
	mp, sp := bridgeStatePaths()
	var e bridgeEnrollment
	var s bridgeSecret
	b, err := secureRead(mp)
	if err != nil {
		return nil, nil, err
	}
	if err = json.Unmarshal(b, &e); err != nil {
		return nil, nil, err
	}
	b, err = secureRead(sp)
	if err != nil {
		return nil, nil, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, nil, err
	}
	return &e, &s, nil
}
func secureRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Bridge state file is not regular")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("Bridge state file %q has insecure permissions", path)
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
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(append(b, '\n'))
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
	defer resp.Body.Close()
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
	OrganizationID      string `json:"organization_id"`
	InstallationID      string `json:"installation_id"`
	TelemetryEndpoint   string `json:"telemetry_endpoint"`
	CollectorCredential string `json:"collector_credential"`
}

func newBridgeLoginCmd() *cobra.Command {
	var bridge string
	var force bool
	cmd := &cobra.Command{Use: "login", Short: "Log into Bridge and enroll this installation", RunE: func(cmd *cobra.Command, _ []string) error { return runBridgeLogin(cmd, bridge, force) }}
	cmd.Flags().StringVar(&bridge, "bridge", "", "Bridge HTTPS origin")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing enrollment")
	return cmd
}
func runBridgeLogin(cmd *cobra.Command, bridge string, force bool) error {
	if _, _, err := readEnrollment(); err == nil && !force {
		fmt.Fprintln(cmd.OutOrStdout(), "Already logged into Bridge.")
		fmt.Fprintln(cmd.OutOrStdout(), "Use --force to re-enroll.")
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
	if _, err = httpJSON(ctx, client, "POST", base+"/v1/device/authorize", map[string]string{"display_name": installationName()}, &auth); err != nil {
		return fmt.Errorf("request Bridge authorization: %w", err)
	}
	if auth.ExpiresIn <= 0 || auth.ExpiresIn > 900 || auth.Interval <= 0 || auth.Interval > 60 {
		return fmt.Errorf("invalid Bridge authorization response")
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Opening %s\n\nAuthorization code:\n\n    %s\n\n", auth.VerificationURI, auth.UserCode)
	if auth.VerificationURIComplete == "" {
		auth.VerificationURIComplete = auth.VerificationURI + "?user_code=" + url.QueryEscape(auth.UserCode)
	}
	if err := openBrowser(auth.VerificationURIComplete); err != nil {
		fmt.Fprintf(out, "Open this URL in your browser: %s\n\n", auth.VerificationURIComplete)
	}
	fmt.Fprintln(out, "Waiting for authorization...")
	deadline := time.NewTimer(time.Duration(auth.ExpiresIn) * time.Second)
	defer deadline.Stop()
	interval := time.Duration(auth.Interval) * time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Bridge authorization expired")
		case <-time.After(interval):
		}
		var tok deviceToken
		status, pollErr := httpJSON(ctx, client, "POST", base+"/v1/device/token", map[string]string{"device_code": auth.DeviceCode}, &tok)
		if pollErr == nil {
			if err := persistBridgeEnrollment(tok, installationName()); err != nil {
				return err
			}
			fmt.Fprintln(out, "✓ Bridge authorization complete\n✓ Installation registered\n✓ Organization selected\n✓ Telemetry configured")
			return nil
		}
		if status == 429 {
			interval *= 2
			if interval > 60*time.Second {
				interval = 60 * time.Second
			}
			continue
		}
		if status == 400 && strings.Contains(pollErr.Error(), "authorization_pending") {
			continue
		}
		return fmt.Errorf("Bridge authorization: %w", pollErr)
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
	if tok.BridgeURL == "" || tok.OrganizationID == "" || tok.InstallationID == "" || tok.TelemetryEndpoint == "" || tok.CollectorCredential == "" {
		return errors.New("Bridge returned incomplete enrollment")
	}
	e := bridgeEnrollment{BridgeURL: tok.BridgeURL, OrganizationID: tok.OrganizationID, InstallationID: tok.InstallationID, InstallationName: name, TelemetryEndpoint: tok.TelemetryEndpoint}
	mp, sp := bridgeStatePaths()
	if err := atomicJSON(mp, e); err != nil {
		return fmt.Errorf("save Bridge enrollment: %w", err)
	}
	if err := atomicJSON(sp, bridgeSecret{CollectorCredential: tok.CollectorCredential}); err != nil {
		return fmt.Errorf("save Bridge credential: %w", err)
	}
	if err := configureBridgeTelemetry(e, mp); err != nil {
		_ = os.Remove(mp)
		_ = os.Remove(sp)
		return fmt.Errorf("configure Bridge telemetry: %w", err)
	}
	return nil
}
func configureBridgeTelemetry(e bridgeEnrollment, metadataPath string) error {
	path := filepath.Join(localserver.StateDir(), "bridge.yaml")
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
	if _, exists := t["collector_url"]; !exists {
		t["collector_url"] = e.TelemetryEndpoint
		t["collector_credential_file"] = filepath.Join(localserver.StateDir(), "bridge-credentials.json")
		t["enabled"] = true
		m["telemetry"] = t
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}

func newBridgeWhoamiCmd() *cobra.Command {
	return &cobra.Command{Use: "whoami", Short: "Show Bridge enrollment status", RunE: func(cmd *cobra.Command, _ []string) error {
		e, _, err := readEnrollment()
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(cmd.OutOrStdout(), "Not logged into Bridge.")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Bridge          %s\nOrganization    %s\nInstallation    %s\nStatus          logged in\n", e.BridgeURL, display(e.OrganizationName, e.OrganizationID), display(e.InstallationName, e.InstallationID))
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
		if _, err := os.Stat(mp); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(cmd.OutOrStdout(), "Not logged into Bridge.")
			return nil
		}
		if err := os.Remove(mp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Bridge enrollment removed locally.")
		fmt.Fprintln(cmd.OutOrStdout(), "Remote revocation is not exposed by this Bridge protocol.")
		return nil
	}}
}
