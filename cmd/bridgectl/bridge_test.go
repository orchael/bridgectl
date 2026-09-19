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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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

// TestBridgeURLFallsBackToSavedOriginWithoutCredential guards the documented
// URL precedence (flag > env > saved enrollment URL > default): a missing or
// unreadable credential file must not hide the saved origin, since only the
// enrollment metadata is needed to know which Bridge was previously used.
func TestBridgeURLFallsBackToSavedOriginWithoutCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mp, _ := bridgeStatePaths()
	if err := atomicJSON(mp, bridgeEnrollment{BridgeURL: "https://saved.example", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://saved.example/v1/telemetry/segments"}); err != nil {
		t.Fatal(err)
	}
	// No bridge-credentials.json was ever written: readEnrollment() (which
	// requires both files) would fail here, but bridgeURL must still resolve
	// the saved origin from metadata alone.
	got, err := bridgeURL("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://saved.example" {
		t.Fatalf("expected saved origin despite missing credential, got %q", got)
	}
}

func TestPersistBridgeEnrollmentDoesNotOverwriteExplicitTelemetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "bridge.yaml"), []byte("telemetry:\n  collector_url: https://explicit.example/v1/telemetry/segments\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err != nil {
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
	if strings.Contains(string(b), "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v") {
		t.Fatal("credential leaked into config")
	}
}

func TestPersistBridgeEnrollmentStoresOrganizationNameAndURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := deviceToken{
		BridgeURL:           "https://bridge.example",
		APIVersion:          "v1",
		OrganizationID:      "org-123",
		OrganizationName:    "Acme Inc",
		OrganizationURL:     "https://bridge.example/organizations/org-123",
		InstallationID:      "install",
		TelemetryEndpoint:   "https://bridge.example/v1/telemetry/segments",
		CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v",
	}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err != nil {
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v2", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err == nil {
		t.Fatal("expected unsupported api_version to be rejected")
	}
	if _, err := readEnrollmentMetadata(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no enrollment to be persisted, got err=%v", err)
	}
	tok.APIVersion = ""
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err == nil {
		t.Fatal("expected missing api_version to be rejected")
	}
}

// TestPersistBridgeEnrollmentRejectsMalformedCredential guards against
// persisting and sending an arbitrary bearer token to the collector: the
// protocol documents brc_<opaque secret> as the only valid credential shape.
func TestPersistBridgeEnrollmentRejectsMalformedCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, bad := range []string{"not-a-credential", "brc_tooshort", "brc_" + strings.Repeat("a", 44), ""} {
		tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: bad}
		if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err == nil {
			t.Fatalf("expected credential %q to be rejected", bad)
		}
	}
}

// TestPersistBridgeEnrollmentRestoresPreviousEnrollmentOnForceFailure guards
// against a --force re-enrollment silently logging the user out: if a step
// after overwriting mp/sp fails, the enrollment that existed before this
// login attempt must be restored, not deleted.
func TestPersistBridgeEnrollmentRestoresPreviousEnrollmentOnForceFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	original := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "old-org", InstallationID: "old-install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := persistBridgeEnrollment(original, "laptop", "https://bridge.example"); err != nil {
		t.Fatal(err)
	}

	// Make configureBridgeTelemetry fail by replacing bridge.yaml (already
	// created by the first login) with a directory, simulating a failure
	// after mp/sp are overwritten.
	yamlPath := filepath.Join(dir, "bridge.yaml")
	if err := os.Remove(yamlPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(yamlPath, 0700); err != nil {
		t.Fatal(err)
	}

	replacement := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "new-org", InstallationID: "new-install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_Z9y8X7w6V5u4T3s2R1q0P9o8N7m6L5k4J3i2H1g0F9e"}
	if err := persistBridgeEnrollment(replacement, "laptop", "https://bridge.example"); err == nil {
		t.Fatal("expected the forced re-enrollment to fail")
	}

	e, err := readEnrollmentMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if e.OrganizationID != "old-org" {
		t.Fatalf("expected the previous enrollment to be restored, got organization_id=%q", e.OrganizationID)
	}
	s, err := readBridgeSecret()
	if err != nil {
		t.Fatal(err)
	}
	if s.CollectorCredential != original.CollectorCredential {
		t.Fatal("expected the previous credential to be restored")
	}
}

func TestPersistBridgeEnrollmentRejectsInvalidOrganizationURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := deviceToken{
		BridgeURL:           "https://bridge.example",
		APIVersion:          "v1",
		OrganizationID:      "org-123",
		OrganizationURL:     "not-a-url",
		InstallationID:      "install",
		TelemetryEndpoint:   "https://bridge.example/v1/telemetry/segments",
		CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v",
	}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err == nil {
		t.Fatal("expected invalid organization URL to be rejected")
	}
}

