package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBridgeReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()
	if !bridgeReachable(server.URL) {
		t.Fatal("expected reachable server to report reachable")
	}
	if bridgeReachable("https://127.0.0.1:1") {
		t.Fatal("expected unroutable address to report unreachable")
	}
}

// TestDoctorReportsNetworkStatus guards the doctor checklist requirement to
// distinguish network availability from enrollment/credential status, not
// just report local file state.
func TestDoctorReportsNetworkStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	tok := deviceToken{BridgeURL: server.URL, APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := atomicJSON(func() string { mp, _ := bridgeStatePaths(); return mp }(), bridgeEnrollment{BridgeURL: tok.BridgeURL, OrganizationID: tok.OrganizationID, InstallationID: tok.InstallationID, TelemetryEndpoint: tok.TelemetryEndpoint}); err != nil {
		t.Fatal(err)
	}
	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "network       ✓ reachable") {
		t.Fatalf("expected reachable network status: %s", out.String())
	}
}
