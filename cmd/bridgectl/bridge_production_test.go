package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orchael/bridgectl/internal/config"
)

type loginProbeTransport struct{ origin *string }

func (p loginProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*p.origin = r.URL.Scheme + "://" + r.URL.Host
	return nil, errors.New("stop before enrollment")
}

func TestProductionLoginURLArgument(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"positional overrides environment", []string{"https://bridge.orchael.com"}, "https://bridge.orchael.com"},
		{"flag preserved", []string{"--bridge", "https://selfhost.example"}, "https://selfhost.example"},
		{"environment preserved", nil, "https://environment.example"},
		{"conflicting selectors rejected", []string{"https://bridge.orchael.com", "--bridge", "https://other.example"}, ""},
		{"extra arguments rejected", []string{"https://bridge.orchael.com", "extra"}, ""},
		{"http rejected", []string{"http://bridge.orchael.com"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("BRIDGECTL_BRIDGE_URL", "https://environment.example")
			var origin string
			original := http.DefaultTransport
			http.DefaultTransport = loginProbeTransport{origin: &origin}
			t.Cleanup(func() { http.DefaultTransport = original })
			cmd := newBridgeLoginCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected probe or argument error")
			}
			if origin != tc.want {
				t.Fatalf("contacted %q, want %q", origin, tc.want)
			}
		})
	}
}

func TestProductionEnrollmentPreservesDesktopConfig(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	path := filepath.Join(xdg, "bridgectl", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("allowed_paths: [/workspace]\nserver:\n  listen: 127.0.0.1:9445\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tok := newControlDeviceToken()
	tok.BridgeURL = "https://bridge.orchael.com"
	tok.ControlEndpoint = "wss://control.bridge.orchael.com/v1/control"
	tok.TelemetryEndpoint = tok.BridgeURL + "/v1/telemetry/segments"
	if err := persistBridgeEnrollment(tok, "ai-desktop", tok.BridgeURL); err != nil {
		t.Fatal(err)
	}
	if bridgeConfigPath() != path {
		t.Fatal("enrollment shadowed desktop config")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Control.Endpoint != tok.ControlEndpoint || cfg.Telemetry.CollectorURL != tok.TelemetryEndpoint || !cfg.Telemetry.Enabled {
		t.Fatal("production endpoints or telemetry not configured")
	}
	if len(cfg.AllowedPaths) != 1 || cfg.AllowedPaths[0] != "/workspace" {
		t.Fatal("desktop workspace policy changed")
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(b), ".orchael.dev") {
		t.Fatal("config contains development endpoint or could not be read")
	}
}