// TestPersistBridgeEnrollmentRejectsMismatchedBridgeURL guards against
// treating bridge_url as optional, fallback-resolvable data: an empty value
// or one that differs from the origin this login actually talked to must be
// rejected outright, not silently replaced via the CLI/env/saved/default
// precedence chain (which exists to pick where to send the *request*, not
// to validate the *response*).
func TestPersistBridgeEnrollmentRejectsMismatchedBridgeURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := deviceToken{APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}

	mismatched := base
	mismatched.BridgeURL = "https://attacker.example"
	if err := persistBridgeEnrollment(mismatched, "laptop", "https://bridge.example"); err == nil {
		t.Fatal("expected a bridge_url that differs from the requested origin to be rejected")
	}

	empty := base
	empty.BridgeURL = ""
	if err := persistBridgeEnrollment(empty, "laptop", "https://bridge.example"); err == nil {
		t.Fatal("expected an empty bridge_url to be rejected instead of falling back to the requested origin")
	}

	if _, err := readEnrollmentMetadata(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no enrollment to be persisted, got err=%v", err)
	}
}

// TestPersistBridgeEnrollmentRollsBackMetadataWhenCredentialWriteFails guards
// against leaving bridge-enrollment.json behind when bridge-credentials.json
// cannot be written: the next login would otherwise see a partially readable
// enrollment and refuse to re-enroll without --force.
func TestPersistBridgeEnrollmentRollsBackMetadataWhenCredentialWriteFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mp, sp := bridgeStatePaths()
	if err := os.MkdirAll(sp, 0700); err != nil { // occupy the credential path with a directory so the write fails
		t.Fatal(err)
	}
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err == nil {
		t.Fatal("expected credential write failure to be reported")
	}
	if _, err := os.Stat(mp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected enrollment metadata to be rolled back, stat err=%v", err)
	}
}

func TestValidateBridgeOriginURL(t *testing.T) {
	const origin = "https://bridge.example"
	cases := []struct {
		name       string
		raw        string
		allowQuery bool
		wantErr    bool
	}{
		{"matching origin no query", "https://bridge.example/device", false, false},
		{"matching origin with query when allowed", "https://bridge.example/device?user_code=ABCD-EFGH-JKLM", true, false},
		{"query rejected when not allowed", "https://bridge.example/device?user_code=ABCD-EFGH-JKLM", false, true},
		{"different host is phishing risk", "https://attacker.example/device", false, true},
		{"http downgrade rejected", "http://bridge.example/device", false, true},
		{"credentials rejected", "https://user:pass@bridge.example/device", false, true},
		{"fragment rejected", "https://bridge.example/device#x", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateBridgeOriginURL(c.raw, origin, c.allowQuery)
			if c.wantErr && err == nil {
				t.Fatalf("expected error for %q", c.raw)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", c.raw, err)
			}
		})
	}
}

func TestWhoamiShowsOrganizationNameAndURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := deviceToken{
		BridgeURL:           "https://bridge.example",
		APIVersion:          "v1",
		OrganizationID:      "org-123",
		OrganizationName:    "Acme Inc",
		OrganizationURL:     "https://bridge.example/organizations/org-123",
		InstallationID:      "install",
		TelemetryEndpoint:   "https://bridge.example/v1/telemetry/segments",
		CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v",
	}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err != nil {
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

// TestReadBridgeSecretRejectsMalformedCredential guards against a corrupted
// or hand-edited credential file reading as a healthy enrollment: whoami,
// doctor, and readEnrollment must all report the same "missing" status that
// server start's stricter localserver.CollectorCredentialPattern check
// would, or a corrupted file looks fine here while telemetry silently fails
// to start, with no recovery path short of --force.
func TestReadBridgeSecretRejectsMalformedCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	_, sp := bridgeStatePaths()
	if err := atomicJSON(sp, bridgeSecret{CollectorCredential: "not-a-valid-credential"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readBridgeSecret(); err == nil {
		t.Fatal("expected a malformed collector_credential to be rejected")
	}
}

// TestValidateDeviceAuthorizationRejectsIncompleteResponse guards against
// printing a blank code and polling forever: an authorize response missing
// device_code or user_code must be rejected outright.
func TestValidateDeviceAuthorizationRejectsIncompleteResponse(t *testing.T) {
	valid := deviceAuthorization{DeviceCode: "secret", UserCode: "ABCD-EFGH-JKLM", ExpiresIn: 600, Interval: 5}
	if err := validateDeviceAuthorization(valid); err != nil {
		t.Fatalf("expected a complete authorization response to be accepted, got %v", err)
	}
	for name, mutate := range map[string]func(*deviceAuthorization){
		"empty device_code": func(a *deviceAuthorization) { a.DeviceCode = "" },
		"empty user_code":   func(a *deviceAuthorization) { a.UserCode = "" },
	} {
		t.Run(name, func(t *testing.T) {
			auth := valid
			mutate(&auth)
			if err := validateDeviceAuthorization(auth); err == nil {
				t.Fatal("expected the incomplete response to be rejected")
			}
		})
	}
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

// TestLogoutRemovesCredentialWhenMetadataAlreadyMissing guards against
// logout reporting "Not logged into Bridge" (and leaving secret material on
// disk) when bridge-enrollment.json is gone but bridge-credentials.json
// survives a partial write or manual cleanup.
func TestLogoutRemovesCredentialWhenMetadataAlreadyMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, sp := bridgeStatePaths()
	if err := atomicJSON(sp, bridgeSecret{CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}); err != nil {
		t.Fatal(err)
	}
	cmd := newBridgeLogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Not logged into Bridge") {
		t.Fatalf("logout incorrectly reported not logged in: %s", out.String())
	}
	if _, err := os.Stat(sp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("credential file was not removed")
	}
}

func TestLogoutRemovesManagedTelemetryAndState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err != nil {
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
	// bridge.yaml was created only to hold managed enrollment settings, so
	// once those are cleared it must be removed entirely rather than left
	// behind empty, which would otherwise permanently outrank (and shadow)
	// an XDG config in server start's resolution order.
	if _, err := os.Stat(filepath.Join(dir, "bridge.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected the managed-only bridge.yaml to be removed, stat err=%v", err)
	}
}

// TestLogoutPreservesStandaloneConfigAlongsideManagedTelemetry guards
// against logout deleting a bridge.yaml that also holds the user's own
// standalone settings: only the managed telemetry keys should be removed,
// and the file itself must survive since it isn't empty afterward.
func TestLogoutPreservesStandaloneConfigAlongsideManagedTelemetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	yamlPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(yamlPath, []byte("providers:\n  claude:\n    binary: claude\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err != nil {
		t.Fatal(err)
	}
	cmd := newBridgeLogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("expected bridge.yaml with standalone settings to survive logout: %v", err)
	}
	if !strings.Contains(string(b), "claude") {
		t.Fatalf("standalone settings lost: %s", b)
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err != nil {
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
		_, _ = w.Write([]byte(`{"bridge_url":"https://bridge.example","api_version":"v1","organization_id":"org","installation_id":"install","telemetry_endpoint":"https://bridge.example/v1/telemetry/segments","collector_credential":"brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}`))
	}))
	defer target.Close()
	client := &http.Client{}
	tok, derr, _, err := pollDeviceToken(t.Context(), client, target.URL, "device-code")
	if err != nil || derr != nil {
		t.Fatalf("tok=%v derr=%v err=%v", tok, derr, err)
	}
	if tok == nil || tok.APIVersion != "v1" || tok.CollectorCredential != "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v" {
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
	if got := nextSlowDownInterval(5*time.Second, time.Duration(derr.Interval)*time.Second, retryAfter); got != 45*time.Second {
		t.Fatalf("expected the greatest of current/granted/Retry-After (45s), got %v", got)
	}
}

// TestNextSlowDownIntervalHasNoUpperClamp guards the documented contract of
// never reducing below what the server granted: a self-imposed ceiling
// (the old exponential-backoff design used one) would violate that when
// Bridge legitimately asks for a wait longer than 60s under abuse
// mitigation.
func TestNextSlowDownIntervalHasNoUpperClamp(t *testing.T) {
	if got := nextSlowDownInterval(5*time.Second, 300*time.Second, 0); got != 300*time.Second {
		t.Fatalf("expected the server-granted interval (300s) to be honored uncapped, got %v", got)
	}
	if got := nextSlowDownInterval(5*time.Second, 0, 400*time.Second); got != 400*time.Second {
		t.Fatalf("expected Retry-After (400s) to be honored uncapped, got %v", got)
	}
	if got := nextSlowDownInterval(90*time.Second, 10*time.Second, 20*time.Second); got != 90*time.Second {
		t.Fatalf("expected the current interval to never be reduced, got %v", got)
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
