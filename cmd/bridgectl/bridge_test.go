package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBridgeURLPrecedenceAndHTTPS(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	t.Setenv("BRIDGECTL_BRIDGE_URL", "https://env.example")
	if got, _ := bridgeURL("https://flag.example"); got != "https://flag.example" {
		t.Fatalf("flag precedence: %q", got)
	}
	if got, _ := bridgeURL(""); got != "https://env.example" {
		t.Fatalf("env precedence: %q", got)
	}
	if _, err := bridgeURL("http://insecure.example"); err == nil {
		t.Fatal("accepted HTTP Bridge URL")
	}
	if _, err := bridgeURL("https://example/path"); err == nil {
		t.Fatal("accepted path Bridge URL")
	}
}

func TestPersistBridgeEnrollmentDoesNotOverwriteExplicitTelemetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "bridge.yaml"), []byte("telemetry:\n  collector_url: https://explicit.example/v1/telemetry/segments\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_secret"}
	if err := persistBridgeEnrollment(tok, "laptop"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "https://explicit.example/v1/telemetry/segments") || strings.Contains(string(b), "bridge.example/v1/telemetry") {
		t.Fatalf("explicit telemetry overwritten: %s", b)
	}
	if mode := mustMode(t, filepath.Join(dir, "bridge-credentials.json")); mode != 0600 {
		t.Fatalf("credential mode %o", mode)
	}
	if strings.Contains(string(b), "brc_secret") {
		t.Fatal("credential leaked into config")
	}
}

func TestPersistBridgeEnrollmentStoresOrganizationNameAndURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	tok := deviceToken{
		BridgeURL:           "https://bridge.example",
		APIVersion:          "v1",
		OrganizationID:      "org-123",
		OrganizationName:    "Acme Inc",
		OrganizationURL:     "https://bridge.example/organizations/org-123",
		InstallationID:      "install",
		TelemetryEndpoint:   "https://bridge.example/v1/telemetry/segments",
		CollectorCredential: "brc_secret",
	}
	if err := persistBridgeEnrollment(tok, "laptop"); err != nil {
		t.Fatal(err)
	}
	e, err := readEnrollmentMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if e.OrganizationName != "Acme Inc" {
		t.Fatalf("organization name: %q", e.OrganizationName)
	}
	if e.OrganizationURL != "https://bridge.example/organizations/org-123" {
		t.Fatalf("organization url: %q", e.OrganizationURL)
	}
}

// TestPersistBridgeEnrollmentRejectsUnsupportedAPIVersion guards the
// documented device-enrollment protocol version contract
// (docs/bridgectl-device-enrollment.md step 5): an api_version bridgectl does
// not recognize must fail enrollment explicitly rather than silently
// persisting a credential issued under an incompatible protocol.
func TestPersistBridgeEnrollmentRejectsUnsupportedAPIVersion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v2", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_secret"}
	if err := persistBridgeEnrollment(tok, "laptop"); err == nil {
		t.Fatal("expected unsupported api_version to be rejected")
	}
	if _, err := readEnrollmentMetadata(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no enrollment to be persisted, got err=%v", err)
	}
	tok.APIVersion = ""
	if err := persistBridgeEnrollment(tok, "laptop"); err == nil {
		t.Fatal("expected missing api_version to be rejected")
	}
}

func TestPersistBridgeEnrollmentRejectsInvalidOrganizationURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	tok := deviceToken{
		BridgeURL:           "https://bridge.example",
		APIVersion:          "v1",
		OrganizationID:      "org-123",
		OrganizationURL:     "not-a-url",
		InstallationID:      "install",
		TelemetryEndpoint:   "https://bridge.example/v1/telemetry/segments",
		CollectorCredential: "brc_secret",
	}
	if err := persistBridgeEnrollment(tok, "laptop"); err == nil {
		t.Fatal("expected invalid organization URL to be rejected")
	}
}

func TestWhoamiShowsOrganizationNameAndURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	tok := deviceToken{
		BridgeURL:           "https://bridge.example",
		APIVersion:          "v1",
		OrganizationID:      "org-123",
		OrganizationName:    "Acme Inc",
		OrganizationURL:     "https://bridge.example/organizations/org-123",
		InstallationID:      "install",
		TelemetryEndpoint:   "https://bridge.example/v1/telemetry/segments",
		CollectorCredential: "brc_secret",
	}
	if err := persistBridgeEnrollment(tok, "laptop"); err != nil {
		t.Fatal(err)
	}
	cmd := newBridgeWhoamiCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Organization    Acme Inc") {
		t.Fatalf("missing organization name: %s", out.String())
	}
	if !strings.Contains(out.String(), "Org URL         https://bridge.example/organizations/org-123") {
		t.Fatalf("missing organization url: %s", out.String())
	}
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestDefaultOrganizationPrecedence(t *testing.T) {
	t.Setenv("BRIDGECTL_ORGANIZATION", "Env Org")
	if got := defaultOrganization("Flag Org"); got != "Flag Org" {
		t.Fatalf("flag precedence: %q", got)
	}
	if got := defaultOrganization(""); got != "Env Org" {
		t.Fatalf("env precedence: %q", got)
	}
	t.Setenv("BRIDGECTL_ORGANIZATION", "")
	if got := defaultOrganization(""); got != "" {
		t.Fatalf("expected empty default, got %q", got)
	}
}

