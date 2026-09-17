package main

import (
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

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
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
