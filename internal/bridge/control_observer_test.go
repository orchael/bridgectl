package bridge

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

type controlSpy struct {
	mu        sync.Mutex
	changes   []SessionInfo
	closeErr  error
	closed    bool
	closeSeen chan struct{}
}

func newControlSpy() *controlSpy {
	return &controlSpy{closeSeen: make(chan struct{})}
}

func (s *controlSpy) SessionChanged(info SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changes = append(s.changes, info)
}

func (s *controlSpy) Close(context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	close(s.closeSeen)
	return s.closeErr
}

func (s *controlSpy) states() []SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionState, len(s.changes))
	for i, c := range s.changes {
		out[i] = c.State
	}
	return out
}

func TestControlObserver_NotifiedOnSessionEndAfterReaderDrains(t *testing.T) {
	spy := newControlSpy()
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithControlObserver(spy))
	defer sup.Close()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ms := &managedSession{
		info:       SessionInfo{SessionID: "s1", Provider: "test", State: SessionStateRunning},
		cmd:        cmd,
		buf:        NewByteBuffer(1024),
		cancel:     func() {},
		readerDone: make(chan struct{}),
	}
	close(ms.readerDone)
	sup.waitLoop(ms)

	states := spy.states()
	if len(states) != 1 || (states[0] != SessionStateStopped && states[0] != SessionStateFailed) {
		t.Fatalf("control observer states=%v, want exactly one terminal state", states)
	}
}

func TestControlObserver_NotifiedOnAttachAndDetach(t *testing.T) {
	spy := newControlSpy()
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithControlObserver(spy))
	ms := &managedSession{
		info:      SessionInfo{SessionID: "s1", Provider: "test", State: SessionStateRunning},
		buf:       NewByteBuffer(1024),
		observers: make(map[string]*observerEntry),
	}
	sup.mu.Lock()
	sup.sessions["s1"] = ms
	sup.mu.Unlock()

	if _, err := sup.Attach("s1", "writer-1", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := sup.Detach("s1", "writer-1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	states := spy.states()
	if len(states) != 2 || states[0] != SessionStateAttached || states[1] != SessionStateRunning {
		t.Fatalf("control observer states=%v, want [Attached Running]", states)
	}
}

func TestControlObserver_NotNotifiedOnObserverAttach(t *testing.T) {
	// Read-only observer attaches don't change lifecycle state, so they must
	// not spuriously notify control (only a writer attach does).
	spy := newControlSpy()
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithControlObserver(spy))
	ms := &managedSession{
		info:      SessionInfo{SessionID: "s1", Provider: "test", State: SessionStateRunning},
		buf:       NewByteBuffer(1024),
		observers: make(map[string]*observerEntry),
	}
	sup.mu.Lock()
	sup.sessions["s1"] = ms
	sup.mu.Unlock()

	if _, err := sup.Attach("s1", "observer-1", 0, AttachRoleObserver); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if states := spy.states(); len(states) != 0 {
		t.Fatalf("control observer states=%v, want none for an observer-role attach", states)
	}
}

func TestControlObserver_CloseErrorDoesNotBreakShutdown(t *testing.T) {
	// A failing/unavailable Bridge control connection must never block or
	// fail local Supervisor shutdown.
	spy := newControlSpy()
	spy.closeErr = errors.New("bridge unreachable")
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithControlObserver(spy))

	done := make(chan struct{})
	go func() {
		sup.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervisor.Close did not return promptly despite a failing control observer")
	}

	select {
	case <-spy.closeSeen:
	default:
		t.Fatal("control observer Close was never called")
	}
}

func TestSupervisor_WorksWithoutControlObserverConfigured(t *testing.T) {
	// Baseline regression: standalone bridgectl (no Bridge enrollment) must
	// behave exactly as before when no ControlObserver is configured at all.
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute)
	ms := &managedSession{
		info:      SessionInfo{SessionID: "s1", Provider: "test", State: SessionStateRunning},
		buf:       NewByteBuffer(1024),
		observers: make(map[string]*observerEntry),
	}
	sup.mu.Lock()
	sup.sessions["s1"] = ms
	sup.mu.Unlock()
	if _, err := sup.Attach("s1", "writer-1", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach without a control observer: %v", err)
	}
}
