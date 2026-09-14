package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateIdentityKeyPersistsPrivateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "identity-key")
	first, err := LoadOrCreateIdentityKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateIdentityKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || string(first) != string(second) {
		t.Fatalf("identity keys lengths=%d/%d equal=%v", len(first), len(second), string(first) == string(second))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity key mode=%#o, want 0600", info.Mode().Perm())
	}
}

func TestDiscoverSessionContextHashesPrivateLocations(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "customer-secret-repo")
	workdir := filepath.Join(repo, "private", "feature")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "refs", "heads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	remote := "https://oauth-secret@github.com/private-org/customer-secret-repo.git"
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+remote+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "refs", "heads", "main"), []byte("0123456789abcdef0123456789abcdef01234567\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	context := DiscoverSessionContext(workdir, "actor-7", "laptop", []byte("01234567890123456789012345678901"))
	if context.ActorID != "actor-7" || context.SourceLabel != "laptop" || context.OS == "" || context.Arch == "" || context.MachineID == "" {
		t.Fatalf("context identity/platform=%+v", context)
	}
	if context.WorkingDirectoryID == "" || context.RepositoryID == "" || context.RemoteHost != "github.com" || context.Branch != "main" || context.CommitSHA == "" {
		t.Fatalf("context repository=%+v", context)
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{repo, workdir, remote, "oauth-secret", "private-org", "customer-secret-repo"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("context leaked %q: %s", secret, encoded)
		}
	}
}

func TestAnalyzerEmitsSchemaV2SessionContext(t *testing.T) {
	sink := &memorySink{}
	analyzer := NewAnalyzer(sink, nil)
	session := Session{SourceID: "source-1", SessionID: "session-1", ActorID: "actor-1"}
	context := SessionContext{ActorID: "actor-1", OS: "linux", Arch: "amd64", WorkingDirectoryID: "directory-id"}
	analyzer.ObserveSessionStart(session)
	analyzer.ObserveSessionContext(session, context)
	events := sink.snapshot()
	if len(events) != 2 || events[0].SchemaVersion != 2 || events[1].SchemaVersion != 2 || events[1].Kind != EventSessionContext || events[1].Context == nil {
		t.Fatalf("events=%+v", events)
	}
}
