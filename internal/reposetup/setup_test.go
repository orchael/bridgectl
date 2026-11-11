package reposetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrepareNoConfigReturnsBaseEnv(t *testing.T) {
	c := NewCoordinator(Options{Enabled: true})
	env, err := c.Prepare(context.Background(), t.TempDir(), []string{"PATH=/bin", "KEEP=1"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := strings.Join(env, "\n"); !strings.Contains(got, "KEEP=1") {
		t.Fatalf("env=%v want KEEP=1", env)
	}
}

func TestRepoSetupAcceptsUpdateSettings(t *testing.T) {
	repo := t.TempDir()
	writeSetup(t, repo, "version: 1\nsetup:\n  - export FROM_SETUP=ready\nupdate:\n  login: true\n  doctor: true\n  bridge: https://bridge.orchael.com\n")
	c := NewCoordinator(Options{Enabled: true})
	env, err := c.Prepare(context.Background(), repo, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(env, "FROM_SETUP"); got != "ready" {
		t.Fatalf("FROM_SETUP = %q, want ready", got)
	}
}

func TestPrepareDisabledReturnsProvidedEnv(t *testing.T) {
	c := NewCoordinator(Options{
		Enabled: false,
		BaseEnv: func() []string {
			return []string{"KEEP=default-base"}
		},
	})
	env, err := c.Prepare(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := envValue(env, "KEEP"); got != "default-base" {
		t.Fatalf("KEEP=%q want default-base", got)
	}
}

func TestPrepareDisabledZeroValueCoordinatorUsesProcessEnv(t *testing.T) {
	t.Setenv("BRIDGE_REPOSETUP_BASE_ENV_TEST", "present")
	c := &Coordinator{}
	env, err := c.Prepare(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := envValue(env, "BRIDGE_REPOSETUP_BASE_ENV_TEST"); got != "present" {
		t.Fatalf("BRIDGE_REPOSETUP_BASE_ENV_TEST=%q want present", got)
	}
}

func TestPrepareNilCoordinatorUsesProcessEnv(t *testing.T) {
	t.Setenv("BRIDGE_REPOSETUP_NIL_BASE_ENV_TEST", "present")
	var c *Coordinator
	env, err := c.Prepare(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := envValue(env, "BRIDGE_REPOSETUP_NIL_BASE_ENV_TEST"); got != "present" {
		t.Fatalf("BRIDGE_REPOSETUP_NIL_BASE_ENV_TEST=%q want present", got)
	}
}

func TestPrepareRunsSetupAndCapturesEnvironment(t *testing.T) {
	repo := t.TempDir()
	writeSetup(t, repo, `
version: 1
shell: bash
timeout: 5s
env:
  FROM_ENV: configured
setup:
  - export FROM_SETUP="$FROM_ENV-done"
`)

	c := NewCoordinator(Options{
		Enabled:        true,
		DefaultTimeout: time.Second,
		MaxTimeout:     10 * time.Second,
		BaseEnv: func() []string {
			return []string{"PATH=" + os.Getenv("PATH")}
		},
	})
	env, err := c.Prepare(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := envValue(env, "FROM_ENV"); got != "configured" {
		t.Fatalf("FROM_ENV=%q want configured", got)
	}
	if got := envValue(env, "FROM_SETUP"); got != "configured-done" {
		t.Fatalf("FROM_SETUP=%q want configured-done", got)
	}
}

func TestPrepareTimesOut(t *testing.T) {
	repo := t.TempDir()
	writeSetup(t, repo, `
version: 1
setup:
  - sleep 5
`)

	c := NewCoordinator(Options{Enabled: true, DefaultTimeout: 50 * time.Millisecond, MaxTimeout: time.Second})
	_, err := c.Prepare(context.Background(), repo, []string{"PATH=" + os.Getenv("PATH")})
	if !errors.Is(err, ErrSetupFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Prepare error=%v want timeout setup failure", err)
	}
}

func TestPrepareFailsWhenTimeoutExceedsMax(t *testing.T) {
	repo := t.TempDir()
	writeSetup(t, repo, `
version: 1
timeout: 20s
setup:
  - true
`)

	c := NewCoordinator(Options{Enabled: true, DefaultTimeout: time.Second, MaxTimeout: 2 * time.Second})
	_, err := c.Prepare(context.Background(), repo, []string{"PATH=" + os.Getenv("PATH")})
	if !errors.Is(err, ErrSetupFailed) || !strings.Contains(err.Error(), "exceeds max_timeout") {
		t.Fatalf("Prepare error=%v want max timeout setup failure", err)
	}
}

func TestLoadConfigValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "bad version", content: `version: 2`, want: "version must be 1"},
		{name: "bad shell", content: "version: 1\nshell: fish\n", want: "unsupported shell"},
		{name: "empty setup", content: "version: 1\nsetup: [\"\"]\n", want: "setup[0]"},
		{name: "bad env key", content: "version: 1\nenv:\n  1BAD: value\n", want: "env key"},
		{name: "bad yaml", content: "version: [", want: "parse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			writeSetup(t, repo, tc.content)
			c := NewCoordinator(Options{Enabled: true})
			_, err := c.Prepare(context.Background(), repo, []string{"PATH=" + os.Getenv("PATH")})
			if !errors.Is(err, ErrSetupFailed) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Prepare error=%v want %q", err, tc.want)
			}
		})
	}
}

func TestResolveConfigPathRejectsEscapes(t *testing.T) {
	repo := t.TempDir()
	for _, configPath := range []string{"/tmp/setup.yaml", "../setup.yaml"} {
		_, _, err := resolveConfigPath(repo, configPath)
		if !errors.Is(err, ErrSetupFailed) {
			t.Fatalf("resolveConfigPath(%q) error=%v want setup failure", configPath, err)
		}
	}
}

func TestParseCapturedEnvRequiresMarker(t *testing.T) {
	_, err := parseCapturedEnv([]byte("NO_MARKER=1\x00"))
	if err == nil || !strings.Contains(err.Error(), "marker missing") {
		t.Fatalf("parseCapturedEnv error=%v want marker missing", err)
	}
}

func TestDefaultRedact(t *testing.T) {
	got := defaultRedact("OPENAI_API_KEY=secret OTHER=value TOKEN=tok")
	if strings.Contains(got, "secret") || strings.Contains(got, "tok") {
		t.Fatalf("defaultRedact leaked secret: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("defaultRedact=%q want redaction marker", got)
	}
}

func TestPrepareFailsOnCommandErrorWithRedactedOutput(t *testing.T) {
	repo := t.TempDir()
	writeSetup(t, repo, `
version: 1
setup:
  - echo "API_KEY=secret-value"
  - exit 7
`)

	c := NewCoordinator(Options{
		Enabled:        true,
		DefaultTimeout: time.Second,
		MaxTimeout:     2 * time.Second,
		Redact: func(s string) string {
			return strings.ReplaceAll(s, "secret-value", "[REDACTED]")
		},
	})
	_, err := c.Prepare(context.Background(), repo, []string{"PATH=" + os.Getenv("PATH")})
	if !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("Prepare error=%v want setup failure", err)
	}
	if strings.Contains(err.Error(), "secret-value") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestPrepareSerializesConcurrentSameRepoSetup(t *testing.T) {
	repo := t.TempDir()
	countFile := filepath.Join(repo, "count")
	writeSetup(t, repo, `
version: 1
setup:
  - current="$(cat count 2>/dev/null || true)"
  - if [ -z "$current" ]; then current=0; fi
  - echo "$((current + 1))" > count
  - sleep 0.2
  - export SETUP_COUNT="$(cat count)"
`)

	var baseCalls atomic.Int32
	c := NewCoordinator(Options{
		Enabled:        true,
		DefaultTimeout: 2 * time.Second,
		MaxTimeout:     5 * time.Second,
		BaseEnv: func() []string {
			baseCalls.Add(1)
			return []string{"PATH=" + os.Getenv("PATH")}
		},
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env, err := c.Prepare(context.Background(), repo, nil)
			if err == nil && envValue(env, "SETUP_COUNT") != "1" {
				err = errors.New("unexpected setup count " + envValue(env, "SETUP_COUNT"))
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Fatalf("setup count=%q want 1", got)
	}
	if got := baseCalls.Load(); got != 2 {
		t.Fatalf("base env calls=%d want 2", got)
	}
}

func writeSetup(t *testing.T, repo, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".bridgectl.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}
