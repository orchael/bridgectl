package localserver

import (
	"os"
	"path/filepath"
	"strings"
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

// TestSecureReadFileRejectsPermissiveOrSymlinkedCredential guards the
// server-start read path for bridge-credentials.json: a permissive or
// replaced credential file must not be silently trusted just because it
// exists at the expected path.
func TestSecureReadFileRejectsPermissiveOrSymlinkedCredential(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"collector_credential":"brc_x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := SecureReadFile(good); err != nil || string(b) != `{"collector_credential":"brc_x"}` {
		t.Fatalf("expected a 0600 regular file to read cleanly, got %q err=%v", b, err)
	}

	permissive := filepath.Join(dir, "permissive.json")
	if err := os.WriteFile(permissive, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := SecureReadFile(permissive); err == nil {
		t.Fatal("expected a group/world-readable file to be rejected")
	}

	symlink := filepath.Join(dir, "link.json")
	if err := os.Symlink(good, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := SecureReadFile(symlink); err == nil {
		t.Fatal("expected a symlink to be rejected")
	}

	if _, err := SecureReadFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected a missing file to error")
	}
}

func TestCollectorCredentialPatternMatchesBridgeFormat(t *testing.T) {
	if !CollectorCredentialPattern.MatchString("brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v") {
		t.Fatal("expected a valid brc_ credential to match")
	}
	for _, bad := range []string{"", "not-a-credential", "brc_tooshort", "brc_" + strings.Repeat("a", 44)} {
		if CollectorCredentialPattern.MatchString(bad) {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}
