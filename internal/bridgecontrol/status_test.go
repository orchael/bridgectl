package bridgecontrol

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadStatus_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	want := Status{State: StateConnected, InstallationID: "install-1", UpdatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := WriteStatus(path, want); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	got, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.State != want.State || got.InstallationID != want.InstallationID || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("ReadStatus = %+v, want %+v", got, want)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("status file mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestReadStatus_MissingFile(t *testing.T) {
	if _, err := ReadStatus(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected an error reading a missing status file")
	}
}
