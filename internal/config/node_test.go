package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNodeMajorVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{name: "plain version", input: "v24.3.0\n", want: 24},
		{name: "whitespace", input: "  v22.19.1  ", want: 22},
		{name: "invalid", input: "node", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNodeMajorVersion(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNodeMajorVersion: %v", err)
			}
			if got != tc.want {
				t.Fatalf("major=%d want %d", got, tc.want)
			}
		})
	}
}

func TestParseRequiredNodeMajor(t *testing.T) {
	got, err := parseRequiredNodeMajor("24\n")
	if err != nil {
		t.Fatalf("parseRequiredNodeMajor: %v", err)
	}
	if got != 24 {
		t.Fatalf("major=%d want 24", got)
	}

	if _, err := parseRequiredNodeMajor("lts/*"); err == nil {
		t.Fatal("expected error for non-numeric nvmrc")
	}
}

func TestNodeVersionMatchesRequirement(t *testing.T) {
	tests := []struct {
		name     string
		required int
		actual   int
		want     bool
	}{
		{name: "match", required: 24, actual: 24, want: true},
		{name: "newer major", required: 24, actual: 25, want: true},
		{name: "older", required: 24, actual: 22, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeVersionMatchesRequirement(tc.required, tc.actual); got != tc.want {
				t.Fatalf("nodeVersionMatchesRequirement(%d, %d)=%v want %v", tc.required, tc.actual, got, tc.want)
			}
		})
	}
}

func TestValidateNodeRuntime(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".nvmrc"), []byte("24\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	oldLookPathNode := lookPathNode
	oldRunCommand := runCommand
	t.Cleanup(func() {
		lookPathNode = oldLookPathNode
		runCommand = oldRunCommand
	})

	lookPathNode = func(file string) (string, error) {
		if file != "node" {
			t.Fatalf("lookPathNode(%q)", file)
		}
		return "/usr/bin/node", nil
	}
	runCommand = func(name string, args ...string) ([]byte, error) {
		if name != "/usr/bin/node" {
			t.Fatalf("runCommand name=%q", name)
		}
		if len(args) != 1 || args[0] != "--version" {
			t.Fatalf("runCommand args=%v", args)
		}
		return []byte("v24.8.0\n"), nil
	}

	if err := ValidateNodeRuntime(dir); err != nil {
		t.Fatalf("ValidateNodeRuntime: %v", err)
	}
}

func TestValidateNodeRuntimeWithExplicitRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".nvmrc"), []byte("24\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	oldLookPathNode := lookPathNode
	oldRunCommand := runCommand
	t.Cleanup(func() {
		lookPathNode = oldLookPathNode
		runCommand = oldRunCommand
	})
	lookPathNode = func(file string) (string, error) { return "/usr/bin/node", nil }
	runCommand = func(name string, args ...string) ([]byte, error) {
		return []byte("v24.1.0\n"), nil
	}

	// Validates successfully when the explicit root contains .nvmrc.
	if err := ValidateNodeRuntime(root); err != nil {
		t.Fatalf("ValidateNodeRuntime with root: %v", err)
	}

	// Fails when .nvmrc is absent from the given root.
	emptyRoot := t.TempDir()
	if err := ValidateNodeRuntime(emptyRoot); err == nil {
		t.Fatal("expected error for missing .nvmrc")
	}
}
