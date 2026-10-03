package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// TestCommandNameReservedBuiltins covers the Copilot review finding on PR
// #279: a custom provider named "doctor", "help", or "completion" must not
// be allowed to register a shortcut that collides with those commands.
func TestCommandNameReservedBuiltins(t *testing.T) {
	root := &cobra.Command{Use: "bridgectl"}
	root.AddCommand(newDoctorCmd())

	for _, name := range []string{"doctor", "help", "completion"} {
		if !commandNameReserved(root, name) {
			t.Errorf("commandNameReserved(%q) = false, want true", name)
		}
	}
	if commandNameReserved(root, "a-custom-provider-name") {
		t.Error("commandNameReserved(unregistered name) = true, want false")
	}
}

// TestCommandNameReservedAlias covers an aliased command name too.
func TestCommandNameReservedAlias(t *testing.T) {
	root := &cobra.Command{Use: "bridgectl"}
	root.AddCommand(&cobra.Command{Use: "whoami", Aliases: []string{"who"}})

	if !commandNameReserved(root, "who") {
		t.Error("commandNameReserved(alias) = false, want true")
	}
}

// TestProviderShortcutIDsSkipsReservedConflicts documents the end-to-end
// behavior: a custom provider in bridge.yaml named after a real subcommand
// should not get a shortcut registered on root.
func TestProviderShortcutIDsSkipsReservedConflicts(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", stateDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configPath := filepath.Join(stateDir, "bridge.yaml")
	yaml := "providers:\n  doctor:\n    binary: cat\n  mycustomprovider:\n    binary: cat\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	ids := providerShortcutIDs()
	root := &cobra.Command{Use: "bridgectl"}
	root.AddCommand(newDoctorCmd())

	var registered []string
	for _, id := range ids {
		if !commandNameReserved(root, id) {
			registered = append(registered, id)
		}
	}

	for _, want := range registered {
		if want == "doctor" {
			t.Fatal("expected the custom \"doctor\" provider to be filtered out, but it was registered")
		}
	}
	found := false
	for _, id := range registered {
		if id == "mycustomprovider" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected \"mycustomprovider\" to be registered, got %v", registered)
	}
}
