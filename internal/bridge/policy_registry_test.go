package bridge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

// trueBin is the absolute path to the "true" binary, resolved once via
// LookPath so tests work on both Linux (/bin/true) and macOS (/usr/bin/true).
var trueBin = func() string {
	if p, err := exec.LookPath("true"); err == nil {
		return p
	}
	return "/usr/bin/true"
}()

type registryProvider struct {
	id        string
	healthErr error
}

func (p *registryProvider) ID() string                    { return p.id }
func (p *registryProvider) Binary() string                { return trueBin }
func (p *registryProvider) PromptPattern() *regexp.Regexp { return nil }
func (p *registryProvider) StartupTimeout() time.Duration { return time.Second }
func (p *registryProvider) StopGrace() time.Duration      { return time.Second }
func (p *registryProvider) BuildCommand(context.Context, SessionConfig) (*exec.Cmd, error) {
	return exec.Command(trueBin), nil
}
func (p *registryProvider) ValidateStartup(context.Context) error { return nil }
func (p *registryProvider) Health(context.Context) error          { return p.healthErr }
func (p *registryProvider) Version(context.Context) (string, error) {
	return "v1", nil
}

func TestPolicyValidationAndRegistryHealth(t *testing.T) {
	repo := t.TempDir()
	policy := Policy{
		MaxPerProject: 1,
		MaxGlobal:     2,
		MaxInputBytes: 4,
		AllowedPaths:  []string{repo},
	}
	if err := policy.ValidateRepoPath(repo); err != nil {
		t.Fatalf("ValidateRepoPath: %v", err)
	}
	if err := policy.ValidateRepoPath(t.TempDir()); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ValidateRepoPath disallowed error=%v want %v", err, ErrInvalidArgument)
	}
	if err := policy.ValidateInput("1234"); err != nil {
		t.Fatalf("ValidateInput: %v", err)
	}
	if err := policy.ValidateInput("12345"); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("ValidateInput oversized error=%v want %v", err, ErrInputTooLarge)
	}
	if err := policy.ValidateInputBytes([]byte("12345")); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("ValidateInputBytes oversized error=%v want %v", err, ErrInputTooLarge)
	}

	registry := NewRegistry()
	if err := registry.Register(&registryProvider{id: "healthy"}); err != nil {
		t.Fatalf("Register healthy: %v", err)
	}
	if err := registry.Register(&registryProvider{id: "broken", healthErr: errors.New("down")}); err != nil {
		t.Fatalf("Register broken: %v", err)
	}
	if got := registry.List(); len(got) != 2 {
		t.Fatalf("List len=%d want 2", len(got))
	}
	results := registry.HealthAll(context.Background())
	if results["healthy"] != nil || results["broken"] == nil {
		t.Fatalf("HealthAll=%v", results)
	}
}

// TestValidateRepoPathRejectsSamePrefixSibling guards against a path-boundary
// bug: an allowed directory like "/home/mark" must not also permit the
// sibling "/home/mark-other", which it would under a bare string-prefix
// check since "/home/mark-other" literally starts with "/home/mark".
// Flagged by Copilot review on PR #279.
func TestValidateRepoPathRejectsSamePrefixSibling(t *testing.T) {
	base := t.TempDir()
	allowed := base + "/mark"
	sibling := base + "/mark-other"
	for _, dir := range []string{allowed, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	policy := Policy{AllowedPaths: []string{allowed}}
	if err := policy.ValidateRepoPath(allowed); err != nil {
		t.Fatalf("ValidateRepoPath(allowed): %v", err)
	}
	if err := policy.ValidateRepoPath(allowed + "/subdir"); err != nil {
		t.Fatalf("ValidateRepoPath(allowed subdir): %v", err)
	}
	if err := policy.ValidateRepoPath(sibling); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ValidateRepoPath(sibling) error=%v want %v", err, ErrInvalidArgument)
	}
}
