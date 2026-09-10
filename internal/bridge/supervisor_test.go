package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

type testProvider struct {
	id        string
	healthErr error
}

func (p *testProvider) ID() string                            { return p.id }
func (p *testProvider) Binary() string                        { return "/bin/cat" }
func (p *testProvider) PromptPattern() *regexp.Regexp         { return nil }
func (p *testProvider) StartupTimeout() time.Duration         { return time.Second }
func (p *testProvider) StopGrace() time.Duration              { return 50 * time.Millisecond }
func (p *testProvider) ValidateStartup(context.Context) error { return nil }
func (p *testProvider) Health(context.Context) error          { return p.healthErr }
func (p *testProvider) Version(context.Context) (string, error) {
	return "test-provider", nil
}
func (p *testProvider) BuildCommand(ctx context.Context, cfg SessionConfig) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "/bin/cat")
	cmd.Dir = cfg.RepoPath
	return cmd, nil
}

type termTrapProvider struct {
	testProvider
}

func (p *termTrapProvider) StopGrace() time.Duration { return 500 * time.Millisecond }

func (p *termTrapProvider) BuildCommand(ctx context.Context, cfg SessionConfig) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGracefulShutdownHelperProcess")
	cmd.Dir = cfg.RepoPath
	cmd.Env = append(os.Environ(), "BRIDGE_GRACEFUL_HELPER=1")
	return cmd, nil
}

type ignoreTermProvider struct {
	testProvider
}

func (p *ignoreTermProvider) StopGrace() time.Duration { return 5 * time.Second }

func (p *ignoreTermProvider) BuildCommand(ctx context.Context, cfg SessionConfig) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGracefulShutdownHelperProcess")
	cmd.Dir = cfg.RepoPath
	cmd.Env = append(os.Environ(), "BRIDGE_IGNORE_TERM_HELPER=1")
	return cmd, nil
}

func TestGracefulShutdownHelperProcess(t *testing.T) {
	switch {
	case os.Getenv("BRIDGE_GRACEFUL_HELPER") == "1":
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM)
		<-sigCh
		_, _ = os.Stdout.WriteString("BRIDGE_TERM_OK\n")
		os.Exit(0)
	case os.Getenv("BRIDGE_IGNORE_TERM_HELPER") == "1":
		signal.Ignore(syscall.SIGTERM)
		select {}
	default:
		return
	}
}

type setupRunnerFunc func(context.Context, string, []string) ([]string, error)

func (f setupRunnerFunc) Prepare(ctx context.Context, repoPath string, baseEnv []string) ([]string, error) {
	return f(ctx, repoPath, baseEnv)
}

type envHealthProvider struct {
	testProvider
	requiredKey string
}

func (p *envHealthProvider) Health(context.Context) error {
	return errors.New("HealthWithEnv was not used")
}

func (p *envHealthProvider) HealthWithEnv(_ context.Context, env []string) error {
	for _, item := range env {
		if strings.HasPrefix(item, p.requiredKey+"=") {
			return nil
		}
	}
	return errors.New("missing " + p.requiredKey)
}

type nilEnvProvider struct {
	testProvider
}

func (p *nilEnvProvider) Health(context.Context) error {
	return nil
}

func (p *nilEnvProvider) HealthWithEnv(context.Context, []string) error {
	return errors.New("HealthWithEnv should not be used without prepared env")
}

