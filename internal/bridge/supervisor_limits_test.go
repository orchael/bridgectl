package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestWriteInputOversizedPayload verifies that WriteInput rejects payloads
// exceeding policy.MaxInputBytes and returns ErrInputTooLarge.
// Issue #153: input size validation.
func TestWriteInputOversizedPayload(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	maxInputBytes := 64 // very small for testing
	policy := Policy{
		MaxPerProject: 10,
		MaxGlobal:     20,
		MaxInputBytes: maxInputBytes,
	}
	supervisor := NewSupervisor(registry, policy, 1024, time.Minute)
	defer supervisor.Close()

	repo := t.TempDir()
	if _, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID:   "project-a",
		SessionID:   "session-input",
		RepoPath:    repo,
		Options:     map[string]string{"provider": "fake"},
		InitialCols: 80,
		InitialRows: 24,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Attach as writer so WriteInput is allowed.
	if _, err := supervisor.Attach("session-input", "client-a", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	tests := []struct {
		name    string
		size    int
		wantErr error
	}{
		{
			name:    "under limit succeeds",
			size:    maxInputBytes - 1,
			wantErr: nil,
		},
		{
			name:    "at limit succeeds",
			size:    maxInputBytes,
			wantErr: nil,
		},
		{
			name:    "over limit returns ErrInputTooLarge",
			size:    maxInputBytes + 1,
			wantErr: ErrInputTooLarge,
		},
		{
			name:    "much larger returns ErrInputTooLarge",
			size:    maxInputBytes * 10,
			wantErr: ErrInputTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, tc.size)
			for i := range data {
				data[i] = 'x'
			}
			_, err := supervisor.WriteInput("session-input", "client-a", data)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("WriteInput: unexpected error: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("WriteInput error=%v want %v", err, tc.wantErr)
			}
		})
	}

	_ = supervisor.Stop("session-input", true)
}

// TestSessionLimitPerProjectEnforcement verifies that the supervisor refuses
// to start sessions beyond max_per_project.
// Issue #153: session limits.
func TestSessionLimitPerProjectEnforcement(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	policy := Policy{
		MaxPerProject: 2,
		MaxGlobal:     10,
		MaxInputBytes: 65536,
	}
	supervisor := NewSupervisor(registry, policy, 1024, time.Minute)
	defer supervisor.Close()

	repo := t.TempDir()

	// Start two sessions for project-a (at the limit).
	for i, sid := range []string{"limit-proj-1", "limit-proj-2"} {
		if _, err := supervisor.Start(context.Background(), SessionConfig{
			ProjectID: "project-a",
			SessionID: sid,
			RepoPath:  repo,
			Options:   map[string]string{"provider": "fake"},
		}); err != nil {
			t.Fatalf("Start session %d: %v", i+1, err)
		}
	}

	// Third session for the same project should fail.
	_, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-a",
		SessionID: "limit-proj-3",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "fake"},
	})
	if !errors.Is(err, ErrSessionLimitReached) {
		t.Fatalf("expected ErrSessionLimitReached, got: %v", err)
	}

	// A different project should still be able to start a session.
	if _, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-b",
		SessionID: "limit-proj-b-1",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "fake"},
	}); err != nil {
		t.Fatalf("Start for project-b: %v", err)
	}

	// Clean up.
	for _, sid := range []string{"limit-proj-1", "limit-proj-2", "limit-proj-b-1"} {
		_ = supervisor.Stop(sid, true)
	}
}

// TestSessionLimitGlobalEnforcement verifies that the supervisor refuses to
// start sessions beyond max_global.
// Issue #153: session limits.
func TestSessionLimitGlobalEnforcement(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	policy := Policy{
		MaxPerProject: 10,
		MaxGlobal:     3,
		MaxInputBytes: 65536,
	}
	supervisor := NewSupervisor(registry, policy, 1024, time.Minute)
	defer supervisor.Close()

	repo := t.TempDir()

	// Start three sessions across different projects.
	for i, id := range []string{"limit-g-1", "limit-g-2", "limit-g-3"} {
		if _, err := supervisor.Start(context.Background(), SessionConfig{
			ProjectID: "project-" + id,
			SessionID: id,
			RepoPath:  repo,
			Options:   map[string]string{"provider": "fake"},
		}); err != nil {
			t.Fatalf("Start session %d: %v", i+1, err)
		}
	}

	// Fourth session (any project) should fail.
	_, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID: "project-new",
		SessionID: "limit-g-4",
		RepoPath:  repo,
		Options:   map[string]string{"provider": "fake"},
	})
	if !errors.Is(err, ErrSessionLimitReached) {
		t.Fatalf("expected ErrSessionLimitReached (global), got: %v", err)
	}

	// Clean up.
	for _, sid := range []string{"limit-g-1", "limit-g-2", "limit-g-3"} {
		_ = supervisor.Stop(sid, true)
	}
}