func TestAuthorizeRequestBodyIncludesRequestedOrganization(t *testing.T) {
	t.Setenv("BRIDGECTL_ORGANIZATION", "")
	body := authorizeRequestBody("")
	if _, ok := body["requested_organization"]; ok {
		t.Fatalf("expected requested_organization omitted when empty, got %v", body)
	}
	body = authorizeRequestBody("Acme Inc")
	if body["requested_organization"] != "Acme Inc" {
		t.Fatalf("expected requested_organization=Acme Inc, got %v", body)
	}
	if body["display_name"] == "" {
		t.Fatalf("expected display_name to still be set, got %v", body)
	}
}

func TestLogoutRemovesManagedTelemetryAndState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_secret"}
	if err := persistBridgeEnrollment(tok, "laptop"); err != nil {
		t.Fatal(err)
	}
	cmd := newBridgeLogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	mp, sp := bridgeStatePaths()
	if _, err := os.Stat(mp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("enrollment metadata still present after logout")
	}
	if _, err := os.Stat(sp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("credential still present after logout")
	}
	b, err := os.ReadFile(filepath.Join(dir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "managed_by_bridge") || strings.Contains(string(b), "collector_url") {
		t.Fatalf("managed telemetry not cleared: %s", b)
	}
}

// TestLogoutPreservesStateWhenTelemetryCleanupFails guards against leaving
// bridge.yaml pointing at a deleted credential file: if the managed
// telemetry config cannot be rewritten, logout must fail loudly and keep
// the enrollment files rather than silently discarding the error while
// deleting state, which previously left a stale collector_url/credential
// reference that broke the next server start.
func TestLogoutPreservesStateWhenTelemetryCleanupFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_secret"}
	if err := persistBridgeEnrollment(tok, "laptop"); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(dir, "bridge.yaml")
	if err := os.Remove(yamlPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(yamlPath, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := newBridgeLogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("expected logout to fail when telemetry config cannot be rewritten")
	}
	mp, sp := bridgeStatePaths()
	if _, err := os.Stat(mp); err != nil {
		t.Fatalf("enrollment metadata removed despite failed cleanup: %v", err)
	}
	if _, err := os.Stat(sp); err != nil {
		t.Fatalf("credential removed despite failed cleanup: %v", err)
	}
}

func TestHTTPJSONRejectsRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example", http.StatusFound)
	}))
	defer target.Close()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	status, err := httpJSON(t.Context(), client, http.MethodPost, target.URL, map[string]string{"device_code": "x"}, &map[string]any{})
	if err == nil || status != http.StatusFound {
		t.Fatalf("redirect status=%d err=%v", status, err)
	}
}

func TestPollDeviceTokenParsesSuccess(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"bridge_url":"https://bridge.example","api_version":"v1","organization_id":"org","installation_id":"install","telemetry_endpoint":"https://bridge.example/v1/telemetry/segments","collector_credential":"brc_secret"}`))
	}))
	defer target.Close()
	client := &http.Client{}
	tok, derr, _, err := pollDeviceToken(t.Context(), client, target.URL, "device-code")
	if err != nil || derr != nil {
		t.Fatalf("tok=%v derr=%v err=%v", tok, derr, err)
	}
	if tok == nil || tok.APIVersion != "v1" || tok.CollectorCredential != "brc_secret" {
		t.Fatalf("unexpected token: %+v", tok)
	}
}

func TestPollDeviceTokenAuthorizationPending(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"authorization_pending","interval":5}`))
	}))
	defer target.Close()
	tok, derr, _, err := pollDeviceToken(t.Context(), &http.Client{}, target.URL, "device-code")
	if err != nil || tok != nil {
		t.Fatalf("tok=%v err=%v", tok, err)
	}
	if derr == nil || derr.Error != "authorization_pending" {
		t.Fatalf("unexpected derr: %+v", derr)
	}
}

// TestPollDeviceTokenSlowDownHonorsGreatestInterval guards the documented
// slow_down contract: the client must keep the greatest of its current
// interval, the server's returned `interval`, and Retry-After, never
// reducing it, per docs/bridgectl-device-enrollment.md.
func TestPollDeviceTokenSlowDownHonorsGreatestInterval(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"slow_down","interval":10}`))
	}))
	defer target.Close()
	tok, derr, retryAfter, err := pollDeviceToken(t.Context(), &http.Client{}, target.URL, "device-code")
	if err != nil || tok != nil {
		t.Fatalf("tok=%v err=%v", tok, err)
	}
	if derr == nil || derr.Error != "slow_down" || derr.Interval != 10 {
		t.Fatalf("unexpected derr: %+v", derr)
	}
	if retryAfter != 45*time.Second {
		t.Fatalf("expected Retry-After=45s, got %v", retryAfter)
	}
	interval := 5 * time.Second
	if granted := time.Duration(derr.Interval) * time.Second; granted > interval {
		interval = granted
	}
	if retryAfter > interval {
		interval = retryAfter
	}
	if interval != 45*time.Second {
		t.Fatalf("expected the greatest of current/granted/Retry-After (45s), got %v", interval)
	}
}

func TestPollDeviceTokenTerminalErrorIsReturned(t *testing.T) {
	for _, code := range []string{"access_denied", "expired_token", "invalid_grant"} {
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"` + code + `","interval":5}`))
		}))
		tok, derr, _, err := pollDeviceToken(t.Context(), &http.Client{}, target.URL, "device-code")
		target.Close()
		if err != nil || tok != nil {
			t.Fatalf("%s: tok=%v err=%v", code, tok, err)
		}
		if derr == nil || derr.Error != code {
			t.Fatalf("%s: unexpected derr: %+v", code, derr)
		}
	}
}