func TestSupervisorSessionLifecycle(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	supervisor := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	info, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID:   "project-a",
		SessionID:   "session-a",
		RepoPath:    t.TempDir(),
		Options:     map[string]string{"provider": "fake"},
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Provider != "fake" {
		t.Fatalf("Provider=%q want %q", info.Provider, "fake")
	}

	state, err := supervisor.Attach("session-a", "client-a", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := supervisor.Attach("session-a", "client-b", 0, AttachRoleWriter); !errors.Is(err, ErrWriterConflict) {
		t.Fatalf("Attach while attached error=%v want %v", err, ErrWriterConflict)
	}

	if _, err := supervisor.WriteInput("session-a", "wrong-client", []byte("hello\n")); !errors.Is(err, ErrClientMismatch) {
		t.Fatalf("WriteInput wrong client error=%v want %v", err, ErrClientMismatch)
	}
	if err := supervisor.Resize("session-a", "wrong-client", 100, 40); !errors.Is(err, ErrClientMismatch) {
		t.Fatalf("Resize wrong client error=%v want %v", err, ErrClientMismatch)
	}

	if _, err := supervisor.WriteInput("session-a", "client-a", []byte("hello\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	chunk := waitForChunk(t, state.Live, "hello")
	if !bytes.Contains(chunk.Payload, []byte("hello")) {
		t.Fatalf("chunk payload=%q does not contain hello", string(chunk.Payload))
	}

	if err := supervisor.Resize("session-a", "client-a", 100, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	got, err := supervisor.Get("session-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Cols != 100 || got.Rows != 40 {
		t.Fatalf("size=%dx%d want 100x40", got.Cols, got.Rows)
	}

	if _, err := supervisor.Detach("session-a", "client-a"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	replayState, err := supervisor.Attach("session-a", "client-b", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach replay: %v", err)
	}
	if len(replayState.Replay) == 0 {
		t.Fatal("Replay was empty, want buffered output")
	}
	if _, err := supervisor.Detach("session-a", "client-b"); err != nil {
		t.Fatalf("Detach replay client: %v", err)
	}

	items := supervisor.List("project-a")
	if len(items) != 1 {
		t.Fatalf("List len=%d want 1", len(items))
	}

	if err := supervisor.Stop("session-a", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForStopped(t, supervisor, "session-a")
}

func TestSupervisorNilEnvUsesProviderHealth(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&nilEnvProvider{testProvider: testProvider{id: "nil-env"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	info, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-a",
		SessionID: "nil-env",
		RepoPath:  t.TempDir(),
		Options:   map[string]string{"provider": "nil-env"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Provider != "nil-env" {
		t.Fatalf("Provider=%q want nil-env", info.Provider)
	}
	_ = supervisor.Stop("nil-env", true)
}

func TestSupervisorRepoSetupRunnerProvidesProviderHealthEnv(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&envHealthProvider{
		testProvider: testProvider{id: "env-provider"},
		requiredKey:  "FROM_SETUP",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := NewSupervisor(
		registry,
		DefaultPolicy(),
		1024,
		time.Minute,
		WithRepoSetupRunner(setupRunnerFunc(func(_ context.Context, _ string, _ []string) ([]string, error) {
			return []string{"FROM_SETUP=1"}, nil
		})),
	)
	defer supervisor.Close()

	info, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-a",
		SessionID: "setup-env",
		RepoPath:  t.TempDir(),
		Options:   map[string]string{"provider": "env-provider"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Provider != "env-provider" {
		t.Fatalf("Provider=%q want env-provider", info.Provider)
	}
	_ = supervisor.Stop("setup-env", true)
}

func TestSupervisorRepoSetupRunnerFailure(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := NewSupervisor(
		registry,
		DefaultPolicy(),
		1024,
		time.Minute,
		WithRepoSetupRunner(setupRunnerFunc(func(context.Context, string, []string) ([]string, error) {
			return nil, errors.New("repo setup failed: boom")
		})),
	)
	defer supervisor.Close()

	_, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-a",
		SessionID: "setup-fail",
		RepoPath:  t.TempDir(),
		Options:   map[string]string{"provider": "fake"},
	})
	if !errors.Is(err, ErrRepoSetupFailed) || strings.Contains(err.Error(), "repo setup failed: repo setup failed") {
		t.Fatalf("Start error=%v want trimmed repo setup failure", err)
	}
}

func TestSupervisorStartValidationAndLimits(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(&testProvider{id: "bad", healthErr: errors.New("down")}); err != nil {
		t.Fatalf("Register bad: %v", err)
	}

	supervisor := NewSupervisor(registry, Policy{MaxPerProject: 1, MaxGlobal: 1}, 1024, time.Minute)
	defer supervisor.Close()

	if _, err := supervisor.Start(context.Background(), SessionConfig{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Start empty error=%v want %v", err, ErrInvalidArgument)
	}

	repo := t.TempDir()
	if _, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-a",
		SessionID: "session-a",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "bad"},
	}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("Start bad provider error=%v want %v", err, ErrProviderUnavailable)
	}

	if _, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-a",
		SessionID: "session-a",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "fake"},
	}); err != nil {
		t.Fatalf("Start first: %v", err)
	}
	if _, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-a",
		SessionID: "session-a",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "fake"},
	}); !errors.Is(err, ErrSessionAlreadyExists) {
		t.Fatalf("Start duplicate error=%v want %v", err, ErrSessionAlreadyExists)
	}
	if _, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-a",
		SessionID: "session-b",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "fake"},
	}); !errors.Is(err, ErrSessionLimitReached) {
		t.Fatalf("Start limit error=%v want %v", err, ErrSessionLimitReached)
	}
}

func TestSupervisorPersistenceAndHistory(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dbPath := t.TempDir() + "/sessions.db"
	store, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}

	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store))
	defer sup.Close()

	repo := t.TempDir()
	if _, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "proj-a",
		SessionID: "persist-1",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "fake"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Stop the session so it reaches a terminal state and is persisted.
	if err := sup.Stop("persist-1", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForStopped(t, sup, "persist-1")
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Simulate a daemon restart: open a fresh supervisor with the same store.
	store2, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	sup2 := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store2))
	defer sup2.Close()
	defer func() { _ = store2.Close() }()

	if err := sup2.LoadHistory(); err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}

	// The stopped session must be visible via Get and List.
	info, err := sup2.Get("persist-1")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if info.State != SessionStateStopped && info.State != SessionStateFailed {
		t.Errorf("State=%v want Stopped or Failed", info.State)
	}
	if info.ProjectID != "proj-a" {
		t.Errorf("ProjectID=%q want %q", info.ProjectID, "proj-a")
	}

	list := sup2.List("proj-a")
	found := false
	for _, s := range list {
		if s.SessionID == "persist-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("persist-1 not found in List after restart")
	}
}

func TestSupervisorShutdownGracefullyStopsAndPersistsRunningSession(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&termTrapProvider{testProvider: testProvider{id: "term-trap"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dbPath := t.TempDir() + "/sessions.db"
	store, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}

	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store))
	if _, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "proj-shutdown",
		SessionID: "shutdown-1",
		RepoPath:  t.TempDir(),
		Options:   map[string]string{"provider": "term-trap"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sup.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	store2, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store2.Close() }()
	sup2 := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store2))
	defer sup2.Close()
	if err := sup2.LoadHistory(); err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}

	info, err := sup2.Get("shutdown-1")
	if err != nil {
		t.Fatalf("Get after shutdown: %v", err)
	}
	if info.State != SessionStateStopped {
		t.Fatalf("State=%v want Stopped", info.State)
	}
	if !info.ExitRecorded {
		t.Fatal("ExitRecorded=false want true")
	}
	if strings.Contains(info.Error, "orphaned by daemon restart") {
		t.Fatalf("Error=%q should not mark graceful shutdown as orphaned", info.Error)
	}
}

func TestSupervisorShutdownRejectsNewSessions(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sup.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	_, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "proj-shutdown",
		SessionID: "shutdown-reject",
		RepoPath:  t.TempDir(),
		Options:   map[string]string{"provider": "fake"},
	})
	if !errors.Is(err, ErrSupervisorShuttingDown) {
		t.Fatalf("Start after shutdown error=%v want %v", err, ErrSupervisorShuttingDown)
	}
}

