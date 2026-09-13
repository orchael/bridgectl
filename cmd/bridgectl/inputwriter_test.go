package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
)

// mockWriteClient records WriteInput calls for assertions.
type mockWriteClient struct {
	mu    sync.Mutex
	calls []*bridgev1.WriteInputRequest
}

func (m *mockWriteClient) WriteInput(_ context.Context, req *bridgev1.WriteInputRequest) (*bridgev1.WriteInputResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	return &bridgev1.WriteInputResponse{Accepted: true, BytesWritten: uint32(len(req.Data))}, nil
}

func (m *mockWriteClient) getCalls() []*bridgev1.WriteInputRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*bridgev1.WriteInputRequest, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *mockWriteClient) allData() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	var buf bytes.Buffer
	for _, c := range m.calls {
		buf.Write(c.Data)
	}
	return buf.Bytes()
}

// slowReader returns one byte at a time with a short delay, simulating raw
// terminal mode keystroke-by-keystroke reads.
type slowReader struct {
	data  []byte
	pos   int
	delay time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.delay > 0 && r.pos > 0 {
		time.Sleep(r.delay)
	}
	p[0] = r.data[r.pos]
	r.pos++
	if r.pos >= len(r.data) {
		return 1, io.EOF
	}
	return 1, nil
}

func TestInputWriterCoalescesKeystrokes(t *testing.T) {
	mock := &mockWriteClient{}
	input := "hello world"
	reader := &slowReader{data: []byte(input), delay: time.Millisecond}

	var readErr error
	errDone := make(chan struct{})
	w := &inputWriter{cfg: inputWriterConfig{
		Reader:        reader,
		Client:        mock,
		SessionID:     "sess-1",
		ClientID:      "client-1",
		FlushInterval: 50 * time.Millisecond,
		OnReadError: func(err error) {
			readErr = err
			close(errDone)
		},
	}}

	w.Run()
	<-errDone

	if readErr != io.EOF {
		t.Fatalf("expected io.EOF, got %v", readErr)
	}

	// All data should be received.
	if got := string(mock.allData()); got != input {
		t.Fatalf("data mismatch: got %q, want %q", got, input)
	}

	// Coalescing should produce fewer RPCs than individual bytes.
	calls := mock.getCalls()
	if len(calls) >= len(input) {
		t.Errorf("expected fewer RPCs than bytes (%d), got %d calls", len(input), len(calls))
	}

	// Verify session/client IDs on all calls.
	for i, c := range calls {
		if c.SessionId != "sess-1" {
			t.Errorf("call %d: session_id = %q, want %q", i, c.SessionId, "sess-1")
		}
		if c.ClientId != "client-1" {
			t.Errorf("call %d: client_id = %q, want %q", i, c.ClientId, "client-1")
		}
	}
}

func TestInputWriterDetachKey(t *testing.T) {
	mock := &mockWriteClient{}
	// Data with detach key (0x1d) in the middle: "abc" + detachKey + "def"
	input := []byte{'a', 'b', 'c', 0x1d, 'd', 'e', 'f'}
	reader := bytes.NewReader(input)

	detached := make(chan struct{})
	w := &inputWriter{cfg: inputWriterConfig{
		Reader:        reader,
		Client:        mock,
		SessionID:     "sess-1",
		ClientID:      "client-1",
		DetachKey:     0x1d,
		FlushInterval: 5 * time.Millisecond,
		OnDetach: func() {
			close(detached)
		},
	}}

	w.Run()

	select {
	case <-detached:
	case <-time.After(time.Second):
		t.Fatal("OnDetach was not called")
	}

	// Only bytes before the detach key should be sent.
	got := string(mock.allData())
	if got != "abc" {
		t.Errorf("expected data before detach key %q, got %q", "abc", got)
	}
}

func TestInputWriterDetachKeyAtStart(t *testing.T) {
	mock := &mockWriteClient{}
	input := []byte{0x1d, 'a', 'b', 'c'}
	reader := bytes.NewReader(input)

	detached := make(chan struct{})
	w := &inputWriter{cfg: inputWriterConfig{
		Reader:        reader,
		Client:        mock,
		SessionID:     "sess-1",
		ClientID:      "client-1",
		DetachKey:     0x1d,
		FlushInterval: 5 * time.Millisecond,
		OnDetach: func() {
			close(detached)
		},
	}}

	w.Run()

	select {
	case <-detached:
	case <-time.After(time.Second):
		t.Fatal("OnDetach was not called")
	}

	// No data should be sent when detach key is at position 0.
	if got := mock.allData(); len(got) != 0 {
		t.Errorf("expected no data, got %q", got)
	}
}

func TestInputWriterFlushesOnMaxBatch(t *testing.T) {
	mock := &mockWriteClient{}

	// Use an io.Pipe so we control when data arrives and when EOF is sent.
	// Write more than maxBatchSize, wait for the size-triggered flush, then
	// write a second chunk and close.
	pr, pw := io.Pipe()

	done := make(chan struct{})
	w := &inputWriter{cfg: inputWriterConfig{
		Reader:        pr,
		Client:        mock,
		SessionID:     "sess-1",
		ClientID:      "client-1",
		FlushInterval: time.Second, // long interval — flush should happen via size threshold only
		OnReadError: func(_ error) {
			close(done)
		},
	}}

	go w.Run()

	// Write more than maxBatchSize in one go.
	first := strings.Repeat("A", maxBatchSize+100)
	_, _ = pw.Write([]byte(first))

	// Wait for the size-triggered flush to happen.
	deadline := time.After(2 * time.Second)
	for len(mock.getCalls()) < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for size-triggered flush")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Write a second chunk and close.
	second := "tail"
	_, _ = pw.Write([]byte(second))
	_ = pw.Close()
	<-done

	want := first + second
	if got := string(mock.allData()); got != want {
		t.Fatalf("data mismatch: got %d bytes, want %d", len(got), len(want))
	}

	calls := mock.getCalls()
	if len(calls) < 2 {
		t.Errorf("expected at least 2 calls (size-triggered + final), got %d", len(calls))
	}
}

func TestInputWriterNoDetachKeyNoCallback(t *testing.T) {
	mock := &mockWriteClient{}
	input := "hello"
	reader := strings.NewReader(input)

	done := make(chan struct{})
	w := &inputWriter{cfg: inputWriterConfig{
		Reader:        reader,
		Client:        mock,
		SessionID:     "sess-1",
		ClientID:      "client-1",
		FlushInterval: 5 * time.Millisecond,
		OnReadError: func(_ error) {
			close(done)
		},
	}}

	w.Run()
	<-done

	if got := string(mock.allData()); got != input {
		t.Fatalf("data mismatch: got %q, want %q", got, input)
	}
}

func TestInputWriterOnReadErrorNil(t *testing.T) {
	// Verify that a nil OnReadError doesn't panic.
	mock := &mockWriteClient{}
	reader := strings.NewReader("hi")

	w := &inputWriter{cfg: inputWriterConfig{
		Reader:        reader,
		Client:        mock,
		SessionID:     "sess-1",
		ClientID:      "client-1",
		FlushInterval: 5 * time.Millisecond,
	}}

	// Should not panic. Run() is synchronous and flushes before returning, so
	// data is guaranteed to be in mock once Run() returns.
	w.Run()

	if got := string(mock.allData()); got != "hi" {
		t.Fatalf("data mismatch: got %q, want %q", got, "hi")
	}
}
