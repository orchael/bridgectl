package localserver

import (
	"path/filepath"
	"testing"
)

func TestTelemetrySpoolDir(t *testing.T) {
	stateDir := t.TempDir()
	if got, want := TelemetrySpoolDir("", stateDir), filepath.Join(stateDir, "telemetry", "segments"); got != want {
		t.Fatalf("TelemetrySpoolDir default=%q, want %q", got, want)
	}
	t.Setenv("BRIDGE_TELEMETRY_TEST_DIR", stateDir)
	if got, want := TelemetrySpoolDir("$BRIDGE_TELEMETRY_TEST_DIR/outbox", stateDir), filepath.Join(stateDir, "outbox"); got != want {
		t.Fatalf("TelemetrySpoolDir expanded=%q, want %q", got, want)
	}
}