func TestSupervisorShutdownForceStopWaitsForTerminalPersistence(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&ignoreTermProvider{testProvider: testProvider{id: "ignore-term"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dbPath := t.TempDir() + "/sessions.db"
	store, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}

	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store))
	if _, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "proj-shutdown",
		SessionID: "shutdown-force-1",
		RepoPath:  t.TempDir(),
		Options:   map[string]string{"provider": "ignore-term"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the session process to be running before attempting shutdown,
	// otherwise the process may exit before the deadline fires.
	for range 50 {
		info, _ := sup.Get("shutdown-force-1")
		if info.State == SessionStateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := sup.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v want %v", err, context.DeadlineExceeded)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	store2, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store2.Close() }()
	sup2 := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store2))
	defer sup2.Close()
	if err := sup2.LoadHistory(); err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}

	info, err := sup2.Get("shutdown-force-1")
	if err != nil {
		t.Fatalf("Get after shutdown: %v", err)
	}
	if info.State != SessionStateStopped {
		t.Fatalf("State=%v want Stopped", info.State)
	}
	if !info.ExitRecorded {
		t.Fatal("ExitRecorded=false want true")
	}
	if strings.Contains(info.Error, "orphaned by daemon restart") {
		t.Fatalf("Error=%q should not mark forced shutdown as orphaned", info.Error)
	}
}

func TestSupervisorHistoryOrphansMarkedFailed(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dbPath := t.TempDir() + "/sessions.db"

	// Seed the store with a running session (simulating a crash).
	store, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}
	orphan := SessionInfo{
		SessionID: "orphan-1",
		ProjectID: "proj-b",
		Provider:  "fake",
		State:     SessionStateRunning,
		CreatedAt: nowUTC(),
	}
	if err := store.Save(orphan); err != nil {
		t.Fatalf("Save orphan: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Restart: orphan must be marked Failed.
	store2, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store2))
	defer sup.Close()
	defer func() { _ = store2.Close() }()

	if err := sup.LoadHistory(); err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}

	info, err := sup.Get("orphan-1")
	if err != nil {
		t.Fatalf("Get orphan: %v", err)
	}
	if info.State != SessionStateFailed {
		t.Errorf("State=%v want Failed", info.State)
	}
	if info.Error == "" {
		t.Errorf("Error should be set for orphaned session")
	}
}

func TestSupervisorLoadHistoryRecoversRunningProcess(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})

	dbPath := t.TempDir() + "/sessions.db"
	store, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}
	recovered := SessionInfo{
		SessionID: "recover-1",
		ProjectID: "proj-r",
		Provider:  "fake",
		State:     SessionStateRunning,
		CreatedAt: nowUTC(),
		ProcessID: cmd.Process.Pid,
	}
	if err := store.Save(recovered); err != nil {
		t.Fatalf("Save recovered session: %v", err)
	}
	chunk := OutputChunk{Seq: 1, Timestamp: nowUTC(), Payload: []byte("persisted output")}
	if err := store.SaveChunk("recover-1", chunk); err != nil {
		t.Fatalf("SaveChunk: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	store2, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store2))
	defer sup.Close()
	defer func() { _ = store2.Close() }()

	if err := sup.LoadHistory(); err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}

	info, err := sup.Get("recover-1")
	if err != nil {
		t.Fatalf("Get recover-1: %v", err)
	}
	if info.State != SessionStateRunning {
		t.Fatalf("State=%v want Running", info.State)
	}
	if !info.Recovered {
		t.Fatal("Recovered flag was false")
	}

	attach, err := sup.Attach("recover-1", "client-a", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach recovered: %v", err)
	}
	if len(attach.Replay) != 1 {
		t.Fatalf("Replay len=%d want 1", len(attach.Replay))
	}
	select {
	case _, ok := <-attach.Live:
		if ok {
			t.Fatal("recovered live channel should be closed")
		}
	default:
		t.Fatal("recovered live channel should be immediately closed")
	}
	attachAfter, err := sup.Attach("recover-1", "client-b", chunk.Seq, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach recovered after seq: %v", err)
	}
	if len(attachAfter.Replay) != 0 {
		t.Fatalf("Replay after persisted seq len=%d want 0", len(attachAfter.Replay))
	}

	if _, err := sup.WriteInput("recover-1", "client-a", []byte("hello")); !errors.Is(err, ErrSessionRecoveryUnavailable) {
		t.Fatalf("WriteInput recovered error=%v want %v", err, ErrSessionRecoveryUnavailable)
	}

	if err := sup.Stop("recover-1", true); err != nil {
		t.Fatalf("Stop recovered: %v", err)
	}
	waitForRecoveredStopped(t, sup, "recover-1")
}

