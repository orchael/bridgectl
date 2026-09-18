package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	tok := deviceToken{BridgeURL: "https://bridge.example", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_secret"}
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

func TestPersistBridgeEnrollmentRejectsInvalidOrganizationURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	tok := deviceToken{
		BridgeURL:           "https://bridge.example",
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
