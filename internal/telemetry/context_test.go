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
	if context.WorkingDirectoryID == "" || context.RepositoryID == "" || context.Branch != "main" || context.CommitSHA == "" {
		t.Fatalf("context repository=%+v", context)
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{repo, workdir, remote, "oauth-secret", "github.com", "private-org", "customer-secret-repo"} {
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

// TEL-117: linked worktrees share repository identity, not directory or HEAD.
func TestDiscoverSessionContextLinkedWorktree(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "origin"}[remote], func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "private-repo")
			common := filepath.Join(repo, ".git")
			worktree := filepath.Join(root, "private-worktree")
			gitDir := filepath.Join(common, "worktrees", "feature")
			write := func(path, data string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(worktree, ".git"), "gitdir: ../private-repo/.git/worktrees/feature\n")
			write(filepath.Join(gitDir, "commondir"), "../..\n")
			write(filepath.Join(common, "HEAD"), "ref: refs/heads/main\n")
			write(filepath.Join(gitDir, "HEAD"), "ref: refs/heads/feature\n")
			write(filepath.Join(common, "refs", "heads", "main"), "main-commit\n")
			write(filepath.Join(common, "refs", "heads", "feature"), "feature-commit\n")
			if remote {
				write(filepath.Join(common, "config"), "[remote \"origin\"]\nurl = https://credential@private.example/private/repo.git\n")
			}
			key := []byte("01234567890123456789012345678901")
			main := DiscoverSessionContext(repo, "", "", key)
			linked := DiscoverSessionContext(worktree, "", "", key)
			if main.RepositoryID == "" || main.RepositoryID != linked.RepositoryID {
				t.Fatalf("repository identities differ: main=%+v linked=%+v", main, linked)
			}
			if main.WorkingDirectoryID == linked.WorkingDirectoryID || linked.Branch != "feature" || linked.CommitSHA != "feature-commit" || main.Branch != "main" || main.CommitSHA != "main-commit" {
				t.Fatalf("worktree metadata: main=%+v linked=%+v", main, linked)
			}
			encoded, err := json.Marshal(linked)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{root, "private-repo", "private-worktree", "credential", "private.example"} {
				if strings.Contains(string(encoded), private) {
					t.Fatalf("leaked %q: %s", private, encoded)
				}
			}
		})
	}
}

func TestGitCommonDirectoryBestEffort(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata string
		missing  bool
		valid    bool
	}{
		{name: "missing", missing: true},
		{name: "empty"},
		{name: "missing-directory", metadata: "../absent"},
		{name: "regular-file", metadata: "HEAD"},
		{name: "multiline", metadata: "../common\n../other"},
		{name: "nul", metadata: "../common\x00"},
		{name: "relative", metadata: "../common\n", valid: true},
		{name: "absolute", valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			gitDir, common := filepath.Join(root, "git"), filepath.Join(root, "common")
			for _, dir := range []string{gitDir, common} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("detached-commit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.name == "absolute" {
				test.metadata = common
			}
			if !test.missing {
				if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(test.metadata), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want := gitDir
			if test.valid {
				want = common
			}
			if got := gitCommonDirectory(gitDir); got != want {
				t.Fatalf("common directory=%q want %q", got, want)
			}
			branch, commit := readGitHead(gitDir, want)
			if branch != "" || commit != "detached-commit" {
				t.Fatalf("HEAD=%q/%q", branch, commit)
			}
		})
	}
}