func TestSupervisorHistoryChunkReplay(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dbPath := t.TempDir() + "/sessions.db"
	store, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}

	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store))
	repo := t.TempDir()
	if _, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "proj-a",
		SessionID: "replay-1",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "fake"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Write some input so /bin/cat echoes it into the PTY buffer.
	state, err := sup.Attach("replay-1", "client-a", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := sup.WriteInput("replay-1", "client-a", []byte("hello\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	waitForChunk(t, state.Live, "hello")
	if _, err := sup.Detach("replay-1", "client-a"); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	// Stop and let the session reach a terminal state.
	if err := sup.Stop("replay-1", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForStopped(t, sup, "replay-1")
	sup.Close()
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Simulate daemon restart: open a fresh supervisor with the same store.
	store2, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	sup2 := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute, WithStore(store2))
	defer sup2.Close()
	defer func() { _ = store2.Close() }()

	if err := sup2.LoadHistory(); err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}

	// AttachSession on a history session must return replay chunks from the store.
	state2, err := sup2.Attach("replay-1", "client-b", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach history session: %v", err)
	}
	if len(state2.Replay) == 0 {
		t.Fatal("expected non-empty replay for history session")
	}
	var found bool
	for _, c := range state2.Replay {
		if bytes.Contains(c.Payload, []byte("hello")) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'hello' in history replay, got %d chunks", len(state2.Replay))
	}
	// Live channel must be closed (no running process).
	select {
	case _, ok := <-state2.Live:
		if ok {
			t.Error("live channel should be closed for history session")
		}
	default:
		t.Error("live channel should be immediately readable (closed)")
	}
}

// streamJSONTestProvider wraps testProvider and implements StreamJSONProvider.
// BuildCommand runs a shell one-liner that prints a fixed JSONL payload and exits.
type streamJSONTestProvider struct {
	testProvider
	jsonLines []string
}

func (p *streamJSONTestProvider) IsStreamJSON() bool { return true }

func (p *streamJSONTestProvider) BuildCommand(ctx context.Context, cfg SessionConfig) (*exec.Cmd, error) {
	// Construct a printf call that emits each line.
	args := make([]string, 0, len(p.jsonLines)*2+2)
	args = append(args, "-c")
	script := ""
	for _, line := range p.jsonLines {
		script += "printf '%s\\n' '" + line + "';"
	}
	args = append(args, script)
	cmd := exec.CommandContext(ctx, "/bin/sh", args...)
	cmd.Dir = cfg.RepoPath
	return cmd, nil
}

// streamJSONGatedProvider wraps streamJSONTestProvider but keeps the
// process alive until a signal file appears. This prevents the provider
// from exiting before the test has called Attach.
type streamJSONGatedProvider struct {
	streamJSONTestProvider
	signalPath string // the process waits for this file to appear before exiting
}

func (p *streamJSONGatedProvider) BuildCommand(ctx context.Context, cfg SessionConfig) (*exec.Cmd, error) {
	script := ""
	for _, line := range p.jsonLines {
		script += "printf '%s\\n' '" + line + "';"
	}
	// After printing lines, poll for the signal file every 10ms up to 10s.
	script += fmt.Sprintf("i=0; while [ ! -f '%s' ] && [ $i -lt 1000 ]; do sleep 0.01; i=$((i+1)); done;", p.signalPath)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = cfg.RepoPath
	return cmd, nil
}

func TestReadLoopStreamJSONParsing(t *testing.T) {
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 64*1024, time.Minute)
	defer sup.Close()

	liveCh := make(chan OutputChunk, 100)
	ms := &managedSession{
		buf: NewByteBuffer(64 * 1024),
		observers: map[string]*observerEntry{
			"test-client": {ch: liveCh, role: AttachRoleWriter},
		},
		info: SessionInfo{SessionID: "test-stream"},
	}

	lines := []string{
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello world"}}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"deep thought"}}`,
		`not json at all`,
		`{"type":"other_event"}`,
	}
	pr, pw := io.Pipe()
	go func() {
		for _, line := range lines {
			_, _ = pw.Write([]byte(line + "\n"))
		}
		_ = pw.Close()
	}()

	// readLoopStreamJSON blocks until EOF; closeLive closes ms.live on return.
	sup.readLoopStreamJSON(ms, pr)

	chunks := ms.buf.After(0)
	if len(chunks) == 0 {
		t.Fatal("expected chunks in buffer, got none")
	}

	var textChunks, thinkingChunks, rawChunks []OutputChunk
	for _, c := range chunks {
		switch c.Type {
		case ChunkTypeOutput:
			textChunks = append(textChunks, c)
		case ChunkTypeThinking:
			thinkingChunks = append(thinkingChunks, c)
		}
		rawChunks = append(rawChunks, c)
	}

	// text_delta → ChunkTypeOutput
	if len(textChunks) == 0 {
		t.Error("expected at least one ChunkTypeOutput chunk")
	}
	var foundText bool
	for _, c := range textChunks {
		if bytes.Contains(c.Payload, []byte("hello world")) {
			foundText = true
		}
	}
	if !foundText {
		t.Errorf("expected 'hello world' in ChunkTypeOutput chunks, got %v", textChunks)
	}

	// thinking_delta → ChunkTypeThinking
	if len(thinkingChunks) == 0 {
		t.Error("expected at least one ChunkTypeThinking chunk")
	}
	var foundThinking bool
	for _, c := range thinkingChunks {
		if bytes.Contains(c.Payload, []byte("deep thought")) {
			foundThinking = true
		}
	}
	if !foundThinking {
		t.Errorf("expected 'deep thought' in ChunkTypeThinking chunks, got %v", thinkingChunks)
	}

	// Non-JSON line → raw ChunkTypeOutput chunk
	var foundRaw bool
	for _, c := range rawChunks {
		if bytes.Contains(c.Payload, []byte("not json")) {
			foundRaw = true
		}
	}
	if !foundRaw {
		t.Error("expected non-JSON line to be emitted as raw ChunkTypeOutput chunk")
	}
}

func TestReadLoopStreamJSONHandlesLargeLines(t *testing.T) {
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 256*1024, time.Minute)
	defer sup.Close()

	ms := &managedSession{
		buf:  NewByteBuffer(256 * 1024),
		info: SessionInfo{SessionID: "test-large-stream"},
	}

	large := strings.Repeat("x", 70*1024)
	line := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + large + `"}}`

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(line + "\n"))
		_ = pw.Close()
	}()

	sup.readLoopStreamJSON(ms, pr)

	chunks := ms.buf.After(0)
	if len(chunks) != 1 {
		t.Fatalf("chunks len=%d want 1", len(chunks))
	}
	if got := string(chunks[0].Payload); got != large {
		t.Fatalf("payload len=%d want %d", len(got), len(large))
	}
}

func TestMonitorRecoveredProcessStopsOnSupervisorClose(t *testing.T) {
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute)
	ms := &managedSession{
		info: SessionInfo{
			SessionID: "recovered-1",
			ProcessID: 999999,
			State:     SessionStateRunning,
		},
	}

	done := make(chan struct{})
	go func() {
		sup.monitorRecoveredProcess(ms)
		close(done)
	}()

	sup.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorRecoveredProcess did not exit after supervisor close")
	}
}

