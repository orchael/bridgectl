package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveLogSettings(t *testing.T) {
	writeConfig := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "bridge.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	t.Run("no config path leaves flags untouched", func(t *testing.T) {
		level, format := resolveLogSettings("", "", "text", false, false)
		if level != "" || format != "text" {
			t.Fatalf("got (%q, %q), want (\"\", \"text\")", level, format)
		}
	})

	t.Run("config value applies when flag not set", func(t *testing.T) {
		path := writeConfig(t, "logging:\n  level: \"info\"\n  format: \"json\"\n")
		level, format := resolveLogSettings(path, "", "text", false, false)
		if level != "info" || format != "json" {
			t.Fatalf("got (%q, %q), want (\"info\", \"json\")", level, format)
		}
	})

	t.Run("explicit flag wins over config value", func(t *testing.T) {
		path := writeConfig(t, "logging:\n  level: \"info\"\n  format: \"json\"\n")
		level, format := resolveLogSettings(path, "warn", "text", true, true)
		if level != "warn" || format != "text" {
			t.Fatalf("got (%q, %q), want (\"warn\", \"text\") since flags were explicitly set", level, format)
		}
	})

	t.Run("missing logging section leaves flags untouched", func(t *testing.T) {
		path := writeConfig(t, "providers: {}\n")
		level, format := resolveLogSettings(path, "", "text", false, false)
		if level != "" || format != "text" {
			t.Fatalf("got (%q, %q), want (\"\", \"text\")", level, format)
		}
	})

	t.Run("unreadable config path is not an error", func(t *testing.T) {
		level, format := resolveLogSettings(filepath.Join(t.TempDir(), "missing.yaml"), "warn", "text", false, false)
		if level != "warn" || format != "text" {
			t.Fatalf("got (%q, %q), want flags unchanged", level, format)
		}
	})
}

// TestIsLoopbackAddr is a regression test for a Copilot review finding on
// the BRIDGECTL_PPROF_ADDR debug endpoint: the loopback-only guarantee
// documented on startDebugPprof was not enforced, so a value like ":6061"
// or "0.0.0.0:6061" would expose the unauthenticated pprof endpoints to the
// network.
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6061", true},
		{"localhost:6061", true},
		{"[::1]:6061", true},
		{":6061", false},
		{"0.0.0.0:6061", false},
		{"[::]:6061", false},
		{"example.com:6061", false},
		{"10.0.0.5:6061", false},
		{"not-a-valid-addr", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