// TestListSessionsFiltersByProject verifies List returns only sessions
// matching the requested project_id.
// Issue #153: ListSessions.
func TestListSessionsFiltersByProject(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	supervisor := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	repo := t.TempDir()

	// Start sessions for two projects.
	for _, cfg := range []struct {
		projectID, sessionID string
	}{
		{"proj-x", "list-x-1"},
		{"proj-x", "list-x-2"},
		{"proj-y", "list-y-1"},
	} {
		if _, err := supervisor.Start(context.Background(), SessionConfig{
			ProjectID: cfg.projectID,
			SessionID: cfg.sessionID,
			RepoPath:  repo,
			Options:   map[string]string{"provider": "fake"},
		}); err != nil {
			t.Fatalf("Start %s: %v", cfg.sessionID, err)
		}
	}

	// List for proj-x: should return 2.
	listX := supervisor.List("proj-x")
	if len(listX) != 2 {
		t.Fatalf("List(proj-x) len=%d want 2", len(listX))
	}

	// List for proj-y: should return 1.
	listY := supervisor.List("proj-y")
	if len(listY) != 1 {
		t.Fatalf("List(proj-y) len=%d want 1", len(listY))
	}

	// List for all (empty project_id): should return 3.
	listAll := supervisor.List("")
	if len(listAll) != 3 {
		t.Fatalf("List('') len=%d want 3", len(listAll))
	}

	// Clean up.
	for _, sid := range []string{"list-x-1", "list-x-2", "list-y-1"} {
		_ = supervisor.Stop(sid, true)
	}
}

// TestResizeSessionUpdatesSize verifies that Resize updates cols/rows.
// Issue #153: ResizeSession.
func TestResizeSessionUpdatesSize(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	supervisor := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	if _, err := supervisor.Start(context.Background(), SessionConfig{
		ProjectID:   "project-a",
		SessionID:   "resize-session",
		RepoPath:    t.TempDir(),
		Options:     map[string]string{"provider": "fake"},
		InitialCols: 80,
		InitialRows: 24,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := supervisor.Attach("resize-session", "client-a", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if err := supervisor.Resize("resize-session", "client-a", 200, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	info, err := supervisor.Get("resize-session")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Cols != 200 || info.Rows != 50 {
		t.Fatalf("size=%dx%d want 200x50", info.Cols, info.Rows)
	}

	_ = supervisor.Stop("resize-session", true)
}

func TestConcurrentResizeKeepsPTYAndReplayGeometryAligned(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&testProvider{id: "fake"}); err != nil {
		t.Fatal(err)
	}
	sup := NewSupervisor(registry, DefaultPolicy(), 1024, time.Minute)
	defer sup.Close()
	const sessionID = "concurrent-resize"
	if _, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "project-a", SessionID: sessionID, RepoPath: t.TempDir(),
		Options: map[string]string{"provider": "fake"}, InitialCols: 80, InitialRows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Attach(sessionID, "writer", 0, AttachRoleWriter); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if err := sup.Resize(sessionID, "writer", uint32(80+worker*20+i), uint32(24+worker*20+i)); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	info, err := sup.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sup.mu.RLock()
	ms := sup.sessions[sessionID]
	sup.mu.RUnlock()
	ms.mu.Lock()
	rows, cols, err := pty.Getsize(ms.ptmx)
	chunks := ms.buf.After(0)
	ms.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("missing resize frames")
	}
	last := chunks[len(chunks)-1]
	if uint32(cols) != info.Cols || uint32(rows) != info.Rows || last.Cols != info.Cols || last.Rows != info.Rows {
		t.Fatalf("PTY=%dx%d metadata=%dx%d last frame=%dx%d", cols, rows, info.Cols, info.Rows, last.Cols, last.Rows)
	}
}