func TestStreamJSONSessionLifecycle(t *testing.T) {
	jsonLines := []string{
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}`,
	}
	p := &streamJSONTestProvider{
		testProvider: testProvider{id: "stream-fake"},
		jsonLines:    jsonLines,
	}
	registry := NewRegistry()
	if err := registry.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	sup := NewSupervisor(registry, DefaultPolicy(), 64*1024, time.Minute)
	defer sup.Close()

	repo := t.TempDir()
	info, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "proj-stream",
		SessionID: "stream-1",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "stream-fake"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Provider != "stream-fake" {
		t.Fatalf("Provider=%q want stream-fake", info.Provider)
	}

	// Attach and wait for at least one chunk from the process output.
	state, err := sup.Attach("stream-1", "client-x", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Seed collected with any chunks already buffered before Attach was called.
	collected := make([]OutputChunk, len(state.Replay))
	copy(collected, state.Replay)

	// Drain the live channel until closed (process exits).
	timeout := time.After(5 * time.Second)
drainLoop:
	for {
		select {
		case c, ok := <-state.Live:
			if !ok {
				break drainLoop
			}
			collected = append(collected, c)
		case <-timeout:
			t.Fatal("timed out waiting for stream-JSON session to complete")
		}
	}

	// Check for text and thinking chunks.
	var sawText, sawThinking bool
	for _, c := range collected {
		if c.Type == ChunkTypeOutput && bytes.Contains(c.Payload, []byte("answer")) {
			sawText = true
		}
		if c.Type == ChunkTypeThinking && bytes.Contains(c.Payload, []byte("thinking")) {
			sawThinking = true
		}
	}
	if !sawText {
		t.Errorf("expected text chunk with 'answer', got %d chunks", len(collected))
	}
	if !sawThinking {
		t.Errorf("expected thinking chunk with 'thinking', got %d chunks", len(collected))
	}
}

// TestStreamJSONThinkingEventsReplay verifies that THINKING events emitted by a
// stream-JSON provider are properly buffered and replayed to a client that
// attaches after the events have been emitted. (Issue #1, issue #153)
func TestStreamJSONThinkingEventsReplay(t *testing.T) {
	jsonLines := []string{
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reasoning step 1"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"final answer"}}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reasoning step 2"}}`,
	}

	// Use a signal file so the provider waits after printing its lines,
	// giving the test time to Attach before the session exits. Without
	// this synchronisation the provider can finish before Attach registers
	// the observer, which causes Detach to return ErrClientMismatch.
	signalFile := filepath.Join(t.TempDir(), "done")
	p := &streamJSONGatedProvider{
		streamJSONTestProvider: streamJSONTestProvider{
			testProvider: testProvider{id: "stream-replay"},
			jsonLines:    jsonLines,
		},
		signalPath: signalFile,
	}
	registry := NewRegistry()
	if err := registry.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	sup := NewSupervisor(registry, DefaultPolicy(), 64*1024, time.Minute)
	defer sup.Close()

	repo := t.TempDir()
	_, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "proj-replay",
		SessionID: "replay-1",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "stream-replay"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Attach as first client before the provider exits.
	state, err := sup.Attach("replay-1", "client-first", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Signal the provider to exit now that the observer is registered.
	if err := os.WriteFile(signalFile, []byte("done"), 0o644); err != nil {
		t.Fatalf("write signal file: %v", err)
	}

	timeout := time.After(5 * time.Second)
drainLoop:
	for {
		select {
		case _, ok := <-state.Live:
			if !ok {
				break drainLoop
			}
		case <-timeout:
			t.Fatal("timed out waiting for stream-JSON session to complete")
		}
	}
	if _, err := sup.Detach("replay-1", "client-first"); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	// Now attach as a second observer client — should get replay from ring buffer.
	state2, err := sup.Attach("replay-1", "client-replay", 0, AttachRoleObserver)
	if err != nil {
		t.Fatalf("Attach replay: %v", err)
	}

	// All events should be in state2.Replay (process already exited).
	replay := state2.Replay
	var thinkingChunks []OutputChunk
	var textChunks []OutputChunk
	for _, c := range replay {
		switch c.Type {
		case ChunkTypeThinking:
			thinkingChunks = append(thinkingChunks, c)
		case ChunkTypeOutput:
			textChunks = append(textChunks, c)
		}
	}

	if len(thinkingChunks) < 2 {
		t.Errorf("expected at least 2 THINKING replay chunks, got %d", len(thinkingChunks))
	}
	var foundStep1, foundStep2 bool
	for _, c := range thinkingChunks {
		if bytes.Contains(c.Payload, []byte("reasoning step 1")) {
			foundStep1 = true
		}
		if bytes.Contains(c.Payload, []byte("reasoning step 2")) {
			foundStep2 = true
		}
	}
	if !foundStep1 {
		t.Error("expected 'reasoning step 1' in replayed THINKING chunks")
	}
	if !foundStep2 {
		t.Error("expected 'reasoning step 2' in replayed THINKING chunks")
	}

	if len(textChunks) == 0 {
		t.Error("expected at least 1 text OUTPUT replay chunk")
	}
	var foundAnswer bool
	for _, c := range textChunks {
		if bytes.Contains(c.Payload, []byte("final answer")) {
			foundAnswer = true
		}
	}
	if !foundAnswer {
		t.Error("expected 'final answer' in replayed OUTPUT chunks")
	}

	// Verify chunk types are preserved in the ring buffer.
	for _, c := range replay {
		if c.Type == ChunkTypeThinking && c.Seq == 0 {
			t.Error("THINKING chunk in replay has zero sequence number")
		}
	}
}

func waitForChunk(t *testing.T, ch <-chan OutputChunk, needle string) OutputChunk {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case chunk := <-ch:
			if bytes.Contains(chunk.Payload, []byte(needle)) {
				return chunk
			}
		case <-timeout:
			t.Fatalf("timed out waiting for chunk containing %q", needle)
		}
	}
}

func TestSupervisorFallbackProvider(t *testing.T) {
	registry := NewRegistry()
	_ = registry.Register(&testProvider{id: "primary", healthErr: errors.New("down")})
	_ = registry.Register(&testProvider{id: "fallback1", healthErr: errors.New("also down")})
	_ = registry.Register(&testProvider{id: "fallback2"})

	supervisor := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	repo := t.TempDir()

	t.Run("primary succeeds, no fallback used", func(t *testing.T) {
		_ = registry.Register(&testProvider{id: "ok"})
		info, err := supervisor.Start(context.Background(), SessionConfig{
			ProjectID: "project-a",
			SessionID: "s-ok",
			RepoPath:  repo,
			Options:   map[string]string{"provider": "ok"},
			Fallbacks: []string{"fallback2"},
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if info.Provider != "ok" {
			t.Fatalf("Provider=%q want ok", info.Provider)
		}
		_ = supervisor.Stop("s-ok", true)
		waitForStopped(t, supervisor, "s-ok")
	})

	t.Run("primary down, first fallback down, second succeeds", func(t *testing.T) {
		info, err := supervisor.Start(context.Background(), SessionConfig{
			ProjectID: "project-a",
			SessionID: "s-fallback",
			RepoPath:  repo,
			Options:   map[string]string{"provider": "primary"},
			Fallbacks: []string{"fallback1", "fallback2"},
		})
		if err != nil {
			t.Fatalf("Start with fallback: %v", err)
		}
		if info.Provider != "fallback2" {
			t.Fatalf("Provider=%q want fallback2", info.Provider)
		}
		_ = supervisor.Stop("s-fallback", true)
		waitForStopped(t, supervisor, "s-fallback")
	})

	t.Run("all providers down returns error", func(t *testing.T) {
		_, err := supervisor.Start(context.Background(), SessionConfig{
			ProjectID: "project-a",
			SessionID: "s-allfail",
			RepoPath:  repo,
			Options:   map[string]string{"provider": "primary"},
			Fallbacks: []string{"fallback1"},
		})
		if !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("Start all-down error=%v want %v", err, ErrProviderUnavailable)
		}
	})

	t.Run("unknown primary with no fallbacks returns error", func(t *testing.T) {
		_, err := supervisor.Start(context.Background(), SessionConfig{
			ProjectID: "project-a",
			SessionID: "s-unknown",
			RepoPath:  repo,
			Options:   map[string]string{"provider": "nonexistent"},
		})
		if !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("Start unknown error=%v want %v", err, ErrProviderUnavailable)
		}
	})
}

// stripANSITestProvider wraps testProvider and implements StripANSIProvider.
type stripANSITestProvider struct {
	testProvider
}

func (p *stripANSITestProvider) IsStripANSI() bool { return true }

func TestReadLoopStripsANSIEscapeCodes(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&stripANSITestProvider{testProvider: testProvider{id: "ansi-fake"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)
	defer sup.Close()

	if _, err := sup.Start(context.Background(), SessionConfig{
		ProjectID:   "proj-ansi",
		SessionID:   "ansi-1",
		RepoPath:    t.TempDir(),
		Options:     map[string]string{"provider": "ansi-fake"},
		InitialCols: 80,
		InitialRows: 24,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	state, err := sup.Attach("ansi-1", "client-ansi", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Write ANSI-wrapped text; /bin/cat echoes it back through the PTY.
	ansiInput := "\x1b[32mBRIDGE_ANSI_OK\x1b[0m\n"
	if _, err := sup.WriteInput("ansi-1", "client-ansi", []byte(ansiInput)); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	// Collect chunks until we see the marker or time out.
	deadline := time.After(3 * time.Second)
	var received []byte
outer:
	for {
		select {
		case chunk, ok := <-state.Live:
			if !ok {
				break outer
			}
			received = append(received, chunk.Payload...)
			if bytes.Contains(received, []byte("BRIDGE_ANSI_OK")) {
				break outer
			}
		case <-deadline:
			t.Fatal("timed out waiting for echo from ansi-fake provider")
		}
	}

	if !bytes.Contains(received, []byte("BRIDGE_ANSI_OK")) {
		t.Fatalf("marker not found in output: %q", string(received))
	}
	// Ensure no raw escape bytes survived stripping.
	if bytes.Contains(received, []byte("\x1b[")) {
		t.Fatalf("ANSI escape codes not stripped from output: %q", string(received))
	}

	_ = sup.Stop("ansi-1", true)
	waitForStopped(t, sup, "ansi-1")
}

func waitForStopped(t *testing.T, supervisor *Supervisor, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, err := supervisor.Get(sessionID)
		if err == nil && info.ExitRecorded {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to stop", sessionID)
}

func waitForRecoveredStopped(t *testing.T, supervisor *Supervisor, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, err := supervisor.Get(sessionID)
		if err == nil && (info.State == SessionStateStopped || info.State == SessionStateFailed) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for recovered session %q to stop", sessionID)
}

func newTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sup := NewSupervisor(registry, DefaultPolicy(), 1024*1024, time.Minute)
	t.Cleanup(func() { sup.Close() })
	return sup
}

func startTestSession(t *testing.T, sup *Supervisor, sessionID string) *SessionInfo {
	t.Helper()
	info, err := sup.Start(context.Background(), SessionConfig{
		ProjectID:   "project-test",
		SessionID:   sessionID,
		RepoPath:    t.TempDir(),
		Options:     map[string]string{"provider": "fake"},
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		t.Fatalf("Start %s: %v", sessionID, err)
	}
	return info
}

func TestMultiObserverFanOut(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "fan-out")

	w, err := sup.Attach("fan-out", "writer", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach writer: %v", err)
	}
	o1, err := sup.Attach("fan-out", "obs-1", 0, AttachRoleObserver)
	if err != nil {
		t.Fatalf("Attach observer 1: %v", err)
	}
	o2, err := sup.Attach("fan-out", "obs-2", 0, AttachRoleObserver)
	if err != nil {
		t.Fatalf("Attach observer 2: %v", err)
	}

	if _, err := sup.WriteInput("fan-out", "writer", []byte("ping\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	for label, ch := range map[string]<-chan OutputChunk{"writer": w.Live, "obs-1": o1.Live, "obs-2": o2.Live} {
		c := waitForChunk(t, ch, "ping")
		if !bytes.Contains(c.Payload, []byte("ping")) {
			t.Errorf("%s: expected 'ping' in chunk", label)
		}
	}
}

func TestWriterConflictWithObserverAllowed(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "conflict")

	if _, err := sup.Attach("conflict", "writer-1", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach writer-1: %v", err)
	}
	// Second writer must fail.
	if _, err := sup.Attach("conflict", "writer-2", 0, AttachRoleWriter); !errors.Is(err, ErrWriterConflict) {
		t.Fatalf("want ErrWriterConflict, got %v", err)
	}
	// Observers are always allowed.
	if _, err := sup.Attach("conflict", "obs-1", 0, AttachRoleObserver); err != nil {
		t.Fatalf("Attach observer while writer held: %v", err)
	}
}

func TestClaimWriterForce(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "claim")

	// First client attaches as observer (will be upgraded to writer via ClaimWriter).
	if _, err := sup.Attach("claim", "old-writer", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach old-writer: %v", err)
	}
	// New client attaches as observer.
	if _, err := sup.Attach("claim", "new-client", 0, AttachRoleObserver); err != nil {
		t.Fatalf("Attach new-client as observer: %v", err)
	}

	// Force-claim the writer slot.
	result, err := sup.ClaimWriter("claim", "new-client", true)
	if err != nil {
		t.Fatalf("ClaimWriter force: %v", err)
	}
	if result.PreviousWriterClientID != "old-writer" {
		t.Errorf("PreviousClientID=%q want %q", result.PreviousWriterClientID, "old-writer")
	}

	// Confirm old-writer is now observer.
	info, err := sup.Get("claim")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.ActiveWriterClientID != "new-client" {
		t.Errorf("ActiveWriterClientID=%q want new-client", info.ActiveWriterClientID)
	}
}

func TestClaimWriterNoForceConflict(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "claim-noforce")

	if _, err := sup.Attach("claim-noforce", "existing-writer", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := sup.Attach("claim-noforce", "new-obs", 0, AttachRoleObserver); err != nil {
		t.Fatalf("Attach observer: %v", err)
	}

	_, err := sup.ClaimWriter("claim-noforce", "new-obs", false)
	if !errors.Is(err, ErrWriterConflict) {
		t.Fatalf("want ErrWriterConflict without force, got %v", err)
	}
}

func TestReleaseWriter(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "release")

	if _, err := sup.Attach("release", "the-writer", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	info, err := sup.Get("release")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.ActiveWriterClientID != "the-writer" {
		t.Fatalf("expected the-writer to hold writer slot")
	}

	if err := sup.ReleaseWriter("release", "the-writer"); err != nil {
		t.Fatalf("ReleaseWriter: %v", err)
	}

	info, err = sup.Get("release")
	if err != nil {
		t.Fatalf("Get after release: %v", err)
	}
	if info.ActiveWriterClientID != "" {
		t.Errorf("ActiveWriterClientID=%q want empty after release", info.ActiveWriterClientID)
	}
}

func TestReleaseWriterNonWriter(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "release-nonwriter")

	if _, err := sup.Attach("release-nonwriter", "obs", 0, AttachRoleObserver); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Releasing when you are not the writer should error.
	if err := sup.ReleaseWriter("release-nonwriter", "obs"); err == nil {
		t.Fatal("expected error releasing writer as observer, got nil")
	}
}

func TestDetachClearsWriterSlot(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "detach-clear")

	state, err := sup.Attach("detach-clear", "wr", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = state

	if _, err := sup.Detach("detach-clear", "wr"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	info, err := sup.Get("detach-clear")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.ActiveWriterClientID != "" {
		t.Errorf("ActiveWriterClientID=%q want empty after detach", info.ActiveWriterClientID)
	}
}

// TestNotifyWriterClaimedFanout verifies that NotifyWriterClaimed broadcasts a
// ChunkTypeWriterClaimed control chunk to all attached observers and that the
// chunk is NOT appended to the replay buffer.
func TestNotifyWriterClaimedFanout(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "notify-claim")

	w, err := sup.Attach("notify-claim", "writer", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach writer: %v", err)
	}
	o1, err := sup.Attach("notify-claim", "obs-1", 0, AttachRoleObserver)
	if err != nil {
		t.Fatalf("Attach obs-1: %v", err)
	}
	o2, err := sup.Attach("notify-claim", "obs-2", 0, AttachRoleObserver)
	if err != nil {
		t.Fatalf("Attach obs-2: %v", err)
	}

	sup.NotifyWriterClaimed("notify-claim", "writer")

	// All three channels should receive the control event.
	for label, ch := range map[string]<-chan OutputChunk{"writer": w.Live, "obs-1": o1.Live, "obs-2": o2.Live} {
		select {
		case chunk := <-ch:
			if chunk.Type != ChunkTypeWriterClaimed {
				t.Errorf("%s: chunk.Type=%v want ChunkTypeWriterClaimed", label, chunk.Type)
			}
			if string(chunk.Payload) != "writer" {
				t.Errorf("%s: payload=%q want %q", label, chunk.Payload, "writer")
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: timed out waiting for ChunkTypeWriterClaimed", label)
		}
	}

	// Control event must NOT appear in the replay buffer.
	reattach, err := sup.Attach("notify-claim", "replay-check", 0, AttachRoleObserver)
	if err != nil {
		t.Fatalf("Attach replay-check: %v", err)
	}
	for _, c := range reattach.Replay {
		if c.Type == ChunkTypeWriterClaimed || c.Type == ChunkTypeWriterReleased {
			t.Errorf("control chunk type=%v found in replay buffer; should not be persisted", c.Type)
		}
	}
}

// TestNotifyWriterReleasedFanout verifies that NotifyWriterReleased broadcasts
// a ChunkTypeWriterReleased control chunk to all observers without persisting
// it in the replay buffer.
func TestNotifyWriterReleasedFanout(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "notify-release")

	w, err := sup.Attach("notify-release", "wr", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach writer: %v", err)
	}
	obs, err := sup.Attach("notify-release", "obs", 0, AttachRoleObserver)
	if err != nil {
		t.Fatalf("Attach obs: %v", err)
	}

	sup.NotifyWriterReleased("notify-release", "wr")

	for label, ch := range map[string]<-chan OutputChunk{"wr": w.Live, "obs": obs.Live} {
		select {
		case chunk := <-ch:
			if chunk.Type != ChunkTypeWriterReleased {
				t.Errorf("%s: chunk.Type=%v want ChunkTypeWriterReleased", label, chunk.Type)
			}
			if string(chunk.Payload) != "wr" {
				t.Errorf("%s: payload=%q want %q", label, chunk.Payload, "wr")
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: timed out waiting for ChunkTypeWriterReleased", label)
		}
	}

	// Control event must NOT appear in the replay buffer.
	reattach, err := sup.Attach("notify-release", "replay-check", 0, AttachRoleObserver)
	if err != nil {
		t.Fatalf("Attach replay-check: %v", err)
	}
	for _, c := range reattach.Replay {
		if c.Type == ChunkTypeWriterClaimed || c.Type == ChunkTypeWriterReleased {
			t.Errorf("control chunk type=%v found in replay buffer; should not be persisted", c.Type)
		}
	}
}

// TestControlEventNotSentToUnknownSession verifies that NotifyWriterClaimed
// and NotifyWriterReleased are no-ops for sessions that do not exist.
func TestControlEventNotSentToUnknownSession(t *testing.T) {
	sup := newTestSupervisor(t)
	// Neither call should panic or return an error.
	sup.NotifyWriterClaimed("does-not-exist", "some-client")
	sup.NotifyWriterReleased("does-not-exist", "some-client")
}

// TestControlEventSeqIsZero verifies that control chunks carry Seq=0 (they are
// not sequenced output chunks and must not increment the ring-buffer sequence).
func TestControlEventSeqIsZero(t *testing.T) {
	sup := newTestSupervisor(t)
	startTestSession(t, sup, "control-seq")

	state, err := sup.Attach("control-seq", "client", 0, AttachRoleWriter)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	sup.NotifyWriterClaimed("control-seq", "client")

	select {
	case chunk := <-state.Live:
		if chunk.Seq != 0 {
			t.Errorf("control chunk Seq=%d want 0", chunk.Seq)
		}
		if chunk.Type != ChunkTypeWriterClaimed {
			t.Errorf("chunk.Type=%v want ChunkTypeWriterClaimed", chunk.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for control chunk")
	}
}

// TestPTYReadErrorTerminatesSession verifies that a non-EOF PTY read error
// causes the supervisor to force-stop the session. This exercises the fix for
// the issue where a dead PTY left the session alive so clients could flood
// WriteInput against it indefinitely.
func TestPTYReadErrorTerminatesSession(t *testing.T) {
	registry := NewRegistry()
	// Use a provider whose process exits immediately; the PTY will then return
	// an I/O error on the next Read, which is exactly the failure mode we want
	// to handle.
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Use a fast-exit command: "true" finishes instantly, closing the PTY.
	trueProv := &exitImmediateTestProvider{testProvider: testProvider{id: "exit-immediate"}}
	if err := registry.Register(trueProv); err != nil {
		t.Fatalf("Register exit-immediate: %v", err)
	}

	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)
	defer sup.Close()

	_, err := sup.Start(context.Background(), SessionConfig{
		ProjectID:   "proj-pty-err",
		SessionID:   "pty-err-1",
		RepoPath:    t.TempDir(),
		Options:     map[string]string{"provider": "exit-immediate"},
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// After the process exits the PTY read will error; the session should
	// transition to Stopped or Failed without manual intervention. The
	// force-stop path may also remove the session from the live map, so
	// ErrSessionNotFound is also a valid termination signal.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, getErr := sup.Get("pty-err-1")
		if errors.Is(getErr, ErrSessionNotFound) {
			return // session force-removed after PTY error
		}
		if getErr == nil && (info.State == SessionStateStopped || info.State == SessionStateFailed) {
			return // session cleaned up as expected
		}
		time.Sleep(50 * time.Millisecond)
	}
	info, getErr := sup.Get("pty-err-1")
	if errors.Is(getErr, ErrSessionNotFound) {
		return
	}
	var state SessionState
	if info != nil {
		state = info.State
	}
	t.Fatalf("session did not terminate after PTY closed: state=%v getErr=%v", state, getErr)
}

// exitImmediateTestProvider runs "true" (exits immediately) via PTY.
type exitImmediateTestProvider struct {
	testProvider
}

func (p *exitImmediateTestProvider) ID() string { return "exit-immediate" }
func (p *exitImmediateTestProvider) BuildCommand(ctx context.Context, cfg SessionConfig) (*exec.Cmd, error) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		truePath = "/usr/bin/true"
	}
	return exec.CommandContext(ctx, truePath), nil
}
